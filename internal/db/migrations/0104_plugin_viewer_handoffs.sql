-- muster 0104 — THE VIEWER CAPABILITY AND HAND-OFF CODES (docs/design/
-- previews.md, "Capabilities on a plugin token" and seam (a), "Viewer
-- hand-off"; docs/plugins.md#viewer-hand-off).
--
--   plugins.origin      the origin pattern a `viewer` token was given, one of
--                       the deployment's MUSTER_PLUGIN_ORIGINS at mint
--                       (`https://*.example.net`); NULL on every other token.
--   plugins.entry_url   the https URL under that origin a hand-off is sent to;
--                       NULL exactly when origin is.
--   plugins_viewer_link both set exactly when the token has `viewer`.
--
--   plugin_handoffs     one row per hand-off CODE: the owner, the plugin it
--       is for (composite FK, so a row cannot name another owner's plugin),
--       the code's sha256 (the code itself is never stored), the target (the
--       session view it was minted on, `{"mirror":{host,session,window}}`),
--       the purpose (`view`), whether the browser's step-up was fresh at mint
--       (server-derived), and minted_at / expires_at / redeemed_at. Single
--       use: redeeming sets redeemed_at, and only a row with none is redeemed.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0054's shape: a hand-off is
-- minted by its owner's browser session and redeemed by its plugin's token,
-- which acts as the same owner (authz.ViaPlugin), so owner_isolation is what
-- makes another owner's code not exist. Re-applied by db.RLSOn. DELETE is
-- granted for the mint's own cleanup: each mint deletes its owner's rows
-- minted more than a day earlier. Nothing else prunes them, so an owner who
-- stops minting keeps their last rows (at most a day's worth) until the next.
--
-- Idempotent.
--
-- ROLLBACK. 🔴 Revoke every viewer token FIRST, then roll the image back: an
-- older image ignores these columns, so a live viewer token would sit there
-- with an origin and entry URL nothing shows or polices (and an image older
-- than 0102 admits it on every plugin route). Then:
--   UPDATE public.plugins SET revoked_at = now()
--    WHERE revoked_at IS NULL AND 'viewer' = ANY (capabilities);
--   DROP TABLE IF EXISTS public.plugin_handoffs;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_viewer_link;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_origin_check;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_entry_url_check;
--   ALTER TABLE public.plugins DROP COLUMN IF EXISTS origin, DROP COLUMN IF EXISTS entry_url;
--   DELETE FROM schema_migrations WHERE version = 104;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS origin TEXT;
ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS entry_url TEXT;

ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_origin_check;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_origin_check
    CHECK (origin IS NULL OR char_length(origin) BETWEEN 1 AND 300);
ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_entry_url_check;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_entry_url_check
    CHECK (entry_url IS NULL OR (char_length(entry_url) BETWEEN 1 AND 2048 AND entry_url LIKE 'https://%'));
ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_viewer_link;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_viewer_link
    CHECK ((origin IS NULL) = (entry_url IS NULL)
       AND (origin IS NOT NULL) = ('viewer' = ANY (capabilities)));

CREATE TABLE IF NOT EXISTS public.plugin_handoffs (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    plugin_id    BIGINT      NOT NULL,
    code_hash    TEXT        NOT NULL UNIQUE CHECK (code_hash ~ '^[0-9a-f]{64}$'),
    target       JSONB       NOT NULL CHECK (jsonb_typeof(target) = 'object'),
    purpose      TEXT        NOT NULL CHECK (purpose ~ '^[a-z_]{1,32}$'),
    stepped_up   BOOLEAN     NOT NULL,
    minted_at    TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    redeemed_at  TIMESTAMPTZ,
    CONSTRAINT plugin_handoffs_plugin_owner_fkey FOREIGN KEY (plugin_id, owner_id)
        REFERENCES public.plugins (id, owner_id) ON DELETE CASCADE,
    CONSTRAINT plugin_handoffs_expiry_check CHECK (expires_at > minted_at)
);

CREATE INDEX IF NOT EXISTS plugin_handoffs_owner_minted
    ON public.plugin_handoffs (owner_id, minted_at);

ALTER TABLE public.plugin_handoffs ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.plugin_handoffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.plugin_handoffs FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.plugin_handoffs;
CREATE POLICY owner_isolation ON public.plugin_handoffs
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.plugin_handoffs TO muster_app;
