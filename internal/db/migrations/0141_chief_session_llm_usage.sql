-- muster 0141 — WHAT A CHIEF'S OPENROUTER SESSIONS SPEND (internal/chiefusage
-- sessionllm.go; docs/chief.md "OpenRouter sessions, budget").
--
--   chief_session_llm_usage   one row per (owner, chat session, UTC day) the
--       LLM proxy served for a chief session on OpenRouter (the request carried
--       X-Muster-Chief-Session): requests, tokens by kind, and the USD booked —
--       the provider's own reported cost (`usage.cost`), or the proxy's
--       pessimistic estimate for a response that carried none (counted in
--       estimated_requests). The daily cap is checked against sum(cost_usd)
--       for the owner and day, the optional per-session cap against
--       sum(cost_usd) for the session, so a restart resets neither.
--
-- 🔴 NUMBERS ONLY. No column can hold a prompt, a response, a header or the
-- provider key. model is the session's stored model id (the pin).
--
-- 🔴 session_id HAS NO FOREIGN KEY, ON PURPOSE: deleting a chat session must
-- not delete the day's spend, or deleting a session would reset the daily cap.
--
-- 🔴 THE APP ROLE CANNOT DELETE A ROW (no DELETE grant): the budget's own book
-- is append-and-add only, so no path in the app can reset a cap by removing
-- today's row.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0075's shape: the row's
-- owner is the chief's owner (the proxy request's principal), and
-- owner_isolation keeps one user from reading or writing another's spend.
-- PERSONAL share class (no team access). rollback/rls_off.sql turns it off
-- with the rest, and --rls-on re-applies this file (db.RLSOn), so every
-- statement is idempotent.
--
-- Must apply after its predecessor, 0140 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT (and db.chiefSessionLLMUsageMigrationVersion) if another
-- migration takes 0141 first.
--
-- ROLLBACK (after rolling the image back; an older image never reads the
-- table):
--   DROP TABLE public.chief_session_llm_usage;
--   DELETE FROM schema_migrations WHERE version = 141;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.chief_session_llm_usage (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id           BIGINT      NOT NULL
                       DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                       REFERENCES public.users (id),
    session_id         BIGINT      NOT NULL CHECK (session_id >= 1),
    day                DATE        NOT NULL,
    model              TEXT        NOT NULL DEFAULT '' CHECK (char_length(model) <= 200),
    requests           BIGINT      NOT NULL DEFAULT 0 CHECK (requests >= 0),
    input_tokens       BIGINT      NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens      BIGINT      NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens  BIGINT      NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_write_tokens BIGINT      NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    cost_usd           NUMERIC(14, 6) NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    estimated_requests BIGINT      NOT NULL DEFAULT 0 CHECK (estimated_requests >= 0),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chief_session_llm_usage_one_row UNIQUE (owner_id, session_id, day)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.chief_session_llm_usage ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

-- The daily cap: sum(cost_usd) WHERE owner_id = $1 AND day = $2.
CREATE INDEX IF NOT EXISTS chief_session_llm_usage_owner_day ON public.chief_session_llm_usage (owner_id, day);
-- The session cap: sum(cost_usd) WHERE session_id = $1.
CREATE INDEX IF NOT EXISTS chief_session_llm_usage_session ON public.chief_session_llm_usage (session_id);

ALTER TABLE public.chief_session_llm_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.chief_session_llm_usage FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.chief_session_llm_usage;
CREATE POLICY owner_isolation ON public.chief_session_llm_usage
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.chief_session_llm_usage TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.chief_session_llm_usage_id_seq TO muster_app;
