-- muster 0178 — an index for a mirrored session's asks (the session view's
-- Asks tab: internal/attention SessionAsks, internal/api session_asks.go).
--
-- The Open button's slot re-reads a window's asks every 10 s while its tab is
-- shown, by host and the session keys the window runs, in ANY state (open
-- and answered). idx_attention_asks_open covers only open rows, and an
-- agent's asks already have idx_attention_asks_agent; without this, a
-- window's read scanned every ask its owner ever had.
--
-- Rollback: DROP INDEX IF EXISTS public.idx_attention_asks_mirror_session;
-- an older image never reads it.

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_attention_asks_mirror_session ON public.attention_asks (mirror_host, mirror_session)
	WHERE mirror_session IS NOT NULL;
