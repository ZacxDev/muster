-- muster 0166 — SCHEDULED HOST JOBS (internal/hostjobs; internal/api
-- hostjobs.go; docs/host-jobs.md).
--
--   host_jobs      one row per (owner, host, job name): the job AS ITS HOST
--       LAST REPORTED IT (the host's muster-mirror jobs file is the only
--       place a job is declared; muster never writes a command), its next
--       cron fire, and page_id, the page muster made for it (every job
--       writes its own: a host names no page).
--       declared is false for a job the host no longer reports: kept for its
--       runs, never fired.
--   host_job_runs  one row per fire or `muster job run`: its trigger, its
--       status (running, ok, failed, timeout, invalid, offline, skipped,
--       refused, lost), when it ran, its exit code, how long it took, the
--       page version it wrote (none when its output was unchanged), the tail
--       of its stderr, and the Attention item a failure raised. At most ONE
--       running run per job (host_job_runs_one_running): a fire while one is
--       running is recorded as skipped.
--
--   A command is stored for DISPLAY only (`muster job list`): no reader
--   hands it back to the host, which runs its own copy.
--
--   pages.rolling_versions  a page muster made for a job (true) keeps its
--       newest pages.MaxVersionsPerPage versions: a new version past the cap
--       drops the oldest version nothing references (not the current one,
--       which every live link serves; not one a response names) instead of
--       being refused, so the job's page never fills and its id and links
--       never change. Every other page is false and keeps the cap (internal/
--       pages rollingDrop). A new column on an owned table; the pages
--       table's row policies (0116, then 0165's) cover it.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, PERSONAL share class: a
-- job's page is one of the owner's PERSONAL pages (no created_by_user: 0165
-- records that only on a team's page), so a TEAM host's jobs are refused before they reach these tables (internal/api
-- hostJobsReported). A run names its job by (owner_id, job_id), so a run
-- cannot name another owner's job. rollback/rls_off.sql turns RLS off with
-- the rest, and --rls-on re-applies this file (db.RLSOn), so every
-- statement is idempotent.
--
-- Written as 0163, renumbered 0166 at merge (after 0165, its predecessor;
-- the contiguity test refuses a gap): 🔴 RENUMBER IT (and
-- db.hostJobsMigrationVersion) at merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads them):
--   DROP TABLE public.host_job_runs, public.host_jobs;
--   ALTER TABLE public.pages DROP COLUMN rolling_versions;
--   DELETE FROM schema_migrations WHERE version = 166;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.pages ADD COLUMN IF NOT EXISTS rolling_versions BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS public.host_jobs (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id) ON DELETE CASCADE,
    host          TEXT        NOT NULL CHECK (host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    name          TEXT        NOT NULL CHECK (name ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(name) <= 63),
    cron          TEXT        NOT NULL CHECK (cron ~ '^\S+( \S+){4}$' AND length(cron) <= 100),
    command       TEXT[]      NOT NULL CHECK (cardinality(command) BETWEEN 1 AND 32),
    page_id       BIGINT      CHECK (page_id > 0),
    title         TEXT        NOT NULL DEFAULT '' CHECK (char_length(title) <= 200),
    output        TEXT        NOT NULL DEFAULT 'markdown' CHECK (output IN ('markdown', 'blocks')),
    timeout_s     INTEGER     NOT NULL CHECK (timeout_s BETWEEN 1 AND 21600),
    declared      BOOLEAN     NOT NULL DEFAULT true,
    next_fire_at  TIMESTAMPTZ,
    last_fired_at TIMESTAMPTZ,
    reported_at   TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT host_jobs_one_name UNIQUE (owner_id, host, name),
    CONSTRAINT host_jobs_owner_id UNIQUE (owner_id, id),
    -- a job its host stopped reporting is never due
    CONSTRAINT host_jobs_undeclared_not_due CHECK (declared OR next_fire_at IS NULL)
);

-- The scheduler's read: declared jobs by their next fire.
CREATE INDEX IF NOT EXISTS host_jobs_due
    ON public.host_jobs (next_fire_at) WHERE declared AND next_fire_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS public.host_job_runs (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id) ON DELETE CASCADE,
    job_id       BIGINT      NOT NULL,
    host         TEXT        NOT NULL CHECK (char_length(host) <= 63),
    job          TEXT        NOT NULL CHECK (char_length(job) <= 63),
    trigger      TEXT        NOT NULL CHECK (trigger IN ('cron', 'manual')),
    status       TEXT        NOT NULL
                 CHECK (status IN ('running', 'ok', 'failed', 'timeout', 'invalid', 'offline', 'skipped', 'refused', 'lost')),
    timeout_s    INTEGER     NOT NULL CHECK (timeout_s BETWEEN 1 AND 21600),
    fired_at     TIMESTAMPTZ NOT NULL,
    started_at   TIMESTAMPTZ,
    ended_at     TIMESTAMPTZ,
    exit_code    INTEGER,
    duration_ms  BIGINT      CHECK (duration_ms >= 0),
    page_id      BIGINT      CHECK (page_id > 0),
    page_version INTEGER     CHECK (page_version > 0),
    stderr_tail  TEXT        NOT NULL DEFAULT '' CHECK (octet_length(stderr_tail) <= 2048),
    detail       TEXT        NOT NULL DEFAULT '' CHECK (char_length(detail) <= 1000),
    notice_id    BIGINT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT host_job_runs_job_fkey FOREIGN KEY (owner_id, job_id)
        REFERENCES public.host_jobs (owner_id, id) ON DELETE CASCADE,
    -- a version written names its page
    CONSTRAINT host_job_runs_version_has_page CHECK (page_version IS NULL OR page_id IS NOT NULL)
);

-- At most ONE running run per job: the skip-while-running rule, held by the
-- database whichever replica or path starts the run.
CREATE UNIQUE INDEX IF NOT EXISTS host_job_runs_one_running
    ON public.host_job_runs (job_id) WHERE status = 'running';
-- A job's runs, newest first (`muster job runs`), and the per-job prune.
CREATE INDEX IF NOT EXISTS host_job_runs_by_job
    ON public.host_job_runs (owner_id, job_id, id DESC);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.host_jobs     ALTER COLUMN owner_id SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.host_job_runs ALTER COLUMN owner_id SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $rls$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['host_jobs', 'host_job_runs'] LOOP
        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', t);
        EXECUTE format($p$CREATE POLICY owner_isolation ON public.%I
            AS PERMISSIVE FOR ALL TO PUBLIC
            USING (
                owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR current_setting('muster.scope', true) = 'all'
            )
            WITH CHECK (
                owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR current_setting('muster.scope', true) = 'all'
            )$p$, t);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.%I TO muster_app', t);
        EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE public.%I TO muster_app', t || '_id_seq');
    END LOOP;
END
$rls$;
