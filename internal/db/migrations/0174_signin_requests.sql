-- muster 0174 — SIGN IN WITH THE MUSTER APP (docs/design/qr-sign-in.md;
-- docs/auth.md#sign-in-with-the-muster-app).
--
--   signin_requests   one row per "Sign in with the muster app" a signed-out
--       browser started: the browser shows a QR code that carries `code`; a
--       phone already signed in to muster opens it, sees this row's details
--       and approves or denies; the browser that started it then redeems the
--       approval for a session of the approver.
--
--     code_sha256      the sha256 of the code in the QR (a 32-byte random
--                      value). The code itself is never stored.
--     binding_sha256   the sha256 of the BROWSER BINDING: a second 32-byte
--                      random value, set as an HttpOnly cookie on the browser
--                      that started the request and never shown anywhere. A
--                      redeem must carry the code AND this cookie, so a code
--                      read off a screen, a log or a photo cannot be redeemed
--                      by any other browser.
--     state            pending -> scanned -> approved | denied; approved ->
--                      redeemed. Expiry is not a state: a row whose
--                      expires_at has passed is expired whatever its state
--                      says, and every transition checks it.
--     requester_ip / requester_agent   what the phone is shown: the client
--                      address muster rate-limits on and a short summary of
--                      the browser's User-Agent ("Chrome on Windows").
--     approved_by / approver_signin_at   who approved, and the SIGN-IN time
--                      of the session that approved. A redeem refuses an
--                      approval whose session has since been ended (the
--                      approver disabled, reset, or signed out everywhere),
--                      with the rule devices.LiveSQL uses for a paired phone.
--
-- 🔴 NOT OWNED: no owner_id. A request has no owner until it is approved, and
-- every step but the approval runs for a signed-out browser. Row-level
-- security is enabled and forced with one policy, scope_all (the system scope
-- only, the shape 0009 gives invites and 0105 gives instance_branding), so a
-- user's principal reads and writes nothing; internal/signinreq runs every
-- query as the system through its one ledgered helper. Neither --rls-off nor
-- db.RLSOn touches it.
--
-- Rows are short-lived (two minutes) and are swept by each insert: a row
-- expired for more than an hour is deleted.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE IF EXISTS public.signin_requests;
--   DELETE FROM schema_migrations WHERE version = 174;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.signin_requests (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code_sha256         BYTEA       NOT NULL UNIQUE,
    binding_sha256      BYTEA       NOT NULL UNIQUE,
    state               TEXT        NOT NULL DEFAULT 'pending',
    requester_ip        TEXT        NOT NULL DEFAULT '',
    requester_agent     TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at          TIMESTAMPTZ NOT NULL,
    scanned_at          TIMESTAMPTZ,
    decided_at          TIMESTAMPTZ,
    redeemed_at         TIMESTAMPTZ,
    approved_by         BIGINT      REFERENCES public.users (id),
    approver_signin_at  TIMESTAMPTZ,
    CONSTRAINT signin_requests_code_sha256_check CHECK (octet_length(code_sha256) = 32),
    CONSTRAINT signin_requests_binding_sha256_check CHECK (octet_length(binding_sha256) = 32),
    CONSTRAINT signin_requests_state_check CHECK (state IN ('pending', 'scanned', 'approved', 'denied', 'redeemed')),
    CONSTRAINT signin_requests_ip_check CHECK (char_length(requester_ip) <= 64),
    CONSTRAINT signin_requests_agent_check CHECK (char_length(requester_agent) <= 120),
    -- An approval always names its approver and that session's sign-in time,
    -- and nothing else does.
    CONSTRAINT signin_requests_approver_check CHECK (
        (state IN ('approved', 'redeemed')) = (approved_by IS NOT NULL AND approver_signin_at IS NOT NULL)),
    CONSTRAINT signin_requests_redeemed_check CHECK ((state = 'redeemed') = (redeemed_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_signin_requests_expires ON public.signin_requests (expires_at);

ALTER TABLE public.signin_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.signin_requests FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS scope_all ON public.signin_requests;
CREATE POLICY scope_all ON public.signin_requests AS PERMISSIVE FOR ALL TO PUBLIC
    USING (current_setting('muster.scope', true) = 'all')
    WITH CHECK (current_setting('muster.scope', true) = 'all');

GRANT SELECT, INSERT, UPDATE, DELETE ON public.signin_requests TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.signin_requests_id_seq TO muster_app;
