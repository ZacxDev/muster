-- muster 0057 — A PAIRED PHONE'S SIGNED ACTIONS: the one-time challenges a
-- device's step-up key signs (internal/devices challenge.go;
-- docs/design/android.md, server change 7, "A gated action, step by step").
--
--   device_challenges  one row per challenge a paired device asked for
--                      (POST /api/device/challenge): the sha256 of its nonce
--                      (the nonce itself is shown to the app once), the
--                      action id and target it is bound to, the sha256 of the
--                      exact text it is bound to (NULL for an action that
--                      carries none), when it expires (2 minutes) and when it
--                      was spent. Spent by ONE conditional UPDATE on used_at
--                      IS NULL and expires_at > now(), whatever the signature
--                      then says (a bad signature burns it too), so a nonce
--                      is used at most once. Removing the device removes its
--                      challenges (ON DELETE CASCADE).
--
-- Under FORCE row-level security, owner-isolated, as devices (0039): a request
-- sees only its own user's rows. Every read and write runs under the device
-- owner's principal (the device tier has authenticated the credential first).
--
-- Must apply after 0056 (the contiguity test refuses a gap). Re-applied by
-- db.RLSOn after rollback/rls_off.sql, so every statement is idempotent.
--
-- ROLLBACK. An image from before 0057 runs over this schema unchanged (it
-- never reads the table). To remove it, after rolling the image back:
--   DROP TABLE IF EXISTS device_challenges;
--   DELETE FROM schema_migrations WHERE version = 57;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.device_challenges (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    device_id     BIGINT      NOT NULL,
    nonce_sha256  BYTEA       NOT NULL UNIQUE,
    action        TEXT        NOT NULL,
    target        TEXT        NOT NULL,
    text_sha256   BYTEA,
    expires_at    TIMESTAMPTZ NOT NULL,
    used_at       TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT device_challenges_nonce_sha256_check CHECK (octet_length(nonce_sha256) = 32),
    CONSTRAINT device_challenges_text_sha256_check CHECK (text_sha256 IS NULL OR octet_length(text_sha256) = 32),
    CONSTRAINT device_challenges_action_check CHECK (action ~ '^[a-z][a-z_]{0,39}$'),
    CONSTRAINT device_challenges_target_check CHECK (char_length(target) BETWEEN 1 AND 128),
    CONSTRAINT device_challenges_device_owner_fkey FOREIGN KEY (device_id, owner_id)
        REFERENCES public.devices (id, owner_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_device_challenges_device ON public.device_challenges (device_id);

-- Re-stated so a re-run (db.RLSOn after rollback/rls_off.sql) restores it.
ALTER TABLE public.device_challenges ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.device_challenges ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.device_challenges FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.device_challenges;
CREATE POLICY owner_isolation ON public.device_challenges
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.device_challenges TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.device_challenges_id_seq TO muster_app;
