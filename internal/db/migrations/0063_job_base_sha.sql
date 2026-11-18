-- muster 0063 — a job's BASE commit (the machine job API, internal/api
-- jobs.go).
--
-- jobs.base_sha is a commit a sha job's worktree fetches — with the pull
-- request's own credential path — into refs/muster-jobs/<id>-base, so the turn
-- can diff against it ('' = none, as before). A full object id, and only on a
-- sha job (jobs_base_sha_check).
--
-- Rollback: an image from before 0063 never reads the column, and would run
-- every queued job with no base. To remove it:
-- ALTER TABLE public.jobs DROP COLUMN base_sha;
-- DELETE FROM schema_migrations WHERE version = 63.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.jobs ADD COLUMN IF NOT EXISTS base_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE public.jobs DROP CONSTRAINT IF EXISTS jobs_base_sha_check;
ALTER TABLE public.jobs ADD CONSTRAINT jobs_base_sha_check
    CHECK (base_sha = '' OR (sha <> '' AND base_sha ~ '^[0-9a-f]{40}$'));
