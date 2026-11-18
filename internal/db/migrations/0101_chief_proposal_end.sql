-- muster 0101 — ENDING A SESSION ON THE OWNER'S TAP (docs/design/
-- chief-in-agent-view.md, "End-session"): chief_proposals (0100) of kind
-- 'end_session' gain what they need.
--
--   subject_agent   the dispatched pod agent the proposal would end (its
--                   agents.id), NULL for a mirrored window — whose subject_*
--                   name it already.
--   end_state       '' until the owner tapped; 'ending' from the tap until
--                   the end settled; then 'ended' or 'failed'.
--   end_detail      how it ended ("the agent exited", "closed after the
--                   grace", "stopped") or why it failed, one line.
--   ended_at        when it settled.
--
-- 🔴 WRITERS ARE A LEDGER (with 0100's): StartEnd (decided = accepted and
-- end_state = ending, on an undecided end_session row) and RecordEnd (ended or
-- failed, on an 'ending' row), each with its one caller.
--
-- New columns on an owned table: the table's row-level security covers them;
-- not re-applied by db.RLSOn. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads them):
--   ALTER TABLE public.chief_proposals DROP CONSTRAINT IF EXISTS chief_proposals_end_only;
--   ALTER TABLE public.chief_proposals DROP COLUMN IF EXISTS subject_agent, DROP COLUMN IF EXISTS end_state,
--     DROP COLUMN IF EXISTS end_detail, DROP COLUMN IF EXISTS ended_at;
--   DELETE FROM schema_migrations WHERE version = 101;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chief_proposals ADD COLUMN IF NOT EXISTS subject_agent BIGINT;
ALTER TABLE public.chief_proposals ADD COLUMN IF NOT EXISTS end_state TEXT NOT NULL DEFAULT ''
    CHECK (end_state IN ('', 'ending', 'ended', 'failed'));
ALTER TABLE public.chief_proposals ADD COLUMN IF NOT EXISTS end_detail TEXT NOT NULL DEFAULT ''
    CHECK (char_length(end_detail) <= 500);
ALTER TABLE public.chief_proposals ADD COLUMN IF NOT EXISTS ended_at TIMESTAMPTZ;

ALTER TABLE public.chief_proposals DROP CONSTRAINT IF EXISTS chief_proposals_end_only;
ALTER TABLE public.chief_proposals ADD CONSTRAINT chief_proposals_end_only CHECK (kind = 'end_session' OR
    (subject_agent IS NULL AND end_state = '' AND end_detail = '' AND ended_at IS NULL));
