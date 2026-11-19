-- muster 0157 — AN OWNER'S "RAISE CAP FOR TODAY" ON A BUILTIN OFFICER'S DAILY
-- BUDGET (internal/officers/builtin cap_raise.go; docs/officers-builtin.md
-- "An officer's page").
--
--   officer_cap_raises   one row per (owner, officer, UTC day): daily_usd, the
--       officer's daily_budget_usd for THAT day only, raised by its owner from
--       the officer's page. The wake loop, the claim and the pages read the
--       row for the current UTC day and nothing else, so the raise ends at the
--       next UTC midnight by construction: no cleanup job, and an old row is
--       inert history. A raise only ever raises: a row at or below the
--       declared budget changes nothing (officers.Officer.WithRaisedCap).
--
--   owner_id is the officer's OWNER, who is the only person who may raise it
--   (a team's officer is not raised here: its owner is a team). raised_at is
--   the last raise that day. Every raise also writes an audit_log row in the
--   same transaction (action officer.cap_raise).
--
--   No foreign key to the officer: an officer is a def, a declared row or a
--   MUSTER_OFFICERS entry, and a raise of one later deleted is inert.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, PERSONAL share class (0156's
-- shape). rollback/rls_off.sql turns it off with the rest, and --rls-on
-- re-applies this file (db.RLSOn), so every statement is idempotent.
--
-- Must apply after its predecessor, 0156_officer_seen (the contiguity test
-- refuses a gap). 🔴 RENUMBER IT (and db.officerCapRaisesMigrationVersion) at
-- merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE public.officer_cap_raises;
--   DELETE FROM schema_migrations WHERE version = 157;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_cap_raises (
    owner_id   BIGINT           NOT NULL
               DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
               REFERENCES public.users (id) ON DELETE CASCADE,
    officer    TEXT             NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    day        DATE             NOT NULL,
    daily_usd  DOUBLE PRECISION NOT NULL CHECK (daily_usd > 0 AND daily_usd <= 10000),
    raised_at  TIMESTAMPTZ      NOT NULL,
    PRIMARY KEY (owner_id, officer, day)
);

ALTER TABLE public.officer_cap_raises ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.officer_cap_raises ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.officer_cap_raises FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.officer_cap_raises;
CREATE POLICY owner_isolation ON public.officer_cap_raises
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.officer_cap_raises TO muster_app;
