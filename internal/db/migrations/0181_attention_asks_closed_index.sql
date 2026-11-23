-- muster 0181 — AN INDEX FOR THE DONE TAB'S PAGES (internal/attention
-- closed.go Closed; internal/api asks_done.go; docs/attention.md "Nothing
-- you decide is lost").
--
--   attention_asks_closed_page   (owner_id, coalesce(resolved_at, expires_at)
--       DESC, id DESC), on every row.
--
-- Closed reads one owner's closed asks newest closed first, keyset-paged on
-- (coalesce(resolved_at, expires_at), id) — the same expression its ORDER BY
-- and its cursor use — so a page is one index range read from the cursor
-- down, stopping at the page size, whatever the owner's history.
--
-- NOT PARTIAL on status: Closed's WHERE is (status <> 'open' OR expires_at <=
-- now()) — an open row past its expiry that no sweep has moved yet is listed
-- too — and a partial index on status <> 'open' cannot answer that OR, so the
-- planner would not use it. The open rows it carries are few (at most
-- MaxOpenPerOwner per owner) and are filtered in the scan.
--
-- Plain CREATE INDEX, not CONCURRENTLY: the runner applies each migration in a
-- transaction (CONCURRENTLY cannot run in one), as every index migration here
-- does (0143). It takes a SHARE lock on attention_asks while it builds —
-- writes wait for it; reads do not — and lock_timeout bounds the wait for
-- that lock.
--
-- Inert without its code: an index only, nothing reads it by name, and an
-- older image runs unchanged over it. Idempotent.
--
-- Must apply after its predecessor, 0180 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT if another migration takes 0181 first.
--
-- ROLLBACK (no image rollback needed; any image runs with or without it):
--   DROP INDEX IF EXISTS public.attention_asks_closed_page;
--   DELETE FROM schema_migrations WHERE version = 181;

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS attention_asks_closed_page
    ON public.attention_asks (owner_id, (coalesce(resolved_at, expires_at)) DESC, id DESC);
