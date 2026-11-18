-- muster 0097 — A HOST-SPAWNED AGENT SAYS IT FINISHED (docs/design/chief-dispatch.md
-- 3.7, "Done is not ended"; the store is internal/plugins/host_dispatches.go,
-- the tool internal/api/host_dispatch_done.go).
--
--   host_dispatches.done_at        when the dispatch's OWN session called
--       agent_report_done; NULL until then.
--   host_dispatches.done_summary   its one-line summary.
--
-- 🔴 DONE IS NOT ENDED. status keeps deciding the live slot and the forward
-- (0093): a done dispatch whose window is still open stays `running` and holds
-- its slot until the window closes (design 3.7: "Its slot is freed only when the
-- window ends"). Ending it here would make the next listing, which still
-- carries the window's tag, raise the orphan notice for an agent the owner is
-- looking at. So the columns are a recorded outcome only, read by the chief's
-- card.
--
-- 🔴 ONE WRITER (asserted over the Go source with 0093's four):
-- MarkHostDispatchDone, only on a running row whose host and agent_session are
-- the caller's, and only once (done_at IS NULL).
--
-- Must apply after 0096 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the columns inherit the
-- table's row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0097 never reads or writes either column and
-- runs against this schema unchanged. To remove them:
--   ALTER TABLE public.host_dispatches DROP COLUMN IF EXISTS done_summary;
--   ALTER TABLE public.host_dispatches DROP COLUMN IF EXISTS done_at;
--   DELETE FROM schema_migrations WHERE version = 97;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.host_dispatches ADD COLUMN IF NOT EXISTS done_at TIMESTAMPTZ;
ALTER TABLE public.host_dispatches ADD COLUMN IF NOT EXISTS done_summary TEXT NOT NULL DEFAULT ''
    CHECK (char_length(done_summary) <= 300);
