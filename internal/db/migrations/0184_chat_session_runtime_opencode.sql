-- muster 0184 — A CHIEF CHAT SESSION MAY RUN ON OPENCODE (internal/agents
-- SessionRuntimeOpencode; cmd/ccd opencode lanes; docs/chief.md "OpenRouter
-- sessions").
--
--   chat_sessions.runtime 'opencode'   a side chat ccd runs in an opencode
--       server of its own, on an OpenRouter model reached through muster's
--       LLM proxy (the chief sessions' spend door). Such a session ALWAYS
--       names its model, stored as 'openrouter/<catalogue id>': the CHECK
--       below holds that shape, the application checks the catalogue.
--
-- 0030's CHECK is replaced whole, with every clause it had ('gateway' is
-- still admitted: rows carrying it route as legacy). The immutability
-- trigger (chat_sessions_runtime_immutable) is untouched, so a session's
-- runtime and model stay fixed for its life — an existing Claude session can
-- never be turned into an opencode one, nor the reverse.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default) and
-- idempotent: DROP/ADD CONSTRAINT.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_runtime_check;
ALTER TABLE public.chat_sessions ADD CONSTRAINT chat_sessions_runtime_check CHECK (
    runtime IN ('', 'claude-code', 'gateway', 'opencode')
    AND (runtime <> '' OR model = '')
    AND (runtime <> 'opencode' OR model LIKE 'openrouter/_%')
    AND char_length(model) <= 200);
