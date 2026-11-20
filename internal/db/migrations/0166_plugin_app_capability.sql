-- muster 0166 — THE `app` CAPABILITY (docs/design/meetings.md, "The core
-- change: the `app` capability"; docs/plugins.md#app-tab).
--
--   plugins.capabilities 'app'   the plugin may have a tab in its owner's
--       sidebar that opens its page full width (core PR 1b draws it). Chosen
--       on /plugins only, never by a pairing, and fixed at mint.
--   plugins.app_label   the tab's label, the OWNER's words: 1..24 characters
--       (internal/plugins ValidAppLabel says which). NULL on every other token.
--   plugins.app_icon    the tab's icon, a KEY into muster's fixed list
--       (internal/plugins AppIcons), never the emoji itself. NULL likewise.
--   plugins_viewer_link   now: an origin and entry URL are set exactly when
--       the token has `viewer` OR `app` (an `app` token carries them as a
--       `viewer` token does: the same columns and the same
--       plugins.ValidViewerLink rule at mint).
--   plugins_app_tab       app_label and app_icon are set exactly when the
--       token has `app`.
--
-- Every existing token is untouched: none has `app`, so each satisfies the
-- widened plugins_viewer_link exactly as it satisfied the old one, and both
-- new columns stay NULL.
--
-- The at-most-five live `app` plugins per owner, and person-owned only, are
-- the mint's (internal/plugins createPluginTx), not constraints here.
--
-- 🔴 plugins_capabilities_known IS A WHOLE LIST: a migration that adds a
-- capability rewrites it. If another such migration lands first, renumber this
-- one AND add that capability to the list below;
-- TestEveryKnownCapabilityIsStorable fails until the list matches the Go one.
--
-- A new column on an owned table: the table's row-level security covers it;
-- not re-applied by db.RLSOn. Must apply after 0165 (the contiguity test
-- refuses a gap); 🔴 RENUMBER IT if another migration takes 0166 first.
-- Idempotent.
--
-- ROLLBACK (revoke every app token FIRST, then roll the image back: an older
-- image neither knows `app` nor reads the columns):
--   UPDATE public.plugins SET revoked_at = now() WHERE 'app' = ANY (capabilities) AND revoked_at IS NULL;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_app_tab;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_viewer_link;
--   UPDATE public.plugins SET origin = NULL, entry_url = NULL
--    WHERE 'app' = ANY (capabilities) AND NOT ('viewer' = ANY (capabilities));
--   UPDATE public.plugins SET capabilities = '{messages}' WHERE capabilities = '{app}';
--   UPDATE public.plugins SET capabilities = array_remove(capabilities, 'app') WHERE 'app' = ANY (capabilities);
--   ALTER TABLE public.plugins DROP COLUMN IF EXISTS app_label, DROP COLUMN IF EXISTS app_icon;
--   ALTER TABLE public.plugins ADD CONSTRAINT plugins_viewer_link
--     CHECK ((origin IS NULL) = (entry_url IS NULL) AND (origin IS NOT NULL) = ('viewer' = ANY (capabilities)));
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
--   ALTER TABLE public.plugins ADD CONSTRAINT plugins_capabilities_known
--     CHECK (capabilities <@ ARRAY['messages', 'viewer', 'draft_checks', 'chief_extension']::text[]);
--   DELETE FROM schema_migrations WHERE version = 166;
-- (An app-only plugin cannot keep an empty set, plugins_capabilities_nonempty:
-- it is revoked first, and its revoked row then names messages, which a
-- revoked token never opens. Its origin goes with `app`, or the restored
-- plugins_viewer_link would refuse the row.)

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS app_label TEXT;
ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS app_icon TEXT;

ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_capabilities_known
    CHECK (capabilities <@ ARRAY['messages', 'viewer', 'draft_checks', 'chief_extension', 'app']::text[]);

ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_viewer_link;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_viewer_link
    CHECK ((origin IS NULL) = (entry_url IS NULL)
       AND (origin IS NOT NULL) = ('viewer' = ANY (capabilities) OR 'app' = ANY (capabilities)));

ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_app_tab;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_app_tab
    CHECK ((app_label IS NULL) = (app_icon IS NULL)
       AND (app_label IS NOT NULL) = ('app' = ANY (capabilities))
       AND (app_label IS NULL OR char_length(app_label) BETWEEN 1 AND 24)
       AND (app_icon IS NULL OR app_icon ~ '^[a-z]{1,16}$'));
