-- muster 0109 — the 'account-usage' personal API token scope (internal/apitokens).
--
--   * api_tokens_scope_check widened to allow 'account-usage': a token that
--     reaches GET /api/cc-accounts/usage and nothing else (internal/api's
--     tokenScopeRoutes) — the read-only, numbers-only view of its owner's
--     Claude accounts' utilization and who used them (docs/jobs.md "Claude
--     account usage"). Minted at /account/tokens, for a service that should
--     read that and reach nothing else.
--
-- No data changes: every existing row is 'full', 'summary' or 'agent-create',
-- which the widened check still allows.
--
-- Must apply after 0108.
--
-- Rollback: an image from before 0109 treats an unknown scope as reaching
-- nothing (internal/api tokenScopeAdmits), so an 'account-usage' token is inert
-- under it — the safe direction. To restore the narrower check, revoke the live
-- 'account-usage' tokens first (api_tokens is under FORCED row-level security;
-- run it with muster.scope = 'all', as 0068's note says), then:
--   ALTER TABLE public.api_tokens DROP CONSTRAINT IF EXISTS api_tokens_scope_check;
--   ALTER TABLE public.api_tokens ADD CONSTRAINT api_tokens_scope_check CHECK (scope IN ('full', 'summary', 'agent-create'));
--   DELETE FROM schema_migrations WHERE version = 109;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.api_tokens DROP CONSTRAINT IF EXISTS api_tokens_scope_check;
ALTER TABLE public.api_tokens ADD CONSTRAINT api_tokens_scope_check CHECK (scope IN ('full', 'summary', 'agent-create', 'account-usage'));
