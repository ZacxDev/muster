-- muster 0068 — personal API token SCOPES (internal/apitokens).
--
--   * api_tokens.scope (default 'full'; api_tokens_scope_check): what the
--     token may reach. 'full' is every route a personal token reaches, as
--     before; 'summary' is GET /api/attention/summary and nothing else (what a
--     status bar polls). It is chosen when the token is minted
--     (/account/tokens) and never changes. Every existing row is 'full', so
--     every token minted before this one keeps the access it was minted with.
--
-- Which routes a scope reaches is decided at the token door (internal/api
-- admitAPIToken, tokenScopeRoutes): a narrower token is refused 403 on every
-- other route.
--
-- The column inherits the table's ownership, grants and row-level security
-- (0009's user_isolation); no policy changes, nothing for db.RLSOn to re-apply.
--
-- 🔴 ROLLOUT: MINT 'summary' TOKENS ONLY AFTER THE ROLLOUT HAS COMPLETED. An
-- image from before 0068 never reads scope, so while old and new pods both
-- serve (a RollingUpdate with maxUnavailable 0 keeps an old pod up until the
-- new one is ready), a 'summary' token minted mid-rollout has FULL access on
-- every request an old pod answers, until the last old pod is gone.
--
-- 🔴 Rollback: an image from before 0068 never reads scope, so it would admit
-- a 'summary' token with FULL access. Revoke every live narrower token first,
-- then drop the column. api_tokens is under FORCED row-level security, which
-- binds the table's OWNER too: as muster_app, or as the owner, without
-- muster.scope = 'all' the UPDATE matches NO rows and says nothing. Run it in
-- one transaction that sets the scope first (what the migration runner sets,
-- internal/db/scope.go), and check the count before committing:
--   BEGIN;
--   SET LOCAL muster.scope = 'all';
--   UPDATE public.api_tokens SET revoked_at = now() WHERE scope <> 'full' AND revoked_at IS NULL;
--   SELECT count(*) FROM public.api_tokens WHERE scope <> 'full' AND revoked_at IS NULL;  -- must be 0
--   COMMIT;
--   ALTER TABLE public.api_tokens DROP COLUMN scope;
--   DELETE FROM schema_migrations WHERE version = 68;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.api_tokens ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'full';
ALTER TABLE public.api_tokens DROP CONSTRAINT IF EXISTS api_tokens_scope_check;
ALTER TABLE public.api_tokens ADD CONSTRAINT api_tokens_scope_check CHECK (scope IN ('full', 'summary'));
