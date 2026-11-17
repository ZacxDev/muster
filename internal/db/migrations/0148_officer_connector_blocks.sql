-- muster 0148 — AN OFFICER'S ITEMS BLOCKED ON A PENDING CONNECTOR GRANT
-- (internal/officers connector_blocks.go; docs/officers.md "Re-running
-- blocked items").
--
--   officer_connector_blocks   one row per (owner, officer, grant, item): an
--       item the officer's instance held a live claim on in HQ when a
--       connector refused it because grant `grant_id` was still pending
--       (the proxy, the query and internal_http tools, the kube read, or an
--       explicit request). When that grant is approved, muster asks HQ to
--       re-run the item if it finished escalated, and skips it otherwise
--       (finished done / no_action, or still live); either way it sets
--       settled_at, and a settled row is never looked at again. blocked_at is
--       the first refusal's time (a repeat refusal writes nothing): an
--       approval reads only rows recorded within its window
--       (officers.RerunWindow). A row stays unsettled when HQ could not
--       answer (no rerun route yet, an outage, an item it did not list), so a
--       later approval of the grant tries it again.
--
-- 🔴 THE LINK IS THE GRANT ID AND THE HELD ITEM, NEVER TEXT: no summary or
-- message is ever matched to find what a grant blocked. Nothing else is kept:
-- what happened to each row is in the log line its approval wrote.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in officer_pr_loops' share
-- class (0127): the row's owner is the officer's owner (a team, for a team
-- officer), read at viewer and written at operator — muster writes and reads
-- it acting as that owner. One policy per command, in 0127's exact shape;
-- owner_id is fixed. rollback/rls_off.sql turns it off with the rest, and
-- --rls-on re-applies this file (db.RLSOn), so every statement is idempotent.
--
-- Must apply after its predecessor, 0147 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT (and db.officerConnectorBlocksMigrationVersion) if another
-- migration takes 0148 first.
--
-- ROLLBACK. An older image never reads the table (an approval then re-runs
-- nothing, as before); roll the image back, then (optionally):
--   DROP TABLE public.officer_connector_blocks;   (its policies and owner_fixed trigger go with it)
--   DELETE FROM schema_migrations WHERE version = 148;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_connector_blocks (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id   BIGINT      NOT NULL
               DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
               REFERENCES public.users (id),
    officer    TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    grant_id   BIGINT      NOT NULL CHECK (grant_id > 0),
    item       TEXT        NOT NULL CHECK (item ~ '^[A-Za-z0-9_-]{1,64}$'),
    blocked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at TIMESTAMPTZ,
    CONSTRAINT officer_connector_blocks_one_per_item UNIQUE (owner_id, officer, grant_id, item)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.officer_connector_blocks ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $rls$
DECLARE t TEXT := 'officer_connector_blocks';
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

-- The approval's read: one officer's unsettled rows of one grant, newest first.
CREATE INDEX IF NOT EXISTS officer_connector_blocks_grant
    ON public.officer_connector_blocks (owner_id, officer, grant_id, blocked_at DESC)
    WHERE settled_at IS NULL;
