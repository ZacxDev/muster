-- muster 0075 — WHY AN EPISODE IS QUIET (internal/sessionstate;
-- docs/design/session-state.md, "Who decides what" and "Push").
--
--   session_episodes.quiet   '' (the episode is, or will be, pushed) or why it
--       is NOT pushed now and is drawn as a quiet row on the Attention page:
--         'left'          the chief judged the stop and said leave it, and
--                         the session asked nothing (the classifier said so);
--         'nudged'        the chief's nudge to the session was SENT;
--         'owner_last'    the owner spoke last in the conversation (no
--                         classifier ran, so muster knows it from the
--                         messages alone): the session is not stopped on its
--                         own word;
--         'repush_floor'  HELD: the session's previous episode pushed under
--                         the repush floor (2 minutes) ago; the stop is pushed
--                         once the floor has passed, if it is still open.
--       Quiet is NOT final, for any reason: a pass that later finds a reason
--       to push (the floor passed, a new streak the chief says notify on, a
--       nudge whose send failed) pushes it and clears this. Only an episode's
--       FIRST push is ever sent.
--
-- 🔴 NO NEW CLAIM. Pushing an episode claims it on pushed_at (UPDATE … WHERE
-- pushed_at IS NULL): only the process whose UPDATE took the row sends the
-- push, so the waiting hook and the state pass (and two replicas) can never
-- both push one stop.
--
-- The partial index serves the restart rule's clear: the ended episodes whose
-- notification was shown and not yet closed.
--
-- Must apply after 0074 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the column inherits the
-- table's row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0075 never reads or writes the column and can
-- run against this schema unchanged. To remove it:
--   DROP INDEX IF EXISTS session_episodes_uncleared;
--   ALTER TABLE session_episodes DROP COLUMN IF EXISTS quiet;
--   DELETE FROM schema_migrations WHERE version = 75;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_episodes ADD COLUMN IF NOT EXISTS quiet TEXT NOT NULL DEFAULT ''
    CONSTRAINT session_episodes_quiet CHECK (quiet IN ('', 'left', 'nudged', 'owner_last', 'repush_floor'));

CREATE INDEX IF NOT EXISTS session_episodes_uncleared
    ON public.session_episodes (owner_id) WHERE ended_at IS NOT NULL AND pushed_at IS NOT NULL AND cleared_at IS NULL;
