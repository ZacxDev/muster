-- muster 0122 — PLUGIN PAIRING (internal/plugins pairing.go; docs/plugins.md
-- "Pairing a plugin without handling its token").
--
--   plugin_pair_requests  a plugin's unauthenticated request for a token, for
--                         ONE owner: what it would be minted as, the SHA-256 of
--                         the code the owner compares and of the poll secret
--                         the plugin collects the token with, and its state.
--                         The token is minted when the plugin collects it, so
--                         no token is ever stored.
--   plugin_pair_decoys    the same answer for a request naming NO account: its
--                         public id, poll secret hash and expiry only, so the
--                         unauthenticated door says nothing about who has an
--                         account. No owner, nothing to decide.
--
-- 🔴 plugin_pair_requests IS OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in
-- 0015's shape: only its owner reads or decides it. The unauthenticated door
-- writes it under the system scope, naming the owner it looked up, and the
-- poll reads it the same way — each a system-ledger entry in internal/api.
-- rollback/rls_off.sql turns RLS off with the rest and db.RLSOn re-applies this
-- body, so every statement is idempotent.
--
-- Must apply after 0121 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT (and db.pairRequestsMigrationVersion) at merge if another
-- migration took 0122.
--
-- ROLLBACK. Nothing older reads either table. To remove them (optionally):
--   DROP TABLE public.plugin_pair_requests, public.plugin_pair_decoys;
--   DELETE FROM schema_migrations WHERE version = 122;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.plugin_pair_requests (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id         BIGINT      NOT NULL
                     DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                     REFERENCES public.users (id),
    public_id        TEXT        NOT NULL UNIQUE CHECK (public_id ~ '^[A-Za-z0-9_-]{22}$'),
    name             TEXT        NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
    label            TEXT        NOT NULL CHECK (char_length(label) BETWEEN 1 AND 80),
    data_scope       TEXT        NOT NULL CHECK (data_scope ~ '^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$'),
    capabilities     TEXT[]      NOT NULL CHECK (capabilities <@ ARRAY['messages', 'draft_checks']::text[] AND 'messages' = ANY (capabilities)),
    deliver_to       TEXT        CHECK (deliver_to IS NULL OR deliver_to ~ '^officer:[a-z0-9]+(-[a-z0-9]+)*:[a-z0-9]+(-[a-z0-9]+)*$'),
    code_hash        TEXT        NOT NULL CHECK (code_hash ~ '^[0-9a-f]{64}$'),
    poll_secret_hash TEXT        NOT NULL CHECK (poll_secret_hash ~ '^[0-9a-f]{64}$'),
    status           TEXT        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'approved', 'denied', 'redeemed')),
    source_addr      TEXT        NOT NULL DEFAULT '' CHECK (char_length(source_addr) <= 100),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL,
    decided_at       TIMESTAMPTZ,
    decided_via      TEXT        CHECK (char_length(decided_via) <= 300),
    redeemed_at      TIMESTAMPTZ,
    -- the plugin it minted (no foreign key: the record outlives a deleted plugin)
    plugin_id        BIGINT,
    -- decided ⇔ a decision time; redeemed ⇔ a hand-out time and a plugin
    CONSTRAINT plugin_pair_requests_decided CHECK ((status = 'pending') = (decided_at IS NULL)),
    CONSTRAINT plugin_pair_requests_redeemed CHECK ((status = 'redeemed') = (redeemed_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_plugin_pair_requests_owner_pending ON public.plugin_pair_requests (owner_id, id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_plugin_pair_requests_created ON public.plugin_pair_requests (created_at);

CREATE TABLE IF NOT EXISTS public.plugin_pair_decoys (
    public_id        TEXT        PRIMARY KEY CHECK (public_id ~ '^[A-Za-z0-9_-]{22}$'),
    poll_secret_hash TEXT        NOT NULL CHECK (poll_secret_hash ~ '^[0-9a-f]{64}$'),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_plugin_pair_decoys_created ON public.plugin_pair_decoys (created_at);

ALTER TABLE public.plugin_pair_requests ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.plugin_pair_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.plugin_pair_requests FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.plugin_pair_requests;
CREATE POLICY owner_isolation ON public.plugin_pair_requests
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.plugin_pair_requests TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.plugin_pair_requests_id_seq TO muster_app;
GRANT SELECT, INSERT, DELETE ON public.plugin_pair_decoys TO muster_app;
