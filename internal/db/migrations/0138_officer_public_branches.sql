-- muster 0138 — A DRAFT OFFICER'S PUBLIC BRANCHES (internal/officers
-- public_branches.go; docs/officers.md "Public repositories").
--
--   officer_public_branches   one row per (owner, officer, repository, item):
--       the branch muster named for that item in a ship repository marked
--       public (officer_public_branch). A public repository shows every branch
--       name, so the name carries no officer id and no item number
--       (<prefix>fix/<slug>-<12 random letters>); THIS row is where muster
--       keeps both. The push gate, the body contract, the rate limits, the
--       sweep and the orphan report read a public branch's officer and item
--       here, never from the name. One branch per item and repository
--       (UNIQUE (owner_id, officer, repo, item)); a name names one item
--       (UNIQUE (owner_id, officer, repo, branch)). Rows are never updated.
--
-- 🔴 THE ROW IS THE ONLY PROOF A PUBLIC BRANCH IS THE OFFICER'S: with no row,
-- a push to it, a pull request from it and a branch deletion of it are
-- refused. The branch CHECK keeps the table to names muster mints.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in officer_pr_loops' share
-- class (0127): the row's owner is the officer's owner (a team, for a team
-- officer), read at viewer and written at operator — the draft gate writes and
-- reads it acting as that owner. One policy per command, in 0127's exact
-- shape; owner_id is fixed. rollback/rls_off.sql turns it off with the rest,
-- and --rls-on re-applies this file (db.RLSOn), so every statement is
-- idempotent.
--
-- Must apply after its predecessor, 0137 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT (and db.officerPublicBranchesMigrationVersion) if another
-- migration takes 0138 first.
--
-- ROLLBACK. An older image never reads the table (it names a public
-- repository's branches the private way); roll the image back, then
-- (optionally):
--   DROP TABLE public.officer_public_branches;   (its policies and owner_fixed trigger go with it)
--   DELETE FROM schema_migrations WHERE version = 138;
-- A public branch minted before the rollback is then no officer's to the newer
-- image if the table is dropped: its pull request can still be closed by a
-- human, and the officer is named a new branch.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_public_branches (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id   BIGINT      NOT NULL
               DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
               REFERENCES public.users (id),
    officer    TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    repo       TEXT        NOT NULL CHECK (repo ~ '^[a-z0-9._-]+/[a-z0-9._-]+$'),
    item       INTEGER     NOT NULL CHECK (item > 0),
    -- <prefix>fix/<slug>-<token>: no digit after the prefix, so no number
    branch     TEXT        NOT NULL CHECK (octet_length(branch) <= 255
                                           AND branch ~ '^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*/fix/[a-z]+(-[a-z]+)*-[a-z]{12}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT officer_public_branches_one_per_item UNIQUE (owner_id, officer, repo, item),
    CONSTRAINT officer_public_branches_one_name UNIQUE (owner_id, officer, repo, branch)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.officer_public_branches ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $rls$
DECLARE t TEXT := 'officer_public_branches';
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
