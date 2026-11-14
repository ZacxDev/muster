-- muster 0130 — TEAM OFFICERS (docs/design/team-sharing.md, phase 3).
--
-- Phase 2a (0127) already put every officer table in its share class
-- (officer_items, officer_pr_loops shared; officer_types, officer_defs,
-- officer_schedules and the plugin tables managed; the usage tables usage).
-- This migration adds what a TEAM officer needs beyond that:
--
--   plugin_reply_drafts.requested_by_user
--       WHO CAUSED A DRAFT: the acting principal's muster.uid when the row is
--       filed (a person's routed session, a person's chief — or, for a team
--       officer's draft, the team itself), forced by muster_actor_column as
--       0127's queued_by is: never written by hand.
--
--   muster_draft_approver (BEFORE UPDATE on plugin_reply_drafts)
--       🔴 NOBODY APPROVES THEIR OWN TEAM DRAFT. For the application role
--       outside the system scope:
--         - a TEAM never approves a draft it owns, by an update that moves it
--           to approved: the team acting principal is a team officer's
--           instance (or the team chief) acting as the team, and its drafts
--           are its MANAGERS' to approve;
--         - a person never approves a draft owned by SOMEONE ELSE (a team)
--           that they caused (requested_by_user = them).
--       A personal draft (owner = the approving person) is unchanged: the
--       owner's own sessions are the owner (operator decision, #649).
--       ONLY A DECISION (an UPDATE) is held to it. A draft FILED approved is
--       a standing approval its owner set (a route's sender allowlist,
--       plugins.RelayRoute), not the filer approving itself — round 0 of #668
--       (D2): a team route relayed as the team under a manager's allowlist
--       must not be refused here.
--       Refused 42501 with HINT 'muster:draft-approver', which internal/api
--       maps to its refusal text.
--
--   officer_host_spawns.shared_team_id
--       🔴 PERSONAL BY DEFAULT, LENT ON REQUEST (operator decision). The row
--       stays its owner's (owner_isolation, unchanged: only the owner writes
--       it). Its owner may LEND it to one team they are an OPERATOR or
--       MANAGER of: the team's members at viewer and above then READ it
--       (team_lent_read, a SELECT policy on the person doors that carry
--       muster.teams), and never write it. Unlending sets it back to NULL.
--       muster_lend_target refuses, for the application role outside the
--       system scope, a lend to a team the acting person is not an operator
--       or manager of, or of a row that is not theirs — read from
--       team_members itself (its self_read admits the person's own rows), so
--       the check holds at EVERY door, whatever muster.teams says.
--       internal/users clears a person's lends to a team when they leave it
--       or drop to viewer.
--
-- db.RLSOn re-applies this file (after 0125, which re-creates
-- owner_isolation and does not touch team_lent_read). Idempotent.
--
-- Must apply after its predecessor, 0129 (#648's attention_item_context; the
-- contiguity test refuses a gap).
-- 🔴 RENUMBER IT (and db.teamOfficersMigrationVersion) if another migration
-- takes 0130 first.
--
-- ROLLBACK (after rolling the image back; an older image never reads the
-- columns):
--   SELECT public.muster_team_officers_rollback();
--   DROP FUNCTION IF EXISTS public.muster_team_officers_rollback();
--   DELETE FROM schema_migrations WHERE version = 130;

SET LOCAL lock_timeout = '3s';

-- ---------------------------------------------------------------------------
-- 1. Who caused a draft.
-- ---------------------------------------------------------------------------

ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS requested_by_user BIGINT;
ALTER TABLE public.plugin_reply_drafts ALTER COLUMN requested_by_user
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
DROP TRIGGER IF EXISTS actor_requested_by_user ON public.plugin_reply_drafts;
CREATE TRIGGER actor_requested_by_user BEFORE INSERT OR UPDATE ON public.plugin_reply_drafts
    FOR EACH ROW EXECUTE FUNCTION public.muster_actor_column('requested_by_user', '');

-- ---------------------------------------------------------------------------
-- 2. Nobody approves their own team draft.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.muster_draft_approver() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    actor bigint := nullif(current_setting('muster.uid', true), '')::bigint;
BEGIN
    IF current_user <> 'muster_app' OR current_setting('muster.scope', true) IS NOT DISTINCT FROM 'all'
       OR NEW.status IS DISTINCT FROM 'approved'
       OR OLD.status IS NOT DISTINCT FROM 'approved' THEN
        RETURN NEW;
    END IF;
    -- users is self_read: the actor reads its OWN row, so a team acting as
    -- itself sees that it is a team.
    IF NEW.owner_id = actor AND EXISTS (SELECT 1 FROM public.users WHERE id = actor AND kind = 'team') THEN
        RAISE EXCEPTION 'draft %: a team never approves its own draft; one of its managers does', NEW.id
            USING ERRCODE = 'insufficient_privilege', HINT = 'muster:draft-approver';
    END IF;
    IF NEW.owner_id IS DISTINCT FROM actor AND NEW.requested_by_user = actor THEN
        RAISE EXCEPTION 'draft %: you caused it, so another manager of its team approves it', NEW.id
            USING ERRCODE = 'insufficient_privilege', HINT = 'muster:draft-approver';
    END IF;
    RETURN NEW;
END
$$;
DROP TRIGGER IF EXISTS draft_approver ON public.plugin_reply_drafts;
CREATE TRIGGER draft_approver BEFORE UPDATE ON public.plugin_reply_drafts
    FOR EACH ROW EXECUTE FUNCTION public.muster_draft_approver();

-- ---------------------------------------------------------------------------
-- 3. A host spawn lent to a team.
-- ---------------------------------------------------------------------------

ALTER TABLE public.officer_host_spawns ADD COLUMN IF NOT EXISTS shared_team_id BIGINT REFERENCES public.users (id);
CREATE INDEX IF NOT EXISTS officer_host_spawns_shared ON public.officer_host_spawns (shared_team_id, id DESC)
    WHERE shared_team_id IS NOT NULL;

DROP POLICY IF EXISTS team_lent_read ON public.officer_host_spawns;
CREATE POLICY team_lent_read ON public.officer_host_spawns AS PERMISSIVE FOR SELECT TO PUBLIC
    USING (shared_team_id = ANY (coalesce((SELECT public.muster_team_ids(1)), '{}')));

CREATE OR REPLACE FUNCTION public.muster_lend_target() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    actor bigint := nullif(current_setting('muster.uid', true), '')::bigint;
BEGIN
    IF current_user <> 'muster_app' OR current_setting('muster.scope', true) IS NOT DISTINCT FROM 'all'
       OR NEW.shared_team_id IS NULL
       OR (TG_OP = 'UPDATE' AND NEW.shared_team_id IS NOT DISTINCT FROM OLD.shared_team_id) THEN
        RETURN NEW;
    END IF;
    IF NEW.owner_id IS DISTINCT FROM actor OR NOT EXISTS (SELECT 1 FROM public.team_members
            WHERE team_id = NEW.shared_team_id AND user_id = actor AND role IN ('operator', 'manager')) THEN
        RAISE EXCEPTION 'officer_host_spawns %: only its owner lends it, and only to a team they operate (team %)',
            NEW.id, NEW.shared_team_id
            USING ERRCODE = 'insufficient_privilege', HINT = 'muster:lend-target';
    END IF;
    RETURN NEW;
END
$$;
DROP TRIGGER IF EXISTS lend_target ON public.officer_host_spawns;
CREATE TRIGGER lend_target BEFORE INSERT OR UPDATE OF shared_team_id ON public.officer_host_spawns
    FOR EACH ROW EXECUTE FUNCTION public.muster_lend_target();

-- ---------------------------------------------------------------------------
-- 4. The rollback.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.muster_team_officers_rollback() RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    DROP TRIGGER IF EXISTS lend_target ON public.officer_host_spawns;
    DROP FUNCTION IF EXISTS public.muster_lend_target();
    DROP POLICY IF EXISTS team_lent_read ON public.officer_host_spawns;
    DROP INDEX IF EXISTS public.officer_host_spawns_shared;
    ALTER TABLE public.officer_host_spawns DROP COLUMN IF EXISTS shared_team_id;
    DROP TRIGGER IF EXISTS draft_approver ON public.plugin_reply_drafts;
    DROP FUNCTION IF EXISTS public.muster_draft_approver();
    DROP TRIGGER IF EXISTS actor_requested_by_user ON public.plugin_reply_drafts;
    ALTER TABLE public.plugin_reply_drafts DROP COLUMN IF EXISTS requested_by_user;
END
$$;
