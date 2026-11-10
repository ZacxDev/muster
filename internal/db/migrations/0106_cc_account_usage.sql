-- muster 0106 — EACH POOLED CLAUDE ACCOUNT'S LATEST SUBSCRIPTION UTILIZATION
-- (internal/ccusage).
--
--   cc_account_usage   one row per (owner, account) the usage poller has
--       read: the latest GOOD reading's windows (`windows`, a JSON object
--       {"five_hour": {"utilization": <percent>, "resets_at": "<RFC 3339>"},
--       "seven_day": {...}, ...}) and when it was taken (`read_at`), plus what
--       the latest attempt made of it — `checked_at`, `error_kind` and a short
--       `error` sentence, the attempted token's fingerprint (`token_fp`: a
--       401/403 holds only while the token is unchanged), the 429 streak and
--       the earliest next attempt (`next_poll_at`).
--
-- 🔴 NUMBERS ONLY. No column holds a token, a response body or a header:
-- `token_fp` is ccpool.Fingerprint (12 hex characters of a sha256), exactly
-- what cc_account_marks.auth_failed_token holds.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in cc_account_marks' (0011)
-- and 0094's shape: owner_isolation keeps one user from reading another's
-- accounts' usage. rollback/rls_off.sql turns it off with the rest, and
-- --rls-on re-applies this file (db.RLSOn), so every statement is idempotent.
--
-- Must apply after 0105 (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads the
-- table):
--   DROP TABLE IF EXISTS public.cc_account_usage;
--   DELETE FROM schema_migrations WHERE version = 106;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.cc_account_usage (
    owner_id          BIGINT      NOT NULL
                      DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                      REFERENCES public.users (id),
    -- ccpool.ValidName.
    account           TEXT        NOT NULL
                      CONSTRAINT cc_account_usage_account_check CHECK (account ~ '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$'),
    windows           JSONB       NOT NULL DEFAULT '{}'::jsonb
                      CONSTRAINT cc_account_usage_windows_check CHECK (jsonb_typeof(windows) = 'object'),
    read_at           TIMESTAMPTZ,
    checked_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- ccusage.Kinds.
    error_kind        TEXT        NOT NULL DEFAULT ''
                      CONSTRAINT cc_account_usage_error_kind_check
                      CHECK (error_kind IN ('', 'no_token', 'unauthorized', 'forbidden', 'rate_limited', 'error')),
    error             TEXT        NOT NULL DEFAULT ''
                      CONSTRAINT cc_account_usage_error_cap CHECK (char_length(error) <= 300),
    token_fp          TEXT        NOT NULL DEFAULT ''
                      CONSTRAINT cc_account_usage_token_fp_check CHECK (token_fp ~ '^([0-9a-f]{12})?$'),
    rate_limit_streak INT         NOT NULL DEFAULT 0
                      CONSTRAINT cc_account_usage_streak_check CHECK (rate_limit_streak >= 0),
    next_poll_at      TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, account)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.cc_account_usage ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.cc_account_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.cc_account_usage FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.cc_account_usage;
CREATE POLICY owner_isolation ON public.cc_account_usage
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.cc_account_usage TO muster_app;
