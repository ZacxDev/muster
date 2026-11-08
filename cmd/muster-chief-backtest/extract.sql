-- The chief backtest's input set, READ ONLY (run by extract.sh with
-- default_transaction_read_only=on). One JSON object per line: a chief input (a
-- plugin message delivered to the chief, or an owner-typed chief turn), and what
-- the Claude-backed chief did in response: the tool calls its transcript
-- recorded between this turn and the next user turn, and the side effects the
-- database holds for the message (session_routes, the task it was filed as, its
-- settle reason, reply drafts, attention asks).
--
-- :chief is the chief agent's id, :decide and :auto how many of the most
-- recent decision-bearing and muster-auto-routed plugin deliveries to take, and
-- :owner_ids the owner-typed turns (chat_messages ids) to add.
with turns as (
  select c.id, c.content, c.typed_by, c.created_at,
    nullif(substring(c.content from 'message_id ([0-9]+)'), '')::bigint as mid,
    c.content like '%routed by muster to the session%' as auto,
    (select min(n.id) from chat_messages n where n.agent_id = c.agent_id and n.role = 'user' and n.id > c.id) as next_id
  from chat_messages c
  where c.agent_id = :chief and c.role = 'user' and c.session_id = (
    select id from chat_sessions where agent_id = :chief and is_default)
),
picked as (
  (select * from turns where content like 'Plugin %' and not auto order by id desc limit :decide)
  union all
  (select * from turns where content like 'Plugin %' and auto order by id desc limit :auto)
  union all
  (select * from turns where typed_by is not null and id = any(string_to_array(:'owner_ids', ',')::bigint[]))
)
select json_build_object(
  'turn_id', p.id,
  'message_id', p.mid,
  'auto_routed', p.auto,
  'owner_typed', p.typed_by is not null,
  'at', p.created_at,
  'input', p.content,
  'claude_events', coalesce((
    select json_agg(json_build_object('id', m.id, 'kind', m.kind, 'tool', m.tool_name, 'ok', m.tool_ok, 'content', m.content) order by m.id)
    from chat_messages m
    where m.agent_id = :chief and m.role = 'assistant' and m.id > p.id and (p.next_id is null or m.id < p.next_id)), '[]'::json),
  'db', json_build_object(
    'message', (select json_build_object('status', pm.status, 'task_id', pm.task_id, 'settle_reason', pm.settle_reason,
                  'routed_to', pm.routed_to, 'no_reply', pm.no_reply, 'sender_verified', pm.sender_verified,
                  'plugin_id', pm.plugin_id)
                from plugin_messages pm where pm.id = p.mid),
    'routes', coalesce((select json_agg(json_build_object('target', sr.target_kind || ':' || sr.target_key, 'state', sr.state,
                  'agent_id', sr.agent_id, 'created_at', sr.created_at) order by sr.id)
                from session_routes sr where sr.plugin_message_id = p.mid), '[]'::json),
    'drafts', coalesce((select json_agg(json_build_object('agent', d.agent_name, 'route_id', d.route_id, 'status', d.status) order by d.id)
                from plugin_reply_drafts d where d.message_id = p.mid), '[]'::json),
    'asks', coalesce((select json_agg(json_build_object('id', a.id, 'agent_id', a.agent_id, 'system', a.system_asker, 'kind', a.kind) order by a.id)
                from attention_asks a where a.plugin_message_id = p.mid), '[]'::json)
  )
)
from picked p order by p.id;
