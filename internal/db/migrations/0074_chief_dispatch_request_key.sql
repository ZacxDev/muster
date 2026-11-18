-- muster 0074 — THE CHIEF'S agent_dispatch request_key LOOKUP
-- (internal/plugins/chief_dispatch.go, requestKeyMatch;
-- docs/design/chief-dispatch.md §2.6.1, where it is named 0072: 0072 and 0073 were
-- taken by 0072_task_references.sql and 0073_session_episodes.sql first).
--
-- agent_dispatch's request_key makes a call idempotent per owner for 24 hours.
-- The key lives in the reservation's own audit row (detail.request_key, with
-- detail.request_hash beside it); there is no new table. This partial index
-- keeps the lookup — the owner's reservations carrying that key — cheap.
--
-- 🔴 ADDITIVE ONLY: one index, no column, no constraint, no data change. An
-- image from before 0074 never writes or reads detail.request_key and runs
-- unchanged over it.
--
-- [rollback-sql] Roll the image back first or not at all, in either order:
--   DROP INDEX IF EXISTS public.idx_audit_log_chief_dispatch_request_key;
--   DELETE FROM schema_migrations WHERE version = 74;
-- Reservation rows written while 0074 was applied keep their request_key in
-- detail; an older image ignores it.

SET LOCAL lock_timeout = '3s';

-- The predicate spells the action as a literal, and requestKeyMatch's query
-- spells the same literal, so the planner can prove the query implies it.
CREATE INDEX IF NOT EXISTS idx_audit_log_chief_dispatch_request_key
    ON public.audit_log (user_id, (detail->>'request_key'))
    WHERE action = 'task.chief_dispatch' AND detail ? 'request_key';
