-- muster 0067 — the POSTGRES connector kind (internal/connectors postgres.go).
--
-- A postgres connector is not proxied: muster-server connects to the database
-- itself and runs one read-only statement per agent call. Its password is
-- sealed in secret_ciphertext like every connector secret; its host:port is
-- hosts[1] and its database user auth_username.
--
--   * connectors.auth_scheme gains 'postgres'.
--   * connectors.settings: a postgres connector's NON-SECRET settings, as JSON
--     ({"database","sslmode","require_tls","statement_timeout_ms","max_rows",
--     "max_bytes"}); '{}' for every other scheme (connectors_settings_scheme).
--     database, sslmode and require_tls are bound into the ciphertext's
--     additional data (seal.go), so editing them out of band makes the secret
--     fail to open.
--   * connector_audit.kind gains 'query': one row per agent query (the SQL's
--     fingerprint and first 120 characters, rows, duration, outcome class —
--     never a parameter value or the password).
--
-- The columns inherit their tables' ownership and row-level security; no
-- policy changes, nothing for db.RLSOn to re-apply.
--
-- Rollback: an image from before 0067 never reads settings and never writes
-- 'postgres' or 'query'. Delete any postgres connector first (an older image
-- would list it and its proxy would try to send its password over HTTP):
--   DELETE FROM public.connector_grants g USING public.connectors c
--     WHERE c.auth_scheme = 'postgres' AND g.connector_name = c.name AND g.owner_id = c.owner_id;
--   DELETE FROM public.connectors WHERE auth_scheme = 'postgres';
--   DELETE FROM public.connector_audit WHERE kind = 'query';
--   ALTER TABLE public.connectors DROP CONSTRAINT connectors_settings_scheme;
--   ALTER TABLE public.connectors DROP COLUMN settings;
--   ALTER TABLE public.connectors DROP CONSTRAINT connectors_auth_scheme_check;
--   ALTER TABLE public.connectors ADD CONSTRAINT connectors_auth_scheme_check
--     CHECK (auth_scheme IN ('bearer', 'header', 'basic'));
--   ALTER TABLE public.connector_audit DROP CONSTRAINT connector_audit_kind_check;
--   ALTER TABLE public.connector_audit ADD CONSTRAINT connector_audit_kind_check
--     CHECK (kind IN ('proxy', 'grant'));
--   DELETE FROM schema_migrations WHERE version = 67;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.connectors DROP CONSTRAINT IF EXISTS connectors_auth_scheme_check;
ALTER TABLE public.connectors ADD CONSTRAINT connectors_auth_scheme_check
    CHECK (auth_scheme IN ('bearer', 'header', 'basic', 'postgres'));

ALTER TABLE public.connectors ADD COLUMN IF NOT EXISTS settings JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE public.connectors DROP CONSTRAINT IF EXISTS connectors_settings_scheme;
ALTER TABLE public.connectors ADD CONSTRAINT connectors_settings_scheme
    CHECK ((auth_scheme = 'postgres') = (settings <> '{}'::jsonb));

ALTER TABLE public.connector_audit DROP CONSTRAINT IF EXISTS connector_audit_kind_check;
ALTER TABLE public.connector_audit ADD CONSTRAINT connector_audit_kind_check
    CHECK (kind IN ('proxy', 'grant', 'query'));
