-- muster 0170 — THE OFFICER WAKE LOOP'S BOUNDED AUTO-RESTART, PERSISTED
-- (internal/officers autorestart.go, restarts_pg.go; docs/officers.md "An
-- instance in error").
--
--   officer_auto_restarts   one row per (owner, officer, UTC day, instance):
--       restarts, how many times the wake loop restarted that instance from
--       error that day (the officer's daily cap is their SUM over its
--       instances; each instance's count is its backoff's exponent);
--       last_restart_at, the last; held_told, whether the owner was told the
--       instance's restart is held (cleared when it leaves error); and
--       last_error, the instance's error_message when it was last restarted
--       or held — the only record of it once a restart's Stop blanks the
--       agent row's. A restarted muster reads the day's rows, so its cap,
--       backoff and held notice carry over a rollout. Every reader asks for
--       the current UTC day, so a day's rows are inert history the next day.
--
--   owner_id is the officer's OWNER (a person or a team: the wake loop acts
--   as it). No foreign key to the officer: an officer is a def, a declared
--   row or a MUSTER_OFFICERS entry, and a row of one later deleted is inert.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, PERSONAL share class (0157's
-- shape). rollback/rls_off.sql turns it off with the rest, and --rls-on
-- re-applies this file (db.RLSOn), so every statement is idempotent.
--
-- Must apply after its predecessor, 0169_officer_pr_queue (the contiguity
-- test refuses a gap). 🔴 RENUMBER IT (and
-- db.officerAutoRestartsMigrationVersion) at merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE public.officer_auto_restarts;
--   DELETE FROM schema_migrations WHERE version = 170;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_auto_restarts (
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id) ON DELETE CASCADE,
    officer         TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    day             DATE        NOT NULL,
    instance        TEXT        NOT NULL CHECK (length(instance) BETWEEN 1 AND 200),
    restarts        INTEGER     NOT NULL DEFAULT 0 CHECK (restarts BETWEEN 0 AND 1000),
    last_restart_at TIMESTAMPTZ,
    held_told       BOOLEAN     NOT NULL DEFAULT false,
    last_error      TEXT        NOT NULL DEFAULT '' CHECK (length(last_error) <= 2000),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, officer, day, instance)
);

ALTER TABLE public.officer_auto_restarts ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.officer_auto_restarts ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.officer_auto_restarts FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.officer_auto_restarts;
CREATE POLICY owner_isolation ON public.officer_auto_restarts
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.officer_auto_restarts TO muster_app;
