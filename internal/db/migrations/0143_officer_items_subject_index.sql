-- muster 0143 — AN INDEX FOR "HAS THIS OFFICER EVER HAD THIS SUBJECT?"
-- (officers O-D; internal/officers/builtin store.go EnqueueOnce, fed by the
-- session minder, internal/api session_minder.go).
--
--   officer_items_subject_ever   (owner_id, officer, kind, subject_key), on
--       EVERY row whatever its status.
--
-- EnqueueOnce adds an item only when NOT EXISTS an item of that officer, kind
-- and subject in ANY status — live or finished — so a stop is minded once for
-- good. 0118's officer_items_live_subject has the same columns but is partial
-- (status IN ('open', 'claimed', 'waiting')), so it cannot answer for a done
-- item, and officer_items_queue (owner_id, officer, status, id) leaves the
-- probe filtering the officer's whole item history on kind and subject_key on
-- every state pass. This index answers it in one probe.
--
-- Plain CREATE INDEX, not CONCURRENTLY: the runner applies each migration in a
-- transaction (CONCURRENTLY cannot run in one), as every index migration here
-- does. It takes a SHARE lock on officer_items while it builds — writes to the
-- table wait for it; reads do not — and lock_timeout bounds the wait for that
-- lock.
--
-- Inert without its code: an index only, nothing reads it by name, and an
-- older image runs unchanged over it. Idempotent.
--
-- Must apply after its predecessor, 0141 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT if another migration takes 0143 first.
--
-- ROLLBACK (no image rollback needed; any image runs with or without it):
--   DROP INDEX IF EXISTS public.officer_items_subject_ever;
--   DELETE FROM schema_migrations WHERE version = 142;

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS officer_items_subject_ever
    ON public.officer_items (owner_id, officer, kind, subject_key);
