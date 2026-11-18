-- muster 0150 — A ROUTE RECORDS THE HOST OF THE SESSION IT WAS SENT TO
-- (internal/plugins routes.go Route.TargetHost; internal/api
-- route_reply_tool.go and host_stepup.go routeReplyToItsOwnSession).
--
--   session_routes.target_host   the mirror host whose window ran the target
--       session when the route was sent (resolveRouteTarget's host), NULL for
--       a route made before this migration. route_reply then answers a route
--       only from that host, and the step-up gate's route_reply carve-out
--       holds only for that host AND that session: a session id is the
--       caller's claim, a host is its token's.
--
-- A route made before this migration (target_host NULL) keeps the old binding
-- (the session alone) in route_reply and never gets the carve-out. Routes
-- time out within minutes, so these are gone soon after the upgrade.
--
-- Idempotent; not re-applied by db.RLSOn (no new table or policy: 0021's
-- policy and grants cover the column).
--
-- Must apply after its predecessor, 0149_mirror_token_spawned (the
-- contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   ALTER TABLE public.session_routes DROP COLUMN IF EXISTS target_host;
--   DELETE FROM schema_migrations WHERE version = 150;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS target_host TEXT
    CHECK (target_host IS NULL OR target_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$');
