-- NUMBER: 0160 (open pull requests hold 0156–0159; renumber at merge if main
-- moved, with db.teamPagesMigrationVersion).
-- muster 0160 — A TEAM'S PAGES ARE SEEN BY THE TEAM'S MEMBERS
-- (internal/pages; docs/pages.md "A team's pages", docs/teams.md).
--
-- A page a team-owned agent makes belongs to the team (pages.owner_id is the
-- team's users row). Until now the four pages tables were personal
-- (owner_isolation), so no member could read one. The operator's rules:
--   - every member of the team (viewer and up) sees the team's pages, opens
--     the owner view and answers its questions and forms ("view + respond");
--   - only the page's CREATOR — the person who started the team agent that
--     made it — deletes it, changes its current version, revokes its links or
--     makes a new one;
--   - any member makes the page's link public by tapping Share, and a team
--     agent never makes one live by itself (enforced in the application:
--     internal/api page_tools.go pageAgentShareAllowed).
--
--   pages.created_by_user        the creator, for a TEAM's page: the person
--       whose say-so started the agent (the chief dispatch's
--       requested_by_user). NULL on a person's own page (the guard below
--       blanks it: the owner is the creator), on a team page whose agent's
--       starter is not recorded, and on every page made before this. It never
--       changes once written (outside the system scope).
--   page_responses.responder_user   WHO sent an owner-view response
--       (by_owner): the signed-in person, forced to muster.uid by the guard —
--       never what the statement wrote. NULL for a response through a link
--       (a stranger has no account). Backfilled to the owner for every owner
--       response made before this (only the owner could send one then).
--
-- THE POLICIES (one per command; never one FOR ALL, whose USING would also
-- be DELETE's). The read arm is 0127's team arm at viewer:
--   owner_id = muster.uid  OR  owner_id = ANY (muster_team_ids(1))  OR  scope all
--   pages          read: member.  insert: owner.  update: member, narrowed by
--                  pages_team_guard.  delete: muster_page_manager.
--   page_versions  read: member.  insert, update, delete: owner.
--   page_links     read: member (a member's Share reveals the page's link).
--                  insert, update: member, narrowed by page_links_team_guard.
--                  delete: owner.
--   page_responses read: member.  insert: owner, or a member's OWN owner-view
--                  response (by_owner).  update, delete: owner.
-- "owner" here is the user arm: the team acting principal (the team's agent)
-- for a team's page, the person for their own.
--
-- muster_page_manager(owner, creator): whether the caller may MANAGE a page —
-- delete it, change its version, revoke a link, make a new one. The owner
-- (the team's agent; a person on their own page), the scope all, the
-- recorded creator while they are a member of the team, and — ONLY when the
-- page records no creator — a MANAGER of the team (else such a page would be
-- undeletable by anyone). internal/pages MayManage is the same rule in Go,
-- for what the screen draws and the routes refuse.
--
-- 🔴 THE GUARDS (BEFORE triggers, the application role only — the login role,
-- a migration or a restore is not held to them, as row-level security does
-- not hold it):
--   pages_team_guard        a non-manager's UPDATE may change updated_at and
--                           nothing else (a member's response touches it);
--                           created_by_user never changes; on INSERT a
--                           person's own page gets no created_by_user.
--   page_links_team_guard   a non-manager may INSERT only a live link no agent
--                           made and no card decides (Share), and UPDATE only
--                           a pending link to live or denied (an agent's
--                           share card, answered) — never revoke one.
--   page_responses_responder  responder_user is the caller on an owner-view
--                           response, NULL on a link's; never changed.
-- And 0127's owner_fixed on all four: owner_id never changes outside the
-- system scope.
--
-- db.RLSOn re-applies this file after 0116 (which re-creates owner_isolation
-- on the four tables); its body drops it again. Every statement is
-- idempotent. rollback/rls_off.sql turns the tables' security off and leaves
-- these policies inert, as it does 0116's.
--
-- Must apply after its predecessor (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image reads neither
-- column and meets owner_isolation again):
--   DO $$ DECLARE t text; BEGIN
--     FOREACH t IN ARRAY ARRAY['pages','page_versions','page_links','page_responses'] LOOP
--       EXECUTE format('DROP POLICY IF EXISTS team_read ON public.%I', t);
--       EXECUTE format('DROP POLICY IF EXISTS page_insert ON public.%I', t);
--       EXECUTE format('DROP POLICY IF EXISTS page_update ON public.%I', t);
--       EXECUTE format('DROP POLICY IF EXISTS page_delete ON public.%I', t);
--       EXECUTE format('DROP TRIGGER IF EXISTS owner_fixed ON public.%I', t);
--       EXECUTE format($p$CREATE POLICY owner_isolation ON public.%I AS PERMISSIVE FOR ALL TO PUBLIC
--         USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint OR current_setting('muster.scope', true) = 'all')
--         WITH CHECK (owner_id = nullif(current_setting('muster.uid', true), '')::bigint OR current_setting('muster.scope', true) = 'all')$p$, t);
--     END LOOP; END $$;
--   DROP TRIGGER IF EXISTS pages_team_guard ON public.pages;
--   DROP TRIGGER IF EXISTS page_links_team_guard ON public.page_links;
--   DROP TRIGGER IF EXISTS page_responses_responder ON public.page_responses;
--   DROP FUNCTION IF EXISTS public.muster_pages_team_guard();
--   DROP FUNCTION IF EXISTS public.muster_page_links_team_guard();
--   DROP FUNCTION IF EXISTS public.muster_page_responder();
--   DROP FUNCTION IF EXISTS public.muster_page_manager(bigint, bigint);
--   ALTER TABLE public.page_responses DROP COLUMN IF EXISTS responder_user;
--   ALTER TABLE public.pages DROP COLUMN IF EXISTS created_by_user;
--   DELETE FROM schema_migrations WHERE version = 160;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.pages ADD COLUMN IF NOT EXISTS created_by_user BIGINT;
ALTER TABLE public.page_responses ADD COLUMN IF NOT EXISTS responder_user BIGINT;

-- Every owner response before this was the owner's own.
UPDATE public.page_responses SET responder_user = owner_id WHERE by_owner AND responder_user IS NULL;

-- ---------------------------------------------------------------------------
-- 1. Who may manage a page.
-- ---------------------------------------------------------------------------

-- coalesce(…, false) WHOLE: a NULL setting must read as "no", never as a
-- NULL that an IF NOT would let through.
CREATE OR REPLACE FUNCTION public.muster_page_manager(owner bigint, creator bigint) RETURNS boolean
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog
AS $$
    SELECT coalesce(
        current_setting('muster.scope', true) = 'all'
        OR owner = nullif(current_setting('muster.uid', true), '')::bigint
        OR (creator IS NOT NULL
            AND creator = nullif(current_setting('muster.uid', true), '')::bigint
            AND owner = ANY (public.muster_team_ids(1)))
        OR (creator IS NULL AND owner = ANY (public.muster_team_ids(3))),
        false)
$$;

-- ---------------------------------------------------------------------------
-- 2. The guards.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.muster_pages_team_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF current_user <> 'muster_app' OR current_setting('muster.scope', true) IS NOT DISTINCT FROM 'all' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        -- A person's own page has no creator column: its owner is its creator.
        IF NOT public.muster_owner_is_team(NEW.owner_id) THEN
            NEW.created_by_user := NULL;
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.created_by_user IS DISTINCT FROM OLD.created_by_user THEN
        RAISE EXCEPTION 'pages.created_by_user never changes (page %)', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT public.muster_page_manager(OLD.owner_id, OLD.created_by_user)
       AND (to_jsonb(NEW) - 'updated_at') IS DISTINCT FROM (to_jsonb(OLD) - 'updated_at') THEN
        RAISE EXCEPTION 'only the creator of team page % changes it', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS pages_team_guard ON public.pages;
CREATE TRIGGER pages_team_guard BEFORE INSERT OR UPDATE ON public.pages
    FOR EACH ROW EXECUTE FUNCTION public.muster_pages_team_guard();

CREATE OR REPLACE FUNCTION public.muster_page_links_team_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    creator bigint;
BEGIN
    IF current_user <> 'muster_app' OR current_setting('muster.scope', true) IS NOT DISTINCT FROM 'all' THEN
        RETURN NEW;
    END IF;
    -- The page's creator, read as the caller (a member reads the team's page).
    SELECT p.created_by_user INTO creator FROM public.pages p WHERE p.id = NEW.page_id AND p.owner_id = NEW.owner_id;
    IF public.muster_page_manager(NEW.owner_id, creator) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        -- A member's Share: a live link, made by a person, decided by no card.
        IF NEW.state = 'live' AND NOT NEW.made_by_agent AND NEW.ask_id IS NULL THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'a member makes only a live link on team page % (Share)', NEW.page_id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    -- A member's answer to the team agent's share card: pending -> live or
    -- denied, and nothing else about the row.
    IF OLD.state = 'pending' AND NEW.state IN ('live', 'denied')
       AND (to_jsonb(NEW) - 'state' - 'decided_at') = (to_jsonb(OLD) - 'state' - 'decided_at') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'only the creator of team page % revokes or changes its links', NEW.page_id
        USING ERRCODE = 'insufficient_privilege';
END
$$;

DROP TRIGGER IF EXISTS page_links_team_guard ON public.page_links;
CREATE TRIGGER page_links_team_guard BEFORE INSERT OR UPDATE ON public.page_links
    FOR EACH ROW EXECUTE FUNCTION public.muster_page_links_team_guard();

CREATE OR REPLACE FUNCTION public.muster_page_responder() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF current_user <> 'muster_app' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        NEW.responder_user := CASE WHEN NEW.by_owner
            THEN nullif(current_setting('muster.uid', true), '')::bigint END;
        RETURN NEW;
    END IF;
    IF NEW.responder_user IS DISTINCT FROM OLD.responder_user THEN
        RAISE EXCEPTION 'page_responses.responder_user never changes (response %)', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS page_responses_responder ON public.page_responses;
CREATE TRIGGER page_responses_responder BEFORE INSERT OR UPDATE ON public.page_responses
    FOR EACH ROW EXECUTE FUNCTION public.muster_page_responder();

-- ---------------------------------------------------------------------------
-- 3. The policies.
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    t   text;
    usr constant text := $a$owner_id = nullif(current_setting('muster.uid', true), '')::bigint$a$;
    mem constant text := $a$owner_id = ANY (coalesce((SELECT public.muster_team_ids(1)), '{}'))$a$;
    sys constant text := $a$current_setting('muster.scope', true) = 'all'$a$;
    own text;
    shr text;
BEGIN
    own := usr || ' OR ' || sys;
    shr := usr || ' OR ' || mem || ' OR ' || sys;
    FOREACH t IN ARRAY ARRAY['pages', 'page_versions', 'page_links', 'page_responses'] LOOP
        EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', t);
        EXECUTE format('DROP POLICY IF EXISTS team_read ON public.%I', t);
        EXECUTE format('DROP POLICY IF EXISTS page_insert ON public.%I', t);
        EXECUTE format('DROP POLICY IF EXISTS page_update ON public.%I', t);
        EXECUTE format('DROP POLICY IF EXISTS page_delete ON public.%I', t);
        EXECUTE format('CREATE POLICY team_read ON public.%I AS PERMISSIVE FOR SELECT TO PUBLIC USING (%s)', t, shr);
        EXECUTE format('DROP TRIGGER IF EXISTS owner_fixed ON public.%I', t);
        EXECUTE format('CREATE TRIGGER owner_fixed BEFORE UPDATE OF owner_id ON public.%I
            FOR EACH ROW EXECUTE FUNCTION public.muster_owner_fixed()', t);
    END LOOP;

    -- pages
    EXECUTE format('CREATE POLICY page_insert ON public.pages AS PERMISSIVE FOR INSERT TO PUBLIC WITH CHECK (%s)', own);
    EXECUTE format('CREATE POLICY page_update ON public.pages AS PERMISSIVE FOR UPDATE TO PUBLIC USING (%s) WITH CHECK (%s)', shr, shr);
    EXECUTE 'CREATE POLICY page_delete ON public.pages AS PERMISSIVE FOR DELETE TO PUBLIC
        USING (public.muster_page_manager(owner_id, created_by_user))';

    -- page_versions: written only by the page's owner (its agent).
    EXECUTE format('CREATE POLICY page_insert ON public.page_versions AS PERMISSIVE FOR INSERT TO PUBLIC WITH CHECK (%s)', own);
    EXECUTE format('CREATE POLICY page_update ON public.page_versions AS PERMISSIVE FOR UPDATE TO PUBLIC USING (%s) WITH CHECK (%s)', own, own);
    EXECUTE format('CREATE POLICY page_delete ON public.page_versions AS PERMISSIVE FOR DELETE TO PUBLIC USING (%s)', own);

    -- page_links: a member's Share and card answer, narrowed by the guard.
    EXECUTE format('CREATE POLICY page_insert ON public.page_links AS PERMISSIVE FOR INSERT TO PUBLIC WITH CHECK (%s)', shr);
    EXECUTE format('CREATE POLICY page_update ON public.page_links AS PERMISSIVE FOR UPDATE TO PUBLIC USING (%s) WITH CHECK (%s)', shr, shr);
    EXECUTE format('CREATE POLICY page_delete ON public.page_links AS PERMISSIVE FOR DELETE TO PUBLIC USING (%s)', own);

    -- page_responses: a member adds only their own owner-view response.
    EXECUTE format('CREATE POLICY page_insert ON public.page_responses AS PERMISSIVE FOR INSERT TO PUBLIC WITH CHECK (%s OR (by_owner AND %s))', own, mem);
    EXECUTE format('CREATE POLICY page_update ON public.page_responses AS PERMISSIVE FOR UPDATE TO PUBLIC USING (%s) WITH CHECK (%s)', own, own);
    EXECUTE format('CREATE POLICY page_delete ON public.page_responses AS PERMISSIVE FOR DELETE TO PUBLIC USING (%s)', own);
END
$$;
