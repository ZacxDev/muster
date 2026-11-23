-- muster 0180 — an index for a mirrored session's asks (the session view's
-- Asks tab and its Open button: internal/attention SessionAsks and
-- SessionAskCounts, internal/api session_asks.go).
--
-- 0180, not 0178: 0178 and 0179 were taken by #998 and #1001 while this was
-- open. Renumber again if a later number lands first (the contiguity test,
-- internal/db migrate_contiguous_test.go, fails on a gap).
--
-- The Open button's slot re-reads a window's ask counts — on every change of
-- the queue its page hears, and every 10 s on a page that hears none (the
-- Terminal view) — by host and the session keys the window runs, in ANY state
-- (open and answered). idx_attention_asks_open covers only open rows, and an
-- agent's asks already have idx_attention_asks_agent; without this, a
-- window's read scanned every ask its owner ever had.
--
-- Rollback (an older image never reads the index):
--   DROP INDEX IF EXISTS public.idx_attention_asks_mirror_session;
--   DELETE FROM schema_migrations WHERE version = 180;

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_attention_asks_mirror_session ON public.attention_asks (mirror_host, mirror_session)
	WHERE mirror_session IS NOT NULL;
