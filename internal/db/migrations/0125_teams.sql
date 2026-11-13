-- muster 0125 — TEAMS AND MEMBERSHIP, NOTHING SHARED YET
-- (docs/design/team-sharing.md, phase 1; internal/users teams.go).
--   users.kind     'person' (every row before this migration, and every row
--                  an invite or the claim creates) or 'team'. A team is an
--                  OWNER that no one can sign in as.
--   team_members   (team_id, user_id) -> role viewer | operator | manager,
--                  who added it and when.
--
-- 🔴 A TEAM NEVER SIGNS IN, AND THE SCHEMA SAYS SO (users_team_no_login): a
-- team row has no GoTrue subject, no email, role 'member' and is never
-- disabled. Every sign-in path finds its user by subject or email, so no
-- GoTrue identity can be bound to a team (the admin claim's UPDATE of
-- gotrue_sub is refused here), and a team can never be an admin or count
-- toward the last-admin guard. The application refuses a team principal at
-- every door as well (internal/api admit; authz.WithPrincipal).
--
-- 🔴 kind NEVER CHANGES (users_kind_fixed): a person cannot become a team,
-- which would hand every row they own to whoever joins it, and a team cannot
-- become a person, which would make it a login.
--
-- 🔴 NOTHING IS SHARED IN THIS PHASE. No owner_isolation policy changes: a
-- row a team owns is visible only to a principal whose muster.uid IS the team,
-- and no request may act as one. Membership is read by internal/users
-- (Account) into the request principal and is used for nothing else yet.
--
-- team_members is USER-KEYED, not owned (no owner_id): it is under FORCED
-- row-level security with two policies, in 0009's shape:
--   self_read  a person reads the rows that name THEM (FOR SELECT);
--   scope_all  every read and write at muster.scope = 'all'.
-- Every write goes through internal/users under authz.System, from the admin
-- pages (/admin/teams), whose admin-role check is the gate.
-- rollback/rls_off.sql does not touch it and db.RLSOn does not re-apply it:
-- they turn the OWNED tables' security off and on.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). The
-- number was the next free one on main when written; it may need renumbering
-- at merge. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads either):
--   DROP TABLE IF EXISTS public.team_members;
--   DROP FUNCTION IF EXISTS public.muster_team_members_kinds();
--   DELETE FROM public.users WHERE kind = 'team';
--   DROP TRIGGER IF EXISTS users_kind_fixed ON public.users;
--   DROP FUNCTION IF EXISTS public.muster_users_kind_fixed();
--   DROP INDEX IF EXISTS public.users_team_name_key;
--   ALTER TABLE public.users DROP CONSTRAINT IF EXISTS users_team_no_login;
--   ALTER TABLE public.users DROP COLUMN IF EXISTS kind;
--   DELETE FROM schema_migrations WHERE version = 125;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.users ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'person';

ALTER TABLE public.users DROP CONSTRAINT IF EXISTS users_kind_check;
ALTER TABLE public.users ADD CONSTRAINT users_kind_check CHECK (kind IN ('person', 'team'));

ALTER TABLE public.users DROP CONSTRAINT IF EXISTS users_team_no_login;
ALTER TABLE public.users ADD CONSTRAINT users_team_no_login CHECK (
    kind = 'person'
    OR (gotrue_sub IS NULL AND email IS NULL AND role = 'member' AND disabled_at IS NULL
        AND char_length(btrim(display_name)) BETWEEN 1 AND 64)
);

-- A team's name is how an admin tells two teams apart: unique among teams,
-- case-insensitively. A person's display_name is not constrained.
CREATE UNIQUE INDEX IF NOT EXISTS users_team_name_key
    ON public.users (lower(btrim(display_name))) WHERE kind = 'team';

CREATE OR REPLACE FUNCTION public.muster_users_kind_fixed() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.kind IS DISTINCT FROM OLD.kind THEN
        RAISE EXCEPTION 'users.kind never changes (user %: % -> %)', OLD.id, OLD.kind, NEW.kind
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS users_kind_fixed ON public.users;
CREATE TRIGGER users_kind_fixed BEFORE UPDATE OF kind ON public.users
    FOR EACH ROW EXECUTE FUNCTION public.muster_users_kind_fixed();

CREATE TABLE IF NOT EXISTS public.team_members (
    team_id   BIGINT      NOT NULL REFERENCES public.users (id),
    user_id   BIGINT      NOT NULL REFERENCES public.users (id),
    role      TEXT        NOT NULL CHECK (role IN ('viewer', 'operator', 'manager')),
    added_by  BIGINT      NOT NULL REFERENCES public.users (id),
    added_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id),
    CONSTRAINT team_members_not_self CHECK (team_id <> user_id)
);

CREATE INDEX IF NOT EXISTS idx_team_members_user ON public.team_members (user_id);

-- team_id must name a team and user_id a person (a team is never a member of
-- a team). A foreign key cannot say which KIND of user a column names, so a
-- trigger does; it reads users, which only scope 'all' can see whole — the
-- only scope any policy below lets write.
CREATE OR REPLACE FUNCTION public.muster_team_members_kinds() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM public.users WHERE id = NEW.team_id AND kind = 'team') THEN
        RAISE EXCEPTION 'team_members.team_id % is not a team', NEW.team_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.users WHERE id = NEW.user_id AND kind = 'person') THEN
        RAISE EXCEPTION 'team_members.user_id % is not a person', NEW.user_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS team_members_kinds ON public.team_members;
CREATE TRIGGER team_members_kinds BEFORE INSERT OR UPDATE OF team_id, user_id ON public.team_members
    FOR EACH ROW EXECUTE FUNCTION public.muster_team_members_kinds();

ALTER TABLE public.team_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.team_members FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS self_read ON public.team_members;
CREATE POLICY self_read ON public.team_members AS PERMISSIVE FOR SELECT TO PUBLIC
    USING (user_id = nullif(current_setting('muster.uid', true), '')::bigint);
DROP POLICY IF EXISTS scope_all ON public.team_members;
CREATE POLICY scope_all ON public.team_members AS PERMISSIVE FOR ALL TO PUBLIC
    USING (current_setting('muster.scope', true) = 'all')
    WITH CHECK (current_setting('muster.scope', true) = 'all');

GRANT SELECT, INSERT, UPDATE, DELETE ON public.team_members TO muster_app;
