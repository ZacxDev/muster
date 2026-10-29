-- muster 0063 — a job's REPO CONFIG OVERLAY switch (the machine job API,
-- internal/api jobs.go).
--
-- jobs.repo_config_overlay is whether a sha job's worktree gets the
-- repository's agent configuration (every `.claude/`, the root CLAUDE.md,
-- CLAUDE.local.md, AGENTS.md and .mcp.json) from the default branch (true, as
-- before this migration), or none of it at all (false): the commit's copies are
-- removed either way (agentprovision jobOverlayScript). Only a sha job has a
-- worktree, so false needs a sha; and a config_ref names the configuration to
-- overlay, so it needs true.
--
-- Rollback: an image from before 0063 never reads this column, and would run
-- every queued job with the overlay. To remove it:
-- ALTER TABLE public.jobs DROP COLUMN repo_config_overlay;
-- DELETE FROM schema_migrations WHERE version = 63.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.jobs ADD COLUMN IF NOT EXISTS repo_config_overlay BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE public.jobs DROP CONSTRAINT IF EXISTS jobs_repo_config_overlay_check;
ALTER TABLE public.jobs ADD CONSTRAINT jobs_repo_config_overlay_check
    CHECK (repo_config_overlay OR (sha <> '' AND config_ref = ''));
