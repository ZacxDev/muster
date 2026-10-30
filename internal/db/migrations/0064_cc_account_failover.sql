-- muster 0064 — Claude account FAILOVER: a claude-code agent whose account hit
-- its limit (or failed authentication) may be REBOUND to another of its
-- owner's accounts (internal/ccfailover).
--
-- 🔴 0003's "AN AGENT KEEPS ITS ACCOUNT FOR LIFE" IS RELAXED, NOT DROPPED. The
-- trigger still refuses every UPDATE of `kind`, and every UPDATE of
-- `cc_account` — EXCEPT one made inside a transaction that set
-- `muster.cc_account_rebind = 'on'` (SET LOCAL). Only the failover rebind
-- (internal/ccfailover PGStore.Rebind) sets it, and it does so for one
-- conditional UPDATE (`WHERE cc_account = <from>`), so a hand-run UPDATE and
-- every other code path are refused exactly as before. 0003's CHECK
-- (agents_cc_account_matches_kind) still holds: a rebind cannot empty the
-- account, and cannot give one to a non-claude-code row.
--
-- agents.cc_account_pinned: the account was named explicitly at creation (a
-- dispatch's or the chief's pin, an officer's `cc_account`). A pinned agent is
-- NOT failed over unless agents.cc_failover is also true (the operator's
-- opt-in). An agent created before 0064 reads unpinned, i.e. Auto.
--
-- jobs.failovers: the account failovers a job was re-queued for, oldest first,
-- each {"from","to","reason","at"}; the job runner re-queues a job at most
-- once per FROM account, so a job never loops.
--
-- cc_account_failovers: one row per rebind, for the agent's page and the API.
-- It holds account NAMES, a reason and a reset time — never a token.
--
-- Rollback: an image from before 0064 never reads the new columns or table,
-- and its trigger function is restored by re-running 0003's CREATE OR REPLACE.
-- To remove them:
--   DROP TABLE public.cc_account_failovers;
--   ALTER TABLE public.jobs DROP COLUMN failovers;
--   ALTER TABLE public.agents DROP COLUMN cc_account_pinned, DROP COLUMN cc_failover;
--   DELETE FROM schema_migrations WHERE version = 64;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agents
    ADD COLUMN IF NOT EXISTS cc_account_pinned BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS cc_failover       BOOLEAN NOT NULL DEFAULT false;

CREATE OR REPLACE FUNCTION public.muster_agent_kind_immutable() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.kind IS DISTINCT FROM OLD.kind
       OR (NEW.cc_account IS DISTINCT FROM OLD.cc_account
           AND coalesce(current_setting('muster.cc_account_rebind', true), '') <> 'on') THEN
        RAISE EXCEPTION 'agent % (%): kind and cc_account are fixed at creation (kind % -> %, account % -> %); an agent keeps its runtime, and its Claude account changes only by an account failover',
            OLD.id, OLD.name, OLD.kind, NEW.kind, OLD.cc_account, NEW.cc_account;
    END IF;
    RETURN NEW;
END;
$$;

ALTER TABLE public.jobs ADD COLUMN IF NOT EXISTS failovers JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE public.jobs DROP CONSTRAINT IF EXISTS jobs_failovers_check;
ALTER TABLE public.jobs ADD CONSTRAINT jobs_failovers_check
    CHECK (jsonb_typeof(failovers) = 'array' AND jsonb_array_length(failovers) <= 16);

CREATE TABLE IF NOT EXISTS public.cc_account_failovers (
    id           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    agent_id     BIGINT      NOT NULL REFERENCES public.agents (id) ON DELETE CASCADE,
    agent_name   TEXT        NOT NULL,
    from_account TEXT        NOT NULL,
    to_account   TEXT        NOT NULL,
    reason       TEXT        NOT NULL,
    resets_at    TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT cc_account_failovers_reason_check CHECK (reason IN ('rate_limited', 'auth_failed')),
    CONSTRAINT cc_account_failovers_accounts_check CHECK (
        from_account <> '' AND to_account <> '' AND from_account <> to_account
        AND octet_length(from_account) <= 128 AND octet_length(to_account) <= 128),
    CONSTRAINT cc_account_failovers_agent_name_cap CHECK (char_length(agent_name) BETWEEN 1 AND 128)
);

-- ⚠ A PLAIN FOREIGN KEY, NOT 0006's COMPOSITE (agent_id, owner_id): that one
-- depends on agents_id_owner_key, which 0006's re-runnable body drops and
-- re-creates (0014 says why). The owner is the agent's because the one writer
-- (internal/ccfailover PGStore.Record) inserts the agent's own owner_id.

-- Re-stated (not only in CREATE TABLE) so db.RLSOn, which re-applies this body
-- after RLSOff set the default to 1, restores it.
ALTER TABLE public.cc_account_failovers ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS idx_cc_account_failovers_agent ON public.cc_account_failovers (agent_id, id DESC);

ALTER TABLE public.cc_account_failovers ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.cc_account_failovers FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.cc_account_failovers;
CREATE POLICY owner_isolation ON public.cc_account_failovers
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.cc_account_failovers TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.cc_account_failovers_id_seq TO muster_app;
