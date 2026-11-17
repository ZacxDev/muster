-- muster 0144 — WHAT A CHIEF'S OPENROUTER SESSIONS SPEND (internal/chiefusage
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
--   chief_session_llm_reservations   one row per proxied request IN FLIGHT:
--       the pessimistic estimate it may cost, reserved against both caps
--       before the request is sent, in the same transaction (under a
--       per-owner advisory lock) as the check that the caps leave room for
--       it; deleted when the request is booked or ends unsent; ignored once
--       expires_at passes (a request whose process died). So N concurrent
--       requests cannot all pass the check on the same spent amount.
--
-- 🔴 NUMBERS ONLY. No column can hold a prompt, a response, a header or the
-- provider key. model is the session's stored model id (the pin).
--
-- 🔴 session_id HAS NO FOREIGN KEY, ON PURPOSE: deleting a chat session must
-- not delete the day's spend, or deleting a session would reset the daily cap.
--
-- 🔴 THE BOOK IS APPEND-AND-ADD ONLY, BY TRIGGER (chief_session_llm_usage_
-- append_only): a row DELETE is refused, and so is an UPDATE that moves a row
-- (owner, session, day) or lowers requests, a token count or cost_usd. A
-- GRANT cannot say this: muster_app holds DELETE on every table (0007's
-- default privileges, internal/db approle.go's boot GRANT). The proxy's upsert
-- only adds, so it passes. TRUNCATE and DROP fire no row trigger; the app
-- role holds neither.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0075's shape: the row's
-- owner is the chief's owner (the proxy request's principal), and
-- owner_isolation keeps one user from reading or writing another's spend.
-- PERSONAL share class (no team access). rollback/rls_off.sql turns it off
-- with the rest, and --rls-on re-applies this file (db.RLSOn), so every
-- statement is idempotent.
--
-- Must apply after its predecessor, 0143_officer_items_subject_index (the
-- contiguity test refuses a gap).
-- 🔴 RENUMBER IT (and db.chiefSessionLLMUsageMigrationVersion) if another
-- migration takes 0144 first.
--
-- ROLLBACK (after rolling the image back; an older image never reads the
-- tables):
--   DROP TABLE public.chief_session_llm_reservations;
--   DROP TABLE public.chief_session_llm_usage;   (its trigger goes with it)
--   DROP FUNCTION public.chief_session_llm_usage_append_only();
--   DELETE FROM schema_migrations WHERE version = 144;

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

-- The book is append-and-add only (the header).
CREATE OR REPLACE FUNCTION public.chief_session_llm_usage_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'chief_session_llm_usage is append-only: a row of the chief sessions'' spend cannot be deleted (id %)', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.session_id IS DISTINCT FROM OLD.session_id
       OR NEW.day IS DISTINCT FROM OLD.day
       OR NEW.requests < OLD.requests OR NEW.input_tokens < OLD.input_tokens OR NEW.output_tokens < OLD.output_tokens
       OR NEW.cache_read_tokens < OLD.cache_read_tokens OR NEW.cache_write_tokens < OLD.cache_write_tokens
       OR NEW.cost_usd < OLD.cost_usd OR NEW.estimated_requests < OLD.estimated_requests THEN
        RAISE EXCEPTION 'chief_session_llm_usage is append-only: a row cannot be moved and no count or cost can go down (id %)', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS chief_session_llm_usage_append_only ON public.chief_session_llm_usage;
CREATE TRIGGER chief_session_llm_usage_append_only BEFORE UPDATE OR DELETE ON public.chief_session_llm_usage
    FOR EACH ROW EXECUTE FUNCTION public.chief_session_llm_usage_append_only();

CREATE TABLE IF NOT EXISTS public.chief_session_llm_reservations (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id    BIGINT      NOT NULL
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    session_id  BIGINT      NOT NULL CHECK (session_id >= 1),
    amount_usd  NUMERIC(14, 6) NOT NULL CHECK (amount_usd > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL
);

ALTER TABLE public.chief_session_llm_reservations ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS chief_session_llm_reservations_owner ON public.chief_session_llm_reservations (owner_id, expires_at);

ALTER TABLE public.chief_session_llm_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.chief_session_llm_reservations FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.chief_session_llm_reservations;
CREATE POLICY owner_isolation ON public.chief_session_llm_reservations
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, DELETE ON public.chief_session_llm_reservations TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.chief_session_llm_reservations_id_seq TO muster_app;
