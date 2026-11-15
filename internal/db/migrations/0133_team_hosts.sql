-- muster 0133 — TEAM HOSTS (docs/design/team-sharing.md, "Phase 2b design:
-- team hosts", part D). One migration for all of phase 2b, landed with its
-- first slice (2b.1) ahead of the slices that use it. Each change is inert
-- without its code: the two columns are defaulted or nullable, and the policy
-- is SELECT-only.
--
--   team_members.notify            (used by 2b.1)
--       THE MEMBER'S OWN SWITCH: whether a team's pushes reach this member.
--       On by default. Toggled by the member on Account -> Teams
--       (users.PGStore.SetTeamNotify, under System, only the caller's own
--       row); a manager does not set it for them. Off means no push from
--       that team at all; the team's items stay on Attention.
--
--   mirror_tokens.rotate_at        (used by 2b.2)
--       Set when an operator or a manager of the token's team leaves it,
--       drops to viewer or is disabled (operator decision D-1113 #6: mark
--       rotate, never auto-revoke). Nothing clears it: a token marked rotate
--       stays marked until it is revoked. There is NO reason column: the page
--       derives "ben left" from the team's audit rows.
--
--   officer_host_spawns policy team_owner_read   (used by 2b.5)
--       A TEAM-owned spawn row is readable by the team's members at viewer
--       and above, through the doors that carry muster.teams (the browser
--       session, a paired phone). SELECT only: nobody but the team acting
--       principal writes it (owner_isolation), and lend_target (0130)
--       already refuses lending a row its actor does not own.
--
-- db.RLSOn re-applies this file (after 0125 and 0130), keyed on
-- team_members.notify. Every statement is idempotent.
--
-- Must apply after its predecessor, 0132 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT (and db.teamHostsMigrationVersion) if another migration
-- takes 0133 first. 0134 is reserved for phase 4.7.
--
-- 🔴 ROLLBACK ORDER. Once 2b.2 is live, removing a member writes rotate_at,
-- so this migration is rolled back only after 2b.5's and then 2b.2's code
-- have been rolled back. Rolling back 2b.1's CODE alone needs nothing here.
--
-- ROLLBACK (after rolling the image back; an older image never reads the
-- columns or relies on the policy):
--   SELECT public.muster_team_hosts_rollback();
--   DROP FUNCTION IF EXISTS public.muster_team_hosts_rollback();
--   DELETE FROM schema_migrations WHERE version = 133;

SET LOCAL lock_timeout = '3s';

-- ---------------------------------------------------------------------------
-- 1. A member's notify switch.
-- ---------------------------------------------------------------------------

ALTER TABLE public.team_members ADD COLUMN IF NOT EXISTS notify BOOLEAN NOT NULL DEFAULT true;

-- ---------------------------------------------------------------------------
-- 2. A team host token marked rotate.
-- ---------------------------------------------------------------------------

ALTER TABLE public.mirror_tokens ADD COLUMN IF NOT EXISTS rotate_at TIMESTAMPTZ;

-- ---------------------------------------------------------------------------
-- 3. A team's own spawn rows, read by its members.
-- ---------------------------------------------------------------------------

DROP POLICY IF EXISTS team_owner_read ON public.officer_host_spawns;
CREATE POLICY team_owner_read ON public.officer_host_spawns AS PERMISSIVE FOR SELECT TO PUBLIC
    USING (owner_id = ANY (coalesce((SELECT public.muster_team_ids(1)), '{}')));

-- ---------------------------------------------------------------------------
-- 4. The rollback.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.muster_team_hosts_rollback() RETURNS void
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    DROP POLICY IF EXISTS team_owner_read ON public.officer_host_spawns;
    ALTER TABLE public.mirror_tokens DROP COLUMN IF EXISTS rotate_at;
    ALTER TABLE public.team_members DROP COLUMN IF EXISTS notify;
END
$$;
