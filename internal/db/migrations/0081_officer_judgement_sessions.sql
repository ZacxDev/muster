-- muster 0081 — THE JUDGEMENT-TIER DAILY SESSION CAP, PERSISTED
-- (internal/officers sessions.go; docs/officers.md "The two pools").
--
--   officer_judgement_sessions   one row per (owner, officer, UTC day): the
--       item ids the officer's judgement pool claimed that day (a claim-next
--       in flight holds a `pending:<token>` entry until HQ names its item).
--       cardinality(items) is the day's session count; the cap
--       (tiers.judgement.max_sessions_per_day, at most 100) is checked
--       against it.
--
-- 🔴 SHARED BY EVERY REPLICA, ADMITTED UNDER THE ROW LOCK. A session is
-- admitted in one transaction that locks the day's row (SELECT … FOR UPDATE)
-- and decides on its latest version, so two muster-servers side by side in a
-- rollout cannot both admit the N+1th, and a restart resets nothing.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0049's shape: the row's
-- owner is the officer's owner, and owner_isolation keeps one user from
-- reading or writing another's. rollback/rls_off.sql turns it off with the
-- rest, and --rls-on re-applies this file (db.RLSOn), so every statement is
-- idempotent.
--
-- Must apply after 0080 (the contiguity test refuses a gap).
--
-- ROLLBACK. 🔴 FIRST edit MUSTER_OFFICERS: an older image has no session
-- cap at all and refuses to boot on a config that names
-- `max_sessions_per_day` (its tiers parser refuses unknown fields), and it
-- refuses ANY officer with `llm` (the pod route did not exist). So remove
-- both from every officer, THEN roll the image back, then (optionally):
--   DROP TABLE public.officer_judgement_sessions;
--   DELETE FROM schema_migrations WHERE version = 81;
-- An older image never reads the table, so leaving it is harmless.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_judgement_sessions (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id   BIGINT      NOT NULL
               DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
               REFERENCES public.users (id),
    officer    TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    day        DATE        NOT NULL,
    items      TEXT[]      NOT NULL DEFAULT '{}' CHECK (cardinality(items) <= 100),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT officer_judgement_sessions_one_row UNIQUE (owner_id, officer, day)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.officer_judgement_sessions ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.officer_judgement_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.officer_judgement_sessions FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.officer_judgement_sessions;
CREATE POLICY owner_isolation ON public.officer_judgement_sessions
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.officer_judgement_sessions TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.officer_judgement_sessions_id_seq TO muster_app;
