-- NUMBER: 0128, allocated by the team-sharing builder (0125-0127: #649, #654, #657)
-- muster 0128 — AN AGENT'S CREDENTIALS MOVE OUT OF ITS ROW
-- (team sharing phase 4, slice 4.2; internal/agents secrets.go).
--
--   agent_secrets  one row per agent that has a credential: hooks_token (the
--                  agent's muster identity) and gateway_secret (its turn
--                  credential, 0042). Keyed by the agent; deleted with it.
--
-- 🔴 WHY. Every ordinary read of an agents row selected both columns, so any
-- request that listed agents loaded every listed agent's credentials into
-- memory. A later slice makes team agents readable by the team's members; with
-- the credentials in the row, every member's request would carry the team
-- chief's identity and turn credential. After this, the store's ordinary reads
-- (agentCols) name neither column, and the credentials are read only through
-- agents.Store.Secrets, by agent id.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY (owner_isolation), and that does
-- NOT change when agents becomes shared: a team member's principal can read a
-- team agent's row and still sees no row here. The composite foreign key keeps
-- a row's owner equal to its agent's.
--
-- ⚠ THE OLD COLUMNS STAY, AND ARE DUAL-WRITTEN. agents.hooks_token and
-- agents.gateway_secret are still written on every create and mint (one
-- statement writes both places), because an older image reads them. A later
-- migration drops them, one release after this one ships; until then
-- agents.Store.Secrets and GetByHooksToken fall back to them when this table
-- has nothing, for a token an older image minted after this ran.
--
-- The backfill fills only what is missing, so db.RLSOn's re-apply of this body
-- (rollback/rls_off.sql turns RLS off with the rest) is idempotent and never
-- overwrites a credential this table already holds.
--
-- 🔴 NUMBERED 0128 BY THE TEAM-SHARING BUILDER. 0125, 0126 and 0127 come from
-- other pull requests (#649; #654, team sharing phase 1; #657, phase 2a), which
-- merge first: until they have, this tree has a gap below 0128 and the
-- contiguity test is red here — by design, not a defect of this file. If the
-- number moves again, renumber it with db.agentSecretsMigrationVersion, the
-- test constant and the comments in 0006 that name it.
--
-- ROLLBACK. An older image ignores this table and reads the old columns, which
-- are still written, so rolling the image back needs no SQL. To remove it
-- (optionally, only while the old columns still exist):
--   DROP TABLE public.agent_secrets;
--   DELETE FROM schema_migrations WHERE version = 128;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.agent_secrets (
    agent_id       BIGINT      PRIMARY KEY,
    owner_id       BIGINT      NOT NULL
                   DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                   REFERENCES public.users (id),
    hooks_token    TEXT        NOT NULL DEFAULT '',
    gateway_secret TEXT        NOT NULL DEFAULT '',
    CONSTRAINT agent_secrets_agent_owner_fkey FOREIGN KEY (agent_id, owner_id)
        REFERENCES public.agents (id, owner_id) ON DELETE CASCADE
);

-- Unique and partial on `<> ''`, for 0001's idx_agents_hooks_token reason:
-- presenting a token identifies one agent, and many rows may hold none.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_secrets_hooks_token
    ON public.agent_secrets (hooks_token) WHERE hooks_token <> '';

ALTER TABLE public.agent_secrets ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.agent_secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.agent_secrets FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.agent_secrets;
CREATE POLICY owner_isolation ON public.agent_secrets
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.agent_secrets TO muster_app;

-- Backfill: every agent with a credential gets its row; a row that exists keeps
-- every value it has and gains only the ones it lacks.
INSERT INTO public.agent_secrets AS s (agent_id, owner_id, hooks_token, gateway_secret)
SELECT id, owner_id, hooks_token, gateway_secret
  FROM public.agents
 WHERE hooks_token <> '' OR gateway_secret <> ''
ON CONFLICT (agent_id) DO UPDATE SET
    hooks_token    = CASE WHEN s.hooks_token = '' THEN EXCLUDED.hooks_token ELSE s.hooks_token END,
    gateway_secret = CASE WHEN s.gateway_secret = '' THEN EXCLUDED.gateway_secret ELSE s.gateway_secret END;
