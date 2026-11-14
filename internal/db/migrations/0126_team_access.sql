-- muster 0126 — TEAM ACCESS CORE (docs/design/team-sharing.md, phase 2a).
--
-- WHAT A TEAM MEMBER MAY READ AND WRITE, decided in the database. Nothing in
-- this migration (or in phase 2a's code) CREATES a team-owned row: no request
-- can act as a team (phase 1), and the paths that will file team rows — team
-- officers (phase 3), team tasks and the team chief (phase 4), team hosts
-- (phase 2b) — come later. What lands here is the rule those paths will meet.
--
--   muster.teams            a connection setting, set on every acquire beside
--                           muster.uid (internal/db/scope.go), from the
--                           request principal's memberships in phase 1's
--                           canonical form '<team>:<role>,…', e.g.
--                           '12:operator,15:viewer'. ONLY a person's own
--                           request doors carry it (authz.Principal.TeamScope:
--                           the browser session and a personal API token);
--                           every background loop and machine door acts with
--                           none, so a per-owner sweep never meets team rows.
--   muster_team_role(o)     0 none, 1 viewer, 2 operator, 3 manager: the
--                           caller's role in owner o, parsed from the setting
--                           by NAME (a role is never cast ::int). For code
--                           that asks about ONE owner; the policies use
--   muster_team_ids(r)      the ids of the teams held at role >= r, as one
--                           InitPlan per query (see below).
--   muster_team_people(t)   SECURITY DEFINER: (id, display_name) of team t's
--                           members, only for a caller who is a member of t.
--                           users is self_read, so without it a manager could
--                           not name their own teammates. No email, no role,
--                           no disabled flag.
--
-- THE SHARE CLASSES. Every owned table is in exactly one (pinned by
-- TestEveryOwnedTableHasAShareClass in internal/db, which fails when a class
-- changes or an owned table appears unclassified):
--   personal  owner_isolation, unchanged: credentials, connectors, devices,
--             push, preferences, pins, recents, tags, flows, pages, jobs and
--             the rest below. NEVER shared. And `agents`, NOT SHARED UNTIL
--             PHASE 4: its rows carry an agent's live secrets (hooks_token,
--             gateway_secret), which phase 4 first moves into an owner-isolated
--             table before it shares agents.
--   shared    the team policies: read at viewer, write at operator.
--   managed   the team policies: read at viewer or operator (per table),
--             write at manager.
-- The team policies keep owner_isolation's two arms and add one:
--   owner_id = muster.uid  OR  owner_id = ANY (coalesce((SELECT muster_team_ids(<role>)), '{}'))  OR  scope all
-- as FOUR policies, one per command (team_read SELECT at the read role;
-- team_insert, team_update, team_delete at the write role). Never one FOR
-- ALL: its USING is DELETE's too, and a viewer would delete the team's rows.
-- owner_id still DEFAULTS to muster.uid, the PERSON: an insert that does not
-- name a team lands private, and naming one succeeds only at the write role.
--
-- 🔴 owner_id NEVER CHANGES ON A SHARE-CLASS TABLE outside the system scope
-- (muster_owner_fixed): with an UPDATE arm open to a team, a member could
-- otherwise re-own a team row to themselves, or move their own row into the
-- team. Moving a row into a team (phase 4's "move a task") is a System write.
--
-- ATTRIBUTION — who did it, beside the owner it was done to:
--   audit_log.team_id                 the team acted on (the design's
--                                     "audit_log.owner_id", renamed: an
--                                     owner_id column marks an OWNED table
--                                     everywhere — RLSStatus, the ledgers —
--                                     and audit_log is user-keyed); self_read
--                                     gains a manager arm for it.
--   send_queue.queued_by              DEFAULT muster.uid: the person who queued.
--   note_comments.author_id           DEFAULT muster.uid: the person who wrote.
--   attention_asks.answered_by_user   stamped by a trigger when an ask moves
--                                     to answered / answered_in_session.
--   plugin_reply_drafts.decided_by_user
--                                     stamped by a trigger when a draft moves
--                                     to approved / rejected.
-- Each is the acting principal's muster.uid, so NULL for the system scope
-- (a background loop) and every row written before this migration. The
-- columns are added WITHOUT a default and given one afterwards, so existing
-- rows are not rewritten.
--
-- db.RLSOn re-applies this file after 0007 and the other owned-table
-- migrations (they re-create owner_isolation; this drops it again on the
-- share-class tables). Idempotent. rollback/rls_off.sql turns the owned
-- tables' security off and leaves these policies inert, as it does 0007's.
--
-- Must apply after its predecessor (the contiguity test refuses a gap); the
-- number follows phase 1's 0125 and moves with it.
--
-- ROLLBACK (after rolling the image back; an older image never reads the
-- setting or the columns, and owner_isolation is restored on every table):
--   SELECT public.muster_team_access_rollback();
--   DROP FUNCTION IF EXISTS public.muster_team_access_rollback();
--   DELETE FROM schema_migrations WHERE version = 126;

SET LOCAL lock_timeout = '3s';

-- ---------------------------------------------------------------------------
-- 1. The role function and the teammates' names.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.muster_team_role(o bigint) RETURNS integer
    LANGUAGE sql
    STABLE
    PARALLEL SAFE
    SET search_path = pg_catalog
AS $$
    SELECT coalesce(max(CASE split_part(x, ':', 2)
                            WHEN 'viewer'   THEN 1
                            WHEN 'operator' THEN 2
                            WHEN 'manager'  THEN 3
                            ELSE 0 END), 0)
    FROM unnest(string_to_array(nullif(current_setting('muster.teams', true), ''), ',')) AS x
    WHERE split_part(x, ':', 1) = o::text
$$;

-- muster_team_ids(r): the ids of the teams the caller holds at least role r
-- in (1 viewer, 2 operator, 3 manager), from the same setting. The policies
-- call it as a SCALAR SUBQUERY, `owner_id = ANY (coalesce((SELECT muster_team_ids(r)), '{}'))`,
-- which Postgres plans as an InitPlan: ONE parse per query, not one per row
-- Measured on 400k rows (phase 2a PR): count(*) ~22 ms with this InitPlan,
-- ~26 ms under owner_isolation alone, ~200 ms with a per-row
-- muster_team_role(owner_id) call instead.
CREATE OR REPLACE FUNCTION public.muster_team_ids(min_role integer) RETURNS bigint[]
    LANGUAGE sql
    STABLE
    PARALLEL SAFE
    SET search_path = pg_catalog
AS $$
    SELECT coalesce(array_agg(split_part(x, ':', 1)::bigint), '{}')
    FROM unnest(string_to_array(nullif(current_setting('muster.teams', true), ''), ',')) AS x
    WHERE split_part(x, ':', 1) ~ '^[1-9][0-9]{0,18}$'
      AND CASE split_part(x, ':', 2)
              WHEN 'viewer'   THEN 1
              WHEN 'operator' THEN 2
              WHEN 'manager'  THEN 3
              ELSE 0 END >= min_role
      AND min_role >= 1
$$;

CREATE OR REPLACE FUNCTION public.muster_team_people(team bigint)
    RETURNS TABLE (id bigint, display_name text)
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = pg_catalog
    SET muster.scope = 'all'
AS $$
    SELECT u.id, u.display_name
    FROM public.team_members m JOIN public.users u ON u.id = m.user_id
    WHERE m.team_id = team
      AND EXISTS (SELECT 1 FROM public.team_members me
                  WHERE me.team_id = team
                    AND me.user_id = nullif(current_setting('muster.uid', true), '')::bigint)
    ORDER BY u.id
$$;

-- ---------------------------------------------------------------------------
-- 2. owner_id is fixed on a share-class table outside the system scope.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.muster_owner_fixed() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    -- Only the APPLICATION role is held to it, as row-level security holds
    -- only it: the login role (a migration, psql, a restore) is not.
    IF NEW.owner_id IS DISTINCT FROM OLD.owner_id
       AND current_user = 'muster_app'
       AND current_setting('muster.scope', true) IS DISTINCT FROM 'all' THEN
        RAISE EXCEPTION '%.owner_id never changes outside the system scope (% -> %)',
            TG_TABLE_NAME, OLD.owner_id, NEW.owner_id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END
$$;

-- ---------------------------------------------------------------------------
-- 3. The share classes: (table, read role, write role). 1 viewer, 2
--    operator, 3 manager. A table not listed keeps owner_isolation.
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    r record;
BEGIN
    FOR r IN SELECT * FROM (VALUES
        -- shared: read viewer, write operator
        ('agent_focus', 1, 2), ('agent_focus_history', 1, 2),
        ('ask_attempts', 1, 2), ('asks', 1, 2), ('attention_asks', 1, 2),
        ('chat_messages', 1, 2), ('chat_sessions', 1, 2), ('chief_nudges', 1, 2),
        ('chief_proposals', 1, 2), ('chief_turn_usage', 1, 2), ('note_attachments', 1, 2),
        ('note_comments', 1, 2), ('notes', 1, 2), ('officer_items', 1, 2),
        ('officer_judgement_sessions', 1, 2), ('officer_llm_usage', 1, 2), ('officer_pr_loops', 1, 2),
        ('routing_rules', 1, 2), ('send_queue', 1, 2), ('session_episodes', 1, 2),
        ('session_prs', 1, 2), ('session_routes', 1, 2), ('task_ref_writes', 1, 2),
        ('task_sessions', 1, 2), ('uploads', 1, 2),
        -- managed: write manager
        ('mirror_tokens', 1, 3), ('officer_defs', 1, 3), ('officer_schedules', 1, 3),
        ('officer_types', 1, 3), ('plugin_handoffs', 2, 3), ('plugin_message_handling', 2, 3),
        ('plugin_messages', 2, 3), ('plugin_reply_drafts', 2, 3), ('plugin_threads', 2, 3),
        ('plugins', 2, 3)
    ) AS c(tbl, rd, wr) LOOP
        EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', r.tbl);
        -- One policy PER COMMAND, not one FOR ALL: a FOR ALL policy's USING
        -- is also DELETE's (and UPDATE's row filter), so a single read-role
        -- USING would let a VIEWER delete the team's rows. SELECT is admitted
        -- at the read role; INSERT, UPDATE and DELETE only at the write role.
        EXECUTE format('DROP POLICY IF EXISTS team_read ON public.%I', r.tbl);
        EXECUTE format('DROP POLICY IF EXISTS team_insert ON public.%I', r.tbl);
        EXECUTE format('DROP POLICY IF EXISTS team_update ON public.%I', r.tbl);
        EXECUTE format('DROP POLICY IF EXISTS team_delete ON public.%I', r.tbl);
        EXECUTE format($p$CREATE POLICY team_read ON public.%I AS PERMISSIVE FOR SELECT TO PUBLIC
                USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(%s)), '{}'))
                    OR current_setting('muster.scope', true) = 'all')$p$, r.tbl, r.rd);
        EXECUTE format($p$CREATE POLICY team_insert ON public.%I AS PERMISSIVE FOR INSERT TO PUBLIC
                WITH CHECK (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(%s)), '{}'))
                    OR current_setting('muster.scope', true) = 'all')$p$, r.tbl, r.wr);
        EXECUTE format($p$CREATE POLICY team_update ON public.%I AS PERMISSIVE FOR UPDATE TO PUBLIC
                USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(%s)), '{}'))
                    OR current_setting('muster.scope', true) = 'all')
                WITH CHECK (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(%s)), '{}'))
                    OR current_setting('muster.scope', true) = 'all')$p$, r.tbl, r.wr, r.wr);
        EXECUTE format($p$CREATE POLICY team_delete ON public.%I AS PERMISSIVE FOR DELETE TO PUBLIC
                USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(%s)), '{}'))
                    OR current_setting('muster.scope', true) = 'all')$p$, r.tbl, r.wr);
        EXECUTE format('DROP TRIGGER IF EXISTS owner_fixed ON public.%I', r.tbl);
        EXECUTE format('CREATE TRIGGER owner_fixed BEFORE UPDATE OF owner_id ON public.%I
            FOR EACH ROW EXECUTE FUNCTION public.muster_owner_fixed()', r.tbl);
    END LOOP;
END
$$;

-- ---------------------------------------------------------------------------
-- 4. Attribution.
-- ---------------------------------------------------------------------------

ALTER TABLE public.audit_log ADD COLUMN IF NOT EXISTS team_id BIGINT REFERENCES public.users (id);
CREATE INDEX IF NOT EXISTS idx_audit_log_team ON public.audit_log (team_id, at DESC) WHERE team_id IS NOT NULL;
DROP POLICY IF EXISTS self_read ON public.audit_log;
CREATE POLICY self_read ON public.audit_log AS PERMISSIVE FOR SELECT TO PUBLIC
    USING (
        user_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR team_id = ANY (coalesce((SELECT public.muster_team_ids(3)), '{}'))
        OR current_setting('muster.scope', true) = 'all'
    );

ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS queued_by BIGINT;
ALTER TABLE public.send_queue ALTER COLUMN queued_by
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.note_comments ADD COLUMN IF NOT EXISTS author_id BIGINT;
ALTER TABLE public.note_comments ALTER COLUMN author_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS answered_by_user BIGINT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS decided_by_user BIGINT;

CREATE OR REPLACE FUNCTION public.muster_stamp_answerer() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.status IN ('answered', 'answered_in_session') AND OLD.status IS DISTINCT FROM NEW.status THEN
        NEW.answered_by_user := nullif(current_setting('muster.uid', true), '')::bigint;
    END IF;
    RETURN NEW;
END
$$;
DROP TRIGGER IF EXISTS stamp_answerer ON public.attention_asks;
CREATE TRIGGER stamp_answerer BEFORE UPDATE OF status ON public.attention_asks
    FOR EACH ROW EXECUTE FUNCTION public.muster_stamp_answerer();

CREATE OR REPLACE FUNCTION public.muster_stamp_decider() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.status IN ('approved', 'rejected') AND OLD.status IS DISTINCT FROM NEW.status THEN
        NEW.decided_by_user := nullif(current_setting('muster.uid', true), '')::bigint;
    END IF;
    RETURN NEW;
END
$$;
DROP TRIGGER IF EXISTS stamp_decider ON public.plugin_reply_drafts;
CREATE TRIGGER stamp_decider BEFORE UPDATE OF status ON public.plugin_reply_drafts
    FOR EACH ROW EXECUTE FUNCTION public.muster_stamp_decider();

-- ---------------------------------------------------------------------------
-- 5. The rollback, as a function the header's block calls (one statement per
--    line there; this keeps the per-table loop in one place).
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.muster_team_access_rollback() RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    t text;
BEGIN
    FOR t IN SELECT DISTINCT tablename FROM pg_policies WHERE schemaname = 'public' AND policyname = 'team_read' LOOP
        EXECUTE format('DROP POLICY IF EXISTS team_read ON public.%I', t);
        EXECUTE format('DROP POLICY IF EXISTS team_insert ON public.%I', t);
        EXECUTE format('DROP POLICY IF EXISTS team_update ON public.%I', t);
        EXECUTE format('DROP POLICY IF EXISTS team_delete ON public.%I', t);
        EXECUTE format('DROP TRIGGER IF EXISTS owner_fixed ON public.%I', t);
        EXECUTE format($p$
            CREATE POLICY owner_isolation ON public.%I
                AS PERMISSIVE FOR ALL TO PUBLIC
                USING (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )
                WITH CHECK (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )$p$, t);
    END LOOP;
    DROP POLICY IF EXISTS self_read ON public.audit_log;
    CREATE POLICY self_read ON public.audit_log AS PERMISSIVE FOR SELECT TO PUBLIC
        USING (
            user_id = nullif(current_setting('muster.uid', true), '')::bigint
            OR current_setting('muster.scope', true) = 'all'
        );
    DROP TRIGGER IF EXISTS stamp_answerer ON public.attention_asks;
    DROP TRIGGER IF EXISTS stamp_decider ON public.plugin_reply_drafts;
    DROP FUNCTION IF EXISTS public.muster_stamp_answerer();
    DROP FUNCTION IF EXISTS public.muster_stamp_decider();
    DROP FUNCTION IF EXISTS public.muster_owner_fixed();
    DROP FUNCTION IF EXISTS public.muster_team_people(bigint);
    DROP FUNCTION IF EXISTS public.muster_team_role(bigint);
    DROP FUNCTION IF EXISTS public.muster_team_ids(integer);
    DROP INDEX IF EXISTS public.idx_audit_log_team;
    ALTER TABLE public.audit_log DROP COLUMN IF EXISTS team_id;
    ALTER TABLE public.send_queue DROP COLUMN IF EXISTS queued_by;
    ALTER TABLE public.note_comments DROP COLUMN IF EXISTS author_id;
    ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS answered_by_user;
    ALTER TABLE public.plugin_reply_drafts DROP COLUMN IF EXISTS decided_by_user;
END
$$;
