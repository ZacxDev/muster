-- muster 0168 — A DRAFT OFFICER'S PULL-REQUEST QUEUE (internal/officers
-- pr_queue.go; docs/officers.md "The open-pull-request queue").
--
--   officer_pr_queue   one row per (owner, officer, repository, branch): a
--       create of a draft pull request that the open cap
--       (officers.MaxOpenPRsPerRepo) held back. The push gate writes it and
--       answers the officer "queued" (202) instead of refusing; the draft
--       sweep opens the oldest queued row of a repository (queued_at, then
--       id) as a draft once a slot frees, and settles it 'opened' (pr) or
--       'skipped' (note: the branch is gone, its head moved with no new
--       create, a pull request is already open from it, or its change was
--       refused). A second create from a QUEUED branch refreshes head_sha,
--       title and body and keeps queued_at (its place); a create from a
--       settled one queues it again at the back. The settle is conditional on
--       head_sha and state = 'queued', so a decision on an older head never
--       settles a refreshed row.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in officer_pr_loops' share
-- class (0127): the row's owner is the officer's owner (a team, for a team
-- officer), read at viewer and written at operator — the push gate and the
-- sweep write and read it acting as that owner. One policy per command, in
-- 0127's exact shape; owner_id is fixed. rollback/rls_off.sql turns it off
-- with the rest, and --rls-on re-applies this file (db.RLSOn), so every
-- statement is idempotent.
--
-- Must apply after its predecessor, 0167 (the contiguity test refuses a gap):
-- 0167 is the officer auto-restart migration, which lands first.
-- 🔴 RENUMBER IT (and db.officerPRQueueMigrationVersion) if another migration
-- takes 0168 first.
--
-- ROLLBACK. An older image never reads the table (a create past the cap is
-- refused there, as before); roll the image back, then (optionally):
--   DROP TABLE public.officer_pr_queue;   (its policies and owner_fixed trigger go with it)
--   DELETE FROM schema_migrations WHERE version = 168;
-- A create still queued at the rollback is then never opened: the officer's
-- branch is still pushed, and a human (or the officer, once a slot frees)
-- opens it.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_pr_queue (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id   BIGINT      NOT NULL
               DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
               REFERENCES public.users (id),
    officer    TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    repo       TEXT        NOT NULL CHECK (repo ~ '^[a-z0-9._-]+/[a-z0-9._-]+$'),
    branch     TEXT        NOT NULL CHECK (octet_length(branch) BETWEEN 1 AND 255),
    head_sha   TEXT        NOT NULL CHECK (head_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    title      TEXT        NOT NULL CHECK (octet_length(title) <= 1024),
    body       TEXT        NOT NULL CHECK (octet_length(body) <= 262144),
    item       INTEGER     NOT NULL CHECK (item > 0),
    instance   TEXT        NOT NULL DEFAULT '' CHECK (octet_length(instance) <= 255),
    state      TEXT        NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'opened', 'skipped')),
    pr         INTEGER     CHECK (pr > 0),
    note       TEXT        NOT NULL DEFAULT '' CHECK (octet_length(note) <= 1000),
    queued_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at TIMESTAMPTZ,
    CONSTRAINT officer_pr_queue_one_per_branch UNIQUE (owner_id, officer, repo, branch),
    CONSTRAINT officer_pr_queue_opened_names_pr CHECK (state <> 'opened' OR pr IS NOT NULL)
);

-- The drain's read: an officer's queued rows of a repository, oldest first.
CREATE INDEX IF NOT EXISTS officer_pr_queue_waiting
    ON public.officer_pr_queue (officer, repo, queued_at, id) WHERE state = 'queued';

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.officer_pr_queue ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $rls$
DECLARE t TEXT := 'officer_pr_queue';
BEGIN
    EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', t);
    EXECUTE format('DROP POLICY IF EXISTS team_read ON public.%I', t);
    EXECUTE format('DROP POLICY IF EXISTS team_insert ON public.%I', t);
    EXECUTE format('DROP POLICY IF EXISTS team_update ON public.%I', t);
    EXECUTE format('DROP POLICY IF EXISTS team_delete ON public.%I', t);
    EXECUTE format($p$CREATE POLICY team_read ON public.%I AS PERMISSIVE FOR SELECT TO PUBLIC
            USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(1)), '{}'))
                OR current_setting('muster.scope', true) = 'all')$p$, t);
    EXECUTE format($p$CREATE POLICY team_insert ON public.%I AS PERMISSIVE FOR INSERT TO PUBLIC
            WITH CHECK (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(2)), '{}'))
                OR current_setting('muster.scope', true) = 'all')$p$, t);
    EXECUTE format($p$CREATE POLICY team_update ON public.%I AS PERMISSIVE FOR UPDATE TO PUBLIC
            USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(2)), '{}'))
                OR current_setting('muster.scope', true) = 'all')
            WITH CHECK (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(2)), '{}'))
                OR current_setting('muster.scope', true) = 'all')$p$, t);
    EXECUTE format($p$CREATE POLICY team_delete ON public.%I AS PERMISSIVE FOR DELETE TO PUBLIC
            USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(2)), '{}'))
                OR current_setting('muster.scope', true) = 'all')$p$, t);
    EXECUTE format('DROP TRIGGER IF EXISTS owner_fixed ON public.%I', t);
    EXECUTE format('CREATE TRIGGER owner_fixed BEFORE UPDATE OF owner_id ON public.%I
        FOR EACH ROW EXECUTE FUNCTION public.muster_owner_fixed()', t);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.%I TO muster_app', t);
    EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE public.%I TO muster_app', t || '_id_seq');
END
$rls$;
