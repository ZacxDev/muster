-- muster 0087 — A ONE-WAY ROUTE (internal/plugins/routes.go): the chief's
-- route_to_session can hand a plugin message its plugin marked no_reply (a
-- feedback relay, a notification) to the session already on what it is about,
-- as a HEADS-UP. The session gets the message, fenced and labelled "no reply
-- possible"; its answer goes back to the chief only, never to the sender.
-- docs/plugins.md#a-one-way-heads-up-for-a-message-that-cannot-be-answered.
--
--   session_routes.one_way   true on a route that carries a no_reply plugin
--       message: its answer is owed to the chief (like a request route's,
--       0084), never relayed as a reply draft. false on every other route.
--
-- 🔴 ONLY A MESSAGE ROUTE CAN BE ONE-WAY (session_routes_one_way_check): a
-- task or request route already answers to the task or the chief.
-- 🔴 A ONE-WAY ROUTE NEVER BECOMES A DRAFT (session_routes_one_way_no_draft):
-- the store's relay refuses a no_reply message anyway (RelayRoute, and 0016's
-- refusal of a draft for one); this makes the route's own row say so too.
--
-- NOT NULL DEFAULT false: no rewrite, and every existing route reads two-way,
-- which every one of them is (route_to_session refused a no_reply message
-- before this). Must apply after 0086 (the contiguity test refuses a gap). Not
-- re-applied by db.RLSOn (it changes no policy, grant or default); the column
-- inherits the table's row-level security. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image would relay a one-way
-- route's answer as a reply draft, which the store refuses, failing the route):
--   UPDATE session_routes SET state = 'failed', finished_at = now(), error = 'rolled back', handback = NULL
--       WHERE one_way AND state IN ('routed', 'answered');
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_one_way_no_draft;
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_one_way_check;
--   ALTER TABLE session_routes DROP COLUMN IF EXISTS one_way;
--   DELETE FROM schema_migrations WHERE version = 87;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS one_way BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_one_way_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_one_way_check
    CHECK (NOT one_way OR plugin_message_id IS NOT NULL);

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_one_way_no_draft;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_one_way_no_draft
    CHECK (NOT one_way OR draft_id IS NULL);
