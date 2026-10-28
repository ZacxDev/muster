-- muster 0060 — THE CHIEF'S PER-USER ON/OFF TOGGLE (internal/chiefpref).
--
-- One row per owner who has turned their chief ON or OFF on the Account page.
-- No row means the owner never chose, and then the chief is on exactly when
-- the owner already HAS a chief agent (api.chiefOn). So:
--
--   - a fresh install, or a new user, has no chief agent and no row: OFF;
--   - a deployment that already runs a chief keeps it with no data migration:
--     the agent row is the state that turned it on, and it still does.
--
-- 🔴 NO BACKFILL, DELIBERATELY. Writing `enabled = true` here for every owner
-- of a chief agent would freeze today's agent table into the toggle; deriving
-- it at read time keeps the existing rule ("a chief agent exists") the default
-- for everyone who never touches the switch.
--
-- 🔴 OFF DELETES NOTHING. The chief agent, its sessions and its chat history
-- stay; off only stops muster sending it work and hides it from navigation.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0048's suggest_preferences
-- shape: owner_isolation keeps one user from reading or flipping another's
-- toggle. rollback/rls_off.sql turns it off with the rest, and --rls-on
-- re-applies this file (db.RLSOn), so every statement is idempotent.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.chief_preferences (
    owner_id    BIGINT      PRIMARY KEY
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    enabled     BOOLEAN     NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.chief_preferences ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.chief_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.chief_preferences FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.chief_preferences;
CREATE POLICY owner_isolation ON public.chief_preferences
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.chief_preferences TO muster_app;
