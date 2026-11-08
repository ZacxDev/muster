-- muster 0094 — the 'agent-create' personal API token scope (internal/apitokens).
--
--   * api_tokens_scope_check widened to allow 'agent-create': a token that
--     reaches POST /api/agents and nothing else (internal/api's
--     explicitScopeRoutes). A full token does NOT reach that route, so the
--     owner mints this scope deliberately, at /account/tokens (Create agents).
--     It is what replaced the web UI's dispatch form (docs/design/chief-dispatch.md,
--     phase 2).
--
-- No data changes: every existing row is 'full' or 'summary', which the widened
-- check still allows.
--
-- Rollback: an image from before 0094 treats an unknown scope as reaching
-- nothing (internal/api tokenScopeAdmits), so an 'agent-create' token is inert
-- under it — the safe direction. To restore the narrower check, revoke the live
-- 'agent-create' tokens first (api_tokens is under FORCED row-level security; run
-- it with muster.scope = 'all', as 0068's note says), then:
--   ALTER TABLE public.api_tokens DROP CONSTRAINT IF EXISTS api_tokens_scope_check;
--   ALTER TABLE public.api_tokens ADD CONSTRAINT api_tokens_scope_check CHECK (scope IN ('full', 'summary'));
--   DELETE FROM schema_migrations WHERE version = 94;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.api_tokens DROP CONSTRAINT IF EXISTS api_tokens_scope_check;
ALTER TABLE public.api_tokens ADD CONSTRAINT api_tokens_scope_check CHECK (scope IN ('full', 'summary', 'agent-create'));
