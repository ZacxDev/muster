-- muster 0098 — A HOST DISPATCH'S OUTCOME BEYOND DONE (docs/design/chief-dispatch.md
-- 3.7, "An opencode dispatch's outcome" and "A later turn"; the store is
-- internal/plugins/host_dispatches.go, the reconciler
-- internal/api/host_dispatch_outcome.go).
--
--   host_dispatches.waiting_at     when an opencode dispatch's turn ended idle
--       WITHOUT the done line (its owner's move); NULL otherwise.
--   host_dispatches.waiting_line   the last line of that reply.
--   host_dispatches.outcome_mark   the host's name for the reply an opencode
--       outcome was read from (its opencode message id), so one reply is
--       recorded, pushed and noted once; '' when none.
--   host_dispatches.done_settled   a Claude dispatch reported done has since
--       been listed IDLE: its reporting turn is over, so a later busy listing
--       is a new turn.
--
-- 🔴 DONE AND WAITING ARE NOT ENDED (0097's rule). status keeps deciding the
-- live slot; a later turn clears done/waiting back to working.
--
-- 🔴 THREE NEW WRITERS (asserted over the Go source with 0093's four and
-- 0097's one): RecordHostDispatchOutcome, RearmHostDispatch and
-- SettleHostDispatchDone, each only on a running row whose host and
-- agent_session are the caller's.
--
-- Must apply after 0097 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the columns inherit the
-- table's row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0098 never reads or writes these columns and
-- runs against this schema unchanged. To remove them:
--   ALTER TABLE public.host_dispatches DROP COLUMN IF EXISTS done_settled;
--   ALTER TABLE public.host_dispatches DROP COLUMN IF EXISTS outcome_mark;
--   ALTER TABLE public.host_dispatches DROP COLUMN IF EXISTS waiting_line;
--   ALTER TABLE public.host_dispatches DROP COLUMN IF EXISTS waiting_at;
--   DELETE FROM schema_migrations WHERE version = 98;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.host_dispatches ADD COLUMN IF NOT EXISTS waiting_at TIMESTAMPTZ;
ALTER TABLE public.host_dispatches ADD COLUMN IF NOT EXISTS waiting_line TEXT NOT NULL DEFAULT ''
    CHECK (char_length(waiting_line) <= 300);
ALTER TABLE public.host_dispatches ADD COLUMN IF NOT EXISTS outcome_mark TEXT NOT NULL DEFAULT ''
    CHECK (char_length(outcome_mark) <= 128);
ALTER TABLE public.host_dispatches ADD COLUMN IF NOT EXISTS done_settled BOOLEAN NOT NULL DEFAULT false;
