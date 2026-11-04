-- muster 0075 — A FAILED REFERENCE WRITE CAN BE DISMISSED (internal/notes/pgstore_refs.go
-- RetryRefWrite / DismissRefWrite; docs/plugins.md#writes-on-a-reference-are-queued).
--
-- 0072's failed write stayed on the task page for ever ("failed: <error>").
-- Its owner may now RETRY it (back to pending: no column needed) or DISMISS it:
--
--   task_ref_writes.dismissed_at  when the owner dismissed this failed write.
--                                 The row is kept (state stays 'failed', so the
--                                 state machine 0072 declared is unchanged); the
--                                 task page leaves it out, and it can be neither
--                                 retried nor dismissed again.
--
-- 🔴 ONLY A FAILED WRITE IS EVER DISMISSED (task_ref_writes_dismissed_failed):
-- a dismissed row that became pending or claimed again would be handed to the
-- plugin while hidden from the page.
--
-- A column with no default and a CHECK every existing row passes (all NULL): no
-- rewrite. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image shows a dismissed write
-- as failed again, which is what it is):
--   ALTER TABLE task_ref_writes DROP CONSTRAINT IF EXISTS task_ref_writes_dismissed_failed,
--     DROP COLUMN IF EXISTS dismissed_at;
--   DELETE FROM schema_migrations WHERE version = 75;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.task_ref_writes ADD COLUMN IF NOT EXISTS dismissed_at TIMESTAMPTZ;

ALTER TABLE public.task_ref_writes DROP CONSTRAINT IF EXISTS task_ref_writes_dismissed_failed;
ALTER TABLE public.task_ref_writes ADD CONSTRAINT task_ref_writes_dismissed_failed
    CHECK (dismissed_at IS NULL OR state = 'failed');
