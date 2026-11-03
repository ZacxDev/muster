-- muster 0070 — A PULL-REQUEST REVIEW ROUTE KNOWS ITS ROUND.
-- docs/plugins.md#a-pull-request-review-mail.
--
--   session_routes.pr_head_sha   the head commit a review mail named (its
--       machine block's head_sha, prroute.ParseMail), set on a route muster
--       made for a pull-request review (plugins.RouteViaPR) and on no other.
--       NULL on every other route, and on a review route made before 0070.
--
-- 🔴 WHAT READS IT:
--   - the supersede (plugins.PGStore.OlderOpenPRRoutes): a review ROUND is one
--     head commit, and a round may arrive as several messages (part 1/2, 2/2).
--     Parts of one round never supersede each other; a route for a newer head
--     supersedes every open route of an OLDER head for the same pull request and
--     session. Before 0070 the supersede keyed on the pull request and session
--     alone, so part 2 of a round superseded part 1 of the same round.
--   - the time-out (plugins.PGStore.TimeOutRoute): a review route whose paste
--     WAS delivered and then got no route_reply by its deadline is handled — the
--     session has its own pull request's review — so it owes the chief no
--     hand-back and its message is settled by muster (no unrouted ask).
--     Non-NULL is what marks a route as a review route there; a route before
--     0070 keeps the old time-out (hand-back), which ends within 4 hours.
--
-- Must apply after 0069 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the column inherits the
-- table's row-level security. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_pr_head_sha_check;
--   ALTER TABLE session_routes DROP COLUMN IF EXISTS pr_head_sha;
--   DELETE FROM schema_migrations WHERE version = 70;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS pr_head_sha TEXT;

-- The shape prroute.ParseMail accepts: 40 lower-case hex, or nothing.
ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_pr_head_sha_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_pr_head_sha_check
    CHECK (pr_head_sha IS NULL OR pr_head_sha ~ '^[0-9a-f]{40}$');
