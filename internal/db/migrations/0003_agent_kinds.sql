-- muster 0003 — agent KINDS, and the per-account Claude Code token pool.
--
-- 🔴 EVERY EXISTING ROW BECOMES KIND 'gateway', WHICH IS WHAT IT ALREADY WAS.
-- Until this migration muster had exactly one agent runtime: the operator-
-- configured image reached over the `hooks-sha256` gateway wire. 'gateway' is
-- that runtime's neutral name (the image's own identifier is a denied token in
-- this public repository — see tests/leakscan.py). The DEFAULT is what keeps an
-- unchanged deployment unchanged: an INSERT that names no kind (every insert any
-- binary before this one makes) lands as 'gateway', and internal/agentspec builds
-- a 'gateway' row byte-for-byte as it built every row before kinds existed.
--
-- 🔴 A 'claude-code' AGENT KEEPS ITS ACCOUNT FOR LIFE, AND THE DATABASE ENFORCES
-- IT RATHER THAN THE APPLICATION REMEMBERING IT. The account is chosen ONCE, at
-- dispatch, by internal/ccpool; the agent's conversation, its rate-limit history
-- and its transcripts on the PVC all belong to that account's subscription. A
-- later UPDATE that moved an agent to another account would hand one account's
-- conversation to another account's token. So:
--   * agents_cc_account_matches_kind: a claude-code row MUST name an account and
--     every other row MUST NOT. A claude-code agent with no account would build a
--     pod with no credential, which internal/agentspec refuses — this makes the
--     row itself unrepresentable.
--   * agents_kind_account_immutable: an UPDATE that changes `kind` or
--     `cc_account` raises. No application code path writes either column after
--     the INSERT (internal/agents.PGStore has no setter, by design); the trigger
--     covers a hand-run UPDATE and every future call site at once.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agents
    ADD COLUMN IF NOT EXISTS kind       TEXT NOT NULL DEFAULT 'gateway',
    ADD COLUMN IF NOT EXISTS cc_account TEXT NOT NULL DEFAULT '';

ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_kind_check;
ALTER TABLE public.agents ADD CONSTRAINT agents_kind_check
    CHECK (kind IN ('gateway', 'claude-code'));

ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_cc_account_matches_kind;
ALTER TABLE public.agents ADD CONSTRAINT agents_cc_account_matches_kind
    CHECK ((kind = 'claude-code') = (cc_account <> ''));

CREATE OR REPLACE FUNCTION public.muster_agent_kind_immutable() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.cc_account IS DISTINCT FROM OLD.cc_account THEN
        RAISE EXCEPTION 'agent % (%): kind and cc_account are fixed at creation (kind % -> %, account % -> %); an agent keeps its runtime and its Claude account for life',
            OLD.id, OLD.name, OLD.kind, NEW.kind, OLD.cc_account, NEW.cc_account;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS agents_kind_account_immutable ON public.agents;
CREATE TRIGGER agents_kind_account_immutable
    BEFORE UPDATE OF kind, cc_account ON public.agents
    FOR EACH ROW
    EXECUTE FUNCTION public.muster_agent_kind_immutable();

-- The pool's memory of what each account's sessions have reported.
--
-- ⚠ KEYED BY THE ACCOUNT NAME, WITH NO SURROGATE id, for the reason
-- agent_retired_names gives: one account has one record, and an id would let it
-- have two. A row is created by the first mark (an UPSERT), never by
-- configuration: an account nothing has ever marked has no row and is, to the
-- selector, an account that has never been rate-limited.
--
-- ⚠ auth_failed_token IS A FINGERPRINT (a truncated sha256), NEVER THE TOKEN. It
-- is what lets an auth failure EXPIRE ON ITS OWN when the operator replaces the
-- account's token: the selector only honours an auth_failed mark whose
-- fingerprint matches the token configured NOW. See internal/ccpool.
CREATE TABLE IF NOT EXISTS public.cc_account_marks (
    account             TEXT PRIMARY KEY,
    rate_limited_at     TIMESTAMPTZ,
    rate_limited_detail TEXT NOT NULL DEFAULT '',
    auth_failed_at      TIMESTAMPTZ,
    auth_failed_detail  TEXT NOT NULL DEFAULT '',
    auth_failed_token   TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_agents_cc_account ON public.agents (cc_account) WHERE cc_account <> '';
