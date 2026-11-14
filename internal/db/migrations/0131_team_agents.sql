-- NUMBER: 0131 (0129 is #648's on main; 0130 is held by team sharing phase 3, #668)
-- muster 0131 — TEAM AGENTS ARE VISIBLE TO THE TEAM'S MEMBERS
-- (team sharing phase 4, slice 4.4; docs/design/team-sharing.md).
--
--   agents   joins the SHARED class of 0127: read at viewer, written at
--            operator, the same four per-command team policies and the same
--            owner_fixed trigger (owner_id never changes outside the system
--            scope). An agent the team owns — the team chief, its workers — is
--            then readable by the team's members through a door where a person
--            is present (the browser session, a paired phone: muster.teams),
--            and by nobody else.
--
-- 🔴 A TEAM-OWNED AGENT'S CREDENTIALS NEVER SIT IN ITS ROW. 0128 moved them to
-- agent_secrets (owner-isolated, and it stays so: a member never reads it) but
-- kept DUAL-WRITING agents.hooks_token / agents.gateway_secret for an older
-- image. Shared, that row would hand every viewer the team chief's identity
-- and turn credential. So:
--   muster_owner_is_team(owner)  SECURITY DEFINER, scope all for its own
--                                duration: whether a users id is a team. One
--                                boolean about one id; it must see the row
--                                whatever principal writes the agent (a
--                                principal that cannot read the team's users
--                                row would otherwise read "person" and keep
--                                the dual-write — fail OPEN).
--   agents_team_no_secret        BEFORE INSERT OR UPDATE OF hooks_token,
--                                gateway_secret, owner_id ON agents: for a
--                                team owner the two columns are FORCED to ''.
--                                Every writer — Create, SetHooksToken,
--                                SetGatewaySecret, an older image, a hand
--                                UPDATE by the application role — meets it;
--                                the credential lands only in agent_secrets
--                                (those statements write it there from their
--                                own parameters, not from the row).
-- A person's agent is unchanged: still dual-written, so an older image runs.
-- The backfill below first copies any team agent's credential the old columns
-- still hold into agent_secrets (0128's rule: fill only what is missing), then
-- blanks the columns.
--
-- db.RLSOn re-applies this file after 0007 (which re-creates owner_isolation
-- on agents) and 0127. Idempotent. rollback/rls_off.sql turns agents' security
-- off with the other owned tables and leaves these policies inert.
--
-- 🔴 NUMBERED 0131 WHILE 0130 IS UNMERGED (#668): until #668 lands this tree
-- has a gap at 0130 and the contiguity test is red here, by design. Renumber
-- with db.teamAgentsMigrationVersion if the number moves.
--
-- ROLLBACK (after rolling the image back; an older image reads agents under
-- owner_isolation and never meets the team policies). A team agent's
-- credentials are not restored to the old columns: no team agent exists
-- before slice 4.5, and agent_secrets still holds them.
--   DROP POLICY IF EXISTS team_read ON public.agents;
--   DROP POLICY IF EXISTS team_insert ON public.agents;
--   DROP POLICY IF EXISTS team_update ON public.agents;
--   DROP POLICY IF EXISTS team_delete ON public.agents;
--   DROP TRIGGER IF EXISTS owner_fixed ON public.agents;
--   CREATE POLICY owner_isolation ON public.agents AS PERMISSIVE FOR ALL TO PUBLIC USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint OR current_setting('muster.scope', true) = 'all') WITH CHECK (owner_id = nullif(current_setting('muster.uid', true), '')::bigint OR current_setting('muster.scope', true) = 'all');
--   DROP TRIGGER IF EXISTS agents_team_no_secret ON public.agents;
--   DROP FUNCTION IF EXISTS public.muster_agents_team_no_secret();
--   DROP FUNCTION IF EXISTS public.muster_owner_is_team(bigint);
--   DELETE FROM schema_migrations WHERE version = 131;

SET LOCAL lock_timeout = '3s';

-- ---------------------------------------------------------------------------
-- 1. A team owner's agent keeps no credential in its row.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.muster_owner_is_team(owner bigint)
    RETURNS boolean
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = pg_catalog
    SET muster.scope = 'all'
AS $$
    SELECT EXISTS (SELECT 1 FROM public.users WHERE id = owner AND kind = 'team')
$$;
-- EXECUTE to PUBLIC, as 0007's muster_agent_name_taken: whether an id is a
-- team is no secret (a member sees their teams' ids on every team page).
GRANT EXECUTE ON FUNCTION public.muster_owner_is_team(bigint) TO PUBLIC;

CREATE OR REPLACE FUNCTION public.muster_agents_team_no_secret() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF public.muster_owner_is_team(NEW.owner_id) THEN
        NEW.hooks_token := '';
        NEW.gateway_secret := '';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS agents_team_no_secret ON public.agents;
CREATE TRIGGER agents_team_no_secret BEFORE INSERT OR UPDATE OF hooks_token, gateway_secret, owner_id ON public.agents
    FOR EACH ROW EXECUTE FUNCTION public.muster_agents_team_no_secret();

-- Backfill: a team agent's credential still in the old columns moves to
-- agent_secrets (only what that table lacks), then the columns are blanked.
INSERT INTO public.agent_secrets AS s (agent_id, owner_id, hooks_token, gateway_secret)
SELECT a.id, a.owner_id, a.hooks_token, a.gateway_secret
  FROM public.agents a JOIN public.users u ON u.id = a.owner_id AND u.kind = 'team'
 WHERE a.hooks_token <> '' OR a.gateway_secret <> ''
ON CONFLICT (agent_id) DO UPDATE SET
    hooks_token    = CASE WHEN s.hooks_token = '' THEN EXCLUDED.hooks_token ELSE s.hooks_token END,
    gateway_secret = CASE WHEN s.gateway_secret = '' THEN EXCLUDED.gateway_secret ELSE s.gateway_secret END;
UPDATE public.agents a SET hooks_token = '', gateway_secret = ''
  FROM public.users u
 WHERE u.id = a.owner_id AND u.kind = 'team' AND (a.hooks_token <> '' OR a.gateway_secret <> '');

-- ---------------------------------------------------------------------------
-- 2. agents joins the shared class (read viewer 1, write operator 2), in
--    0127's exact shape.
-- ---------------------------------------------------------------------------

DROP POLICY IF EXISTS owner_isolation ON public.agents;
DROP POLICY IF EXISTS team_read ON public.agents;
DROP POLICY IF EXISTS team_insert ON public.agents;
DROP POLICY IF EXISTS team_update ON public.agents;
DROP POLICY IF EXISTS team_delete ON public.agents;
CREATE POLICY team_read ON public.agents AS PERMISSIVE FOR SELECT TO PUBLIC
    USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(1)), '{}'))
        OR current_setting('muster.scope', true) = 'all');
CREATE POLICY team_insert ON public.agents AS PERMISSIVE FOR INSERT TO PUBLIC
    WITH CHECK (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(2)), '{}'))
        OR current_setting('muster.scope', true) = 'all');
CREATE POLICY team_update ON public.agents AS PERMISSIVE FOR UPDATE TO PUBLIC
    USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(2)), '{}'))
        OR current_setting('muster.scope', true) = 'all')
    WITH CHECK (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(2)), '{}'))
        OR current_setting('muster.scope', true) = 'all');
CREATE POLICY team_delete ON public.agents AS PERMISSIVE FOR DELETE TO PUBLIC
    USING (owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR owner_id = ANY (coalesce((SELECT public.muster_team_ids(2)), '{}'))
        OR current_setting('muster.scope', true) = 'all');
DROP TRIGGER IF EXISTS owner_fixed ON public.agents;
CREATE TRIGGER owner_fixed BEFORE UPDATE OF owner_id ON public.agents
    FOR EACH ROW EXECUTE FUNCTION public.muster_owner_fixed();
