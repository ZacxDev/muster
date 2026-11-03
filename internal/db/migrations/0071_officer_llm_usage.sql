-- muster 0071 — THE OFFICER LLM PROXY'S USAGE (internal/llmproxy).
-- docs/officers.md "The LLM proxy".
--
--   officer_llm_usage   one row per (owner, officer, instance number, UTC day)
--       the proxy has served: requests, tokens by direction, the USD booked
--       (the provider's own reported cost, or the proxy's pessimistic estimate
--       for a response that carried none — estimated_requests counts those).
--       The daily budget is checked against sum(cost_usd) for the officer and
--       day, so a restart does not reset it.
--   connector_audit.kind gains 'llm': one row per proxied request (officer,
--       instance, item, model, provider, tokens, USD, duration, result).
--
-- 🔴 NUMBERS ONLY. No column can hold a prompt, a response, a header or the
-- provider key.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0049's shape: the row's
-- owner is the officer's owner (the request's principal), and owner_isolation
-- keeps one user from reading or writing another's usage. rollback/rls_off.sql
-- turns it off with the rest, and --rls-on re-applies this file (db.RLSOn),
-- so every statement is idempotent.
--
-- Must apply after 0070 (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads the
-- table or writes 'llm'):
--   DELETE FROM public.connector_audit WHERE kind = 'llm';
--   ALTER TABLE public.connector_audit DROP CONSTRAINT connector_audit_kind_check;
--   ALTER TABLE public.connector_audit ADD CONSTRAINT connector_audit_kind_check
--     CHECK (kind IN ('proxy', 'grant', 'query'));
--   DROP TABLE public.officer_llm_usage;
--   DELETE FROM schema_migrations WHERE version = 71;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_llm_usage (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id           BIGINT      NOT NULL
                       DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                       REFERENCES public.users (id),
    officer            TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    instance           INT         NOT NULL CHECK (instance >= 1),
    day                DATE        NOT NULL,
    requests           BIGINT      NOT NULL DEFAULT 0 CHECK (requests >= 0),
    input_tokens       BIGINT      NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens      BIGINT      NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens  BIGINT      NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_write_tokens BIGINT      NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    cost_usd           NUMERIC(14, 6) NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    estimated_requests BIGINT      NOT NULL DEFAULT 0 CHECK (estimated_requests >= 0),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT officer_llm_usage_one_row UNIQUE (owner_id, officer, instance, day)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.officer_llm_usage ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

-- SpentToday: sum(cost_usd) WHERE officer = $1 AND day = $2.
CREATE INDEX IF NOT EXISTS officer_llm_usage_officer_day ON public.officer_llm_usage (officer, day);

ALTER TABLE public.officer_llm_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.officer_llm_usage FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.officer_llm_usage;
CREATE POLICY owner_isolation ON public.officer_llm_usage
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.officer_llm_usage TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.officer_llm_usage_id_seq TO muster_app;

ALTER TABLE public.connector_audit DROP CONSTRAINT IF EXISTS connector_audit_kind_check;
ALTER TABLE public.connector_audit ADD CONSTRAINT connector_audit_kind_check
    CHECK (kind IN ('proxy', 'grant', 'query', 'llm'));
