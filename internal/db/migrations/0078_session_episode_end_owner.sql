-- muster 0078 — AN EPISODE ENDS WHEN ITS OWNER ACTS (internal/sessionstate;
-- docs/design/session-state.md, "Episodes").
--
--   session_episodes.end_reason   gains 'owner': the owner acted in the
--       session after the episode opened — a message of their own written
--       after it opened, or, for a waiting episode, the prompt it waited on
--       answered (sessionstate.OwnerActed). The episode ends on the next state
--       pass, and its push is withdrawn with it, without waiting for EndAfter
--       of sustained work ('worked'), which stays the rule for a session that
--       carries on by itself.
--
-- The constraint is the column's own CHECK from 0073, named by Postgres
-- (session_episodes_end_reason_check); it is dropped and re-added wider.
-- Every existing row ('', 'worked', 'gone') passes the new check.
--
-- Must apply after 0077 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default). Idempotent.
--
-- ROLLBACK. An image from before 0078 never writes 'owner'; it reads an
-- 'owner' row as an ended episode like any other. To narrow the check again
-- (after rewriting any 'owner' row, e.g. to 'worked'):
--   UPDATE session_episodes SET end_reason = 'worked' WHERE end_reason = 'owner';
--   ALTER TABLE session_episodes DROP CONSTRAINT IF EXISTS session_episodes_end_reason_check;
--   ALTER TABLE session_episodes ADD CONSTRAINT session_episodes_end_reason_check
--       CHECK (end_reason IN ('', 'worked', 'gone'));
--   DELETE FROM schema_migrations WHERE version = 78;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_episodes DROP CONSTRAINT IF EXISTS session_episodes_end_reason_check;
ALTER TABLE public.session_episodes ADD CONSTRAINT session_episodes_end_reason_check
    CHECK (end_reason IN ('', 'worked', 'gone', 'owner'));
