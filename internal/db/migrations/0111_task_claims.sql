-- muster 0111 — A SESSION'S EXPLICIT CLAIM ON A TASK (docs/tasks.md#claims;
-- the predicate is internal/notes/claims.go, the writers internal/api/task_claims.go).
--
--   task_sessions.claimed_at   set when THIS session picked the task up through
--       muster's task tools (a status write to in_progress that carried the
--       session's id), NULL otherwise. Cleared when the task is set back to
--       open by a person, session or agent, reopened upstream, stored as
--       complete, or when the claiming session ended (internal/api
--       task_claims.go). A claim is a fact about a request, never inferred from
--       focus or transcript text.
--   task_sessions.claimed_by   the name the claim was made under (a mirrored
--       session's stable name, or "session <id prefix>"): what the board shows.
--   task_sessions.claim_host   the host the MIRROR listed the claiming session
--       on (a session's own claim, or an upstream claimant's linked session):
--       what "the session ended" is judged against. '' when no host listed it
--       — never a request's own host header (task_sessions.host), which names
--       a machine — and an empty claim_host is never judged ended.
--   task_sessions.upstream_released_rev   for a session an UPSTREAM claimant
--       names (a reference's snapshot claimants): "rev:<snapshot rev>" when
--       muster released that claim because the session ended, '' otherwise.
--       The snapshot at that rev no longer claims for it; a newer revision (a
--       new pickup, or any upstream change) is judged afresh.
--
--   plugin_message_handling.handler gains 'task_claimed': the task triage left
--       an assignment of a CLAIMED task alone (no "pick this up" card, no chief
--       turn).
--
-- The other half of a claim — the upstream's, for a plugin task reference — is
-- not stored here: it is read from the reference's snapshot, whose claimants
-- name the session that picked the record up. A status is NEVER a claim (an
-- upstream in progress names nobody), and a later snapshot without claimants
-- is the unclaim.
--
-- IF NOT EXISTS / DROP IF EXISTS: every migration is idempotent (the
-- skipped-migration recovery re-runs a file whose effect may already be there).
--
-- ROLLBACK (after rolling the image back; an older image never reads them):
--   ALTER TABLE task_sessions DROP COLUMN IF EXISTS claimed_at, DROP COLUMN IF EXISTS claimed_by,
--       DROP COLUMN IF EXISTS upstream_released_rev, DROP COLUMN IF EXISTS claim_host;
--   DELETE FROM plugin_message_handling WHERE handler = 'task_claimed';
--   (then restore 0107's handler CHECK)
--   DELETE FROM schema_migrations WHERE version = 111;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.task_sessions ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;
ALTER TABLE public.task_sessions ADD COLUMN IF NOT EXISTS claimed_by TEXT NOT NULL DEFAULT ''
    CHECK (char_length(claimed_by) <= 200);
ALTER TABLE public.task_sessions ADD COLUMN IF NOT EXISTS claim_host TEXT NOT NULL DEFAULT ''
    CHECK (char_length(claim_host) <= 200);
ALTER TABLE public.task_sessions ADD COLUMN IF NOT EXISTS upstream_released_rev TEXT NOT NULL DEFAULT ''
    CHECK (char_length(upstream_released_rev) <= 120);

ALTER TABLE public.plugin_message_handling DROP CONSTRAINT IF EXISTS plugin_message_handling_handler_check;
ALTER TABLE public.plugin_message_handling ADD CONSTRAINT plugin_message_handling_handler_check
    CHECK (handler IN ('fleet_report', 'thread', 'closed_pr', 'pr_route', 'mail_resume', 'task_event_mention',
                       'task_forward', 'task_route', 'task_board', 'task_claimed', 'rule', 'chief'));
