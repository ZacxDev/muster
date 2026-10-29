-- muster 0062 — A ROUTE THE OWNER ANSWERED THEMSELVES, AND A MESSAGE FROM A
-- PERSON IN A CONVERSATION. docs/plugins.md#when-you-answer-it-yourself.
--
--   session_routes.state 'owner_answered'   the owner replied on the message's
--       upstream conversation directly, not through muster: the plugin said so
--       (POST /plugin/v1/owner-replies). Terminal, like relayed.
--   session_routes.owner_replied_at          when the owner replied, as the
--       plugin reported it; set exactly on an owner_answered route.
--   plugin_messages.human_conversation       the plugin's mark that the sender is
--       a person in a direct conversation: a route of it waits
--       ConversationRouteTimeout (4h) rather than RouteTimeout (30m).
--
-- Must apply after 0061 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); 0021's body, which RLSOn
-- may re-apply, creates session_routes only IF NOT EXISTS, so a re-apply leaves
-- the widened check in place. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image cannot read the state):
--   UPDATE session_routes SET state = 'timed_out', error = 'answered by the owner directly'
--    WHERE state = 'owner_answered';
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_owner_replied_check;
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_state_check;
--   ALTER TABLE session_routes ADD CONSTRAINT session_routes_state_check
--     CHECK (state IN ('routed', 'answered', 'relayed', 'timed_out', 'fallback', 'failed'));
--   ALTER TABLE session_routes DROP COLUMN IF EXISTS owner_replied_at;
--   ALTER TABLE plugin_messages DROP COLUMN IF EXISTS human_conversation;
--   DELETE FROM schema_migrations WHERE version = 62;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS owner_replied_at TIMESTAMPTZ;

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_state_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_state_check
    CHECK (state IN ('routed', 'answered', 'relayed', 'timed_out', 'fallback', 'failed', 'owner_answered'));

-- 🔴 THE STATE AND ITS TIME TOGETHER: an owner_answered route always says when
-- the owner replied, and no other route claims a reply it never had.
ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_owner_replied_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_owner_replied_check
    CHECK ((state = 'owner_answered') = (owner_replied_at IS NOT NULL));

ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS human_conversation BOOLEAN NOT NULL DEFAULT false;
