-- muster 0125 — OFFICER INSTANCES AS HOST-SPAWNED SESSIONS
-- (internal/officers hostspawns.go; docs/design/officers-on-hosts.md).
--
--   agents.runs_on_host   the mirror host a HOST OFFICER INSTANCE's agent row
--       stands for ('' for every other row): the row a host instance's
--       officer_post_draft and connector grants hang on, which NOTHING
--       provisions — agentprovision.Adapter refuses every lifecycle call on
--       it, the stopped-agent reaper's predicate excludes it, and every
--       consumer that iterates agent rows is classified in
--       internal/agents' hostRowConsumers ledger. The CHECK keeps such a row
--       out of the cluster's shapes: no namespace, no Claude account (so it is
--       the gateway kind), no worker pool.
--
--   officer_host_spawns   one row per host-spawned Claude Code session muster
--       asked a mirror host to start for an instance of a `"runtime": "host"`
--       officer: the officer, the instance agent name, the host, and what the
--       host's reply named (tmux_window, agent_session) or its refusal.
--
-- 🔴 THE id IS DRAWN FROM host_dispatches' OWN SEQUENCE. It travels as the
-- spawn's `dispatch` on the wire, becomes the window's @muster-dispatch, the
-- worktree directory and the branch muster/<id>-<slug> on the host, and the
-- chief's host-dispatch reconciler reads every window's dispatch: sharing the
-- sequence is what makes an officer spawn's id never equal a chief dispatch's.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0119's shape: the row's
-- owner is the officer's owner. rollback/rls_off.sql turns it off with the
-- rest, and --rls-on re-applies this file (db.RLSOn), so every statement is
-- idempotent. No DELETE grant: a row is history, never removed.
--
-- Must apply after 0124 (the contiguity test refuses a gap); 🔴 RENUMBER IT
-- (and db.officerHostSpawnsMigrationVersion) if another migration takes 0125
-- first.
--
-- ROLLBACK. An older image never reads the table; roll the image back, then
-- (optionally):
--   DROP TABLE public.officer_host_spawns;
--   DELETE FROM public.agents WHERE runs_on_host <> '';   -- the host instances' rows
--   ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_runs_on_host_check;
--   ALTER TABLE public.agents DROP COLUMN IF EXISTS runs_on_host;
--   DELETE FROM schema_migrations WHERE version = 125;
-- 🔴 Delete the host rows BEFORE rolling the image back: an older image reads
-- them as ordinary stopped gateway agents it could Start.
-- A host officer's window left running is then an ordinary window on its host.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_host_spawns (
    id            BIGINT      PRIMARY KEY DEFAULT nextval('public.host_dispatches_id_seq'::regclass),
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    officer       TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    instance      TEXT        NOT NULL CHECK (instance ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    host          TEXT        NOT NULL CHECK (host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    tmux_window   TEXT        NOT NULL DEFAULT '' CHECK (tmux_window = '' OR tmux_window ~ '^@[0-9]+$'),
    agent_session TEXT        NOT NULL DEFAULT '' CHECK (char_length(agent_session) <= 200),
    refusal       TEXT        NOT NULL DEFAULT '' CHECK (char_length(refusal) <= 500),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.officer_host_spawns ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS officer_host_spawns_instance ON public.officer_host_spawns (owner_id, officer, instance, id DESC);
CREATE INDEX IF NOT EXISTS officer_host_spawns_session ON public.officer_host_spawns (owner_id, host, agent_session) WHERE agent_session <> '';

ALTER TABLE public.officer_host_spawns ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.officer_host_spawns FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.officer_host_spawns;
CREATE POLICY owner_isolation ON public.officer_host_spawns
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.officer_host_spawns TO muster_app;
-- nextval() on the shared sequence (an identity column's own inserts need no
-- grant; an explicit nextval does).
GRANT USAGE ON SEQUENCE public.host_dispatches_id_seq TO muster_app;

-- The host officer instance's agent row (header).
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS runs_on_host TEXT NOT NULL DEFAULT '';
ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_runs_on_host_check;
ALTER TABLE public.agents ADD CONSTRAINT agents_runs_on_host_check CHECK (
    runs_on_host = ''
    OR (runs_on_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' AND namespace = '' AND kind = 'gateway' AND cc_account = '' AND worker_pool = '')
);
