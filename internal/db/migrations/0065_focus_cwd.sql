-- muster 0065 — A SESSION'S WORKING DIRECTORY, AS ITS TRANSCRIPT LAST SAID.
-- docs/focus.md#the-working-directory.
--
--   agent_focus.cwd   the newest working directory the session's transcript
--       tail named when its focus was last written (Claude Code writes `cwd`
--       on every record; muster-mirror passes a prompt's along), cleaned and
--       absolute; '' = unknown. A write that names none keeps the one stored.
--       Read by the fleet report's mail door (internal/api/fleet_report.go) to
--       decide whether a session is under MUSTER_FLEET_REPORT_PATHS; never
--       rendered and never sent.
--
-- 🔴 ONE COLUMN, ON THE ROW THAT ALREADY OUTLIVES THE SESSION. A mirrored
-- session's agent_focus row is kept for the focus retention (7 days by
-- default) after it was last seen, longer than any report window (4 days), so
-- an ENDED session still has its directory. agent_focus is owned and under
-- FORCE row-level security (0020); a new column inherits both, so nothing here
-- touches a policy, grant or default and db.RLSOn re-applies nothing.
--
-- Must apply after 0064 (the contiguity test refuses a gap). Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   ALTER TABLE agent_focus DROP CONSTRAINT IF EXISTS agent_focus_cwd_check;
--   ALTER TABLE agent_focus DROP COLUMN IF EXISTS cwd;
--   DELETE FROM schema_migrations WHERE version = 65;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agent_focus ADD COLUMN IF NOT EXISTS cwd TEXT NOT NULL DEFAULT '';

-- Absolute or unknown, bounded: the same rule focus.CleanCwd applies in code.
ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_cwd_check;
ALTER TABLE public.agent_focus ADD CONSTRAINT agent_focus_cwd_check
    CHECK (cwd = '' OR (left(cwd, 1) = '/' AND char_length(cwd) <= 512));
