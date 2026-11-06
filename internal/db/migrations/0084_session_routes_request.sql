-- muster 0084 — A ROUTE CAN CARRY THE OWNER'S OWN REQUEST (internal/plugins/routes.go):
-- the chief's route_to_session {text, subject, owner_asked: true} hands a
-- mirrored session an idea or request the owner TYPED in the chief's chat, as
-- the chief relays it. Everything after the route is the message route's (the
-- paste, route_reply, the deadline, the hand-back, the audit); the answer goes
-- back to the chief's chat session where the owner asked.
-- docs/plugins.md#routing-your-own-request-to-a-session.
--
--   session_routes.request_text        what is pasted: the chief's relay of the
--       owner's request. NULL on every message and task route.
--   session_routes.request_message_id  the owner's own chat_messages row whose
--       turn passed the owner_asked check (internal/api ownerTurnFor): the
--       provenance, for the audit trail and the paste.
--   session_routes.chief_session_id    the chief chat session that row is in:
--       where the answer, and any hand-back, is delivered.
--
-- 🔴 EXACTLY ONE OF plugin_message_id, task_id, request_text
-- (session_routes_one_item_check, replacing 0053's two-way check), and a
-- request route carries both its provenance columns (session_routes_request_check).
-- 🔴 AT MOST ONE OPEN ROUTE PER OWNER MESSAGE AND TARGET
-- (session_routes_one_open_request): a chief retrying the same call routes once.
--
-- Neither id is a foreign key, as 0081's typed_by is not: a chat row is its
-- owner's under row-level security, and the route outlives a deleted session as
-- an audit record (the route's chief_session_id then names nothing, and the
-- answer is not delivered: routeChiefSession checks it).
--
-- Must apply after 0083 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the columns inherit the
-- table's row-level security. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image cannot read a request route):
--   DELETE FROM session_routes WHERE request_text IS NOT NULL;
--   DROP INDEX IF EXISTS session_routes_one_open_request;
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_request_check;
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_request_text_cap;
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_one_item_check;
--   ALTER TABLE session_routes ADD CONSTRAINT session_routes_one_item_check
--       CHECK ((plugin_message_id IS NULL) <> (task_id IS NULL));
--   ALTER TABLE session_routes DROP COLUMN IF EXISTS request_text,
--       DROP COLUMN IF EXISTS request_message_id, DROP COLUMN IF EXISTS chief_session_id;
--   DELETE FROM schema_migrations WHERE version = 84;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS request_text TEXT;
ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS request_message_id BIGINT;
ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS chief_session_id BIGINT;

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_one_item_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_one_item_check
    CHECK ((plugin_message_id IS NOT NULL)::int + (task_id IS NOT NULL)::int + (request_text IS NOT NULL)::int = 1);

-- A request route names the owner's message and the chief session; no other route does.
ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_request_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_request_check
    CHECK ((request_text IS NOT NULL) = (request_message_id IS NOT NULL)
       AND (request_text IS NOT NULL) = (chief_session_id IS NOT NULL));

-- api.maxRequestBytes (8 KiB, the chief's prompt cap) with room; never empty.
ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_request_text_cap;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_request_text_cap
    CHECK (request_text IS NULL OR octet_length(request_text) BETWEEN 1 AND 16384);

CREATE UNIQUE INDEX IF NOT EXISTS session_routes_one_open_request
    ON public.session_routes (request_message_id, target_kind, target_key)
    WHERE request_message_id IS NOT NULL AND state IN ('routed', 'answered');
