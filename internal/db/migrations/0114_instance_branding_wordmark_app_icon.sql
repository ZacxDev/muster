-- muster 0114 — A WORDMARK LOGO, AND AN APP ICON OF ITS OWN (internal/branding;
-- docs/branding.md#a-logo-that-spells-the-name, #an-app-icon-of-its-own).
--
--   instance_branding.logo_shows_name   an admin's statement that the logo is
--       a wordmark: it already spells the display name. When it is true AND a
--       logo is set, the header, the sidebar and the sign-in page draw the
--       logo alone (its alt text is the name, so the name is still announced)
--       instead of the logo followed by the name. Without a logo it changes
--       nothing. The name itself is kept and still used everywhere else: the
--       tab title, GET /api/instance, the apps' server switcher.
--       NOT NULL DEFAULT false, so the existing row (and every row an older
--       image writes) reads as "draw the name", exactly as before.
--   instance_branding.appicon192_png, appicon512_png
--       the square icons muster drew from a separate APP ICON upload
--       (branding.ProcessIcon), or NULL for none. Both or neither
--       (instance_branding_app_icon_whole), and only with a logo
--       (instance_branding_app_icon_needs_logo): it replaces the logo's icons,
--       it is not a brand on its own. While they are set, /brand/icon-192.png
--       and /brand/icon-512.png serve THEM, and logo_etag is the sha256 over
--       logo_png ‖ appicon192_png ‖ appicon512_png, so every ?v= link rolls
--       when the icon changes. icon192_png and icon512_png keep the icons drawn
--       from the logo, which removing the app icon serves again.
--
-- New columns on a table that is not owned (0105): its forced scope_all
-- policy covers them, and nothing re-applies it. Must apply after 0113 (the
-- contiguity test refuses a gap). The number was the next free one on main
-- when written; it may need renumbering at merge. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image reads none of the columns, draws the name, and serves the logo's icons — whose ETag the first statement restores):
--   UPDATE public.instance_branding SET logo_etag = substr(encode(sha256(logo_png || icon192_png || icon512_png), 'hex'), 1, 16) WHERE appicon192_png IS NOT NULL;
--   ALTER TABLE public.instance_branding DROP CONSTRAINT IF EXISTS instance_branding_app_icon_needs_logo;
--   ALTER TABLE public.instance_branding DROP CONSTRAINT IF EXISTS instance_branding_app_icon_whole;
--   ALTER TABLE public.instance_branding DROP COLUMN IF EXISTS appicon512_png;
--   ALTER TABLE public.instance_branding DROP COLUMN IF EXISTS appicon192_png;
--   ALTER TABLE public.instance_branding DROP COLUMN IF EXISTS logo_shows_name;
--   DELETE FROM schema_migrations WHERE version = 114;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.instance_branding
    ADD COLUMN IF NOT EXISTS logo_shows_name BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS appicon192_png  BYTEA,
    ADD COLUMN IF NOT EXISTS appicon512_png  BYTEA;

ALTER TABLE public.instance_branding DROP CONSTRAINT IF EXISTS instance_branding_app_icon_whole;
ALTER TABLE public.instance_branding ADD CONSTRAINT instance_branding_app_icon_whole
    CHECK ((appicon192_png IS NULL) = (appicon512_png IS NULL));
ALTER TABLE public.instance_branding DROP CONSTRAINT IF EXISTS instance_branding_app_icon_needs_logo;
ALTER TABLE public.instance_branding ADD CONSTRAINT instance_branding_app_icon_needs_logo
    CHECK (appicon192_png IS NULL OR logo_png IS NOT NULL);
