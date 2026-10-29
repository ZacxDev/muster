-- muster 0063 — a job's REPO CONFIG OVERLAY switch and BASE commit (the
-- machine job API, internal/api jobs.go).
--
-- jobs.repo_config_overlay is whether a sha job's worktree gets the
-- repository's agent configuration (every `.claude/`, the root CLAUDE.md,
-- CLAUDE.local.md, AGENTS.md and .mcp.json) from the default branch (true, as
-- before this migration), or none of it at all (false): the commit's copies are
-- removed either way (agentprovision jobOverlayScript). Only a sha job has a
-- worktree, so false needs a sha; and a config_ref names the configuration to
-- overlay, so it needs true.
--
-- jobs.base_sha is a commit a sha job's worktree fetches — with the pull
-- request's own credential path — into refs/muster-jobs/<id>-base, so the turn
-- can diff against it ('' = none, as before). A full object id, and only on a
-- sha job.
--
-- Rollback: an image from before 0063 never reads these columns, and would run
-- every queued job with the overlay and no base. To remove them:
-- ALTER TABLE public.jobs DROP COLUMN repo_config_overlay, DROP COLUMN base_sha;
-- DELETE FROM schema_migrations WHERE version = 63.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.jobs ADD COLUMN IF NOT EXISTS repo_config_overlay BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE public.jobs DROP CONSTRAINT IF EXISTS jobs_repo_config_overlay_check;
ALTER TABLE public.jobs ADD CONSTRAINT jobs_repo_config_overlay_check
    CHECK (repo_config_overlay OR (sha <> '' AND config_ref = ''));

ALTER TABLE public.jobs ADD COLUMN IF NOT EXISTS base_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE public.jobs DROP CONSTRAINT IF EXISTS jobs_base_sha_check;
ALTER TABLE public.jobs ADD CONSTRAINT jobs_base_sha_check
    CHECK (base_sha = '' OR (sha <> '' AND base_sha ~ '^[0-9a-f]{40}$'));
