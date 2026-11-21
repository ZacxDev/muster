-- muster 0173 — THE SERVER'S THEME (internal/theme, internal/branding;
-- docs/ui-kit.md#the-theme).
--
--   instance_branding.theme_preset   the preset an admin chose, by name
--       (internal/theme.Presets), or NULL for the default. The NAME is
--       checked here only for its shape; which names exist is muster's (Go),
--       so a later image may add one without a migration.
--   instance_branding.theme_accent   the admin's own accent colour, "#rrggbb"
--       in lower case, or NULL for the preset's own. muster derives the
--       label and focus colours from it and checks the whole theme against
--       the contrast table before it writes this column (theme.Validate,
--       branding.Stored.Next); nothing that fails that table is stored.
--
-- Both NULL — the existing row, a row an older image writes, and no row at
-- all — is the default theme: every page draws exactly the palette it drew
-- before this migration, and links no theme stylesheet.
--
-- New columns on a table that is not owned (0105): its forced scope_all
-- policy covers them, and nothing re-applies it. Must apply after 0172 (the
-- contiguity test refuses a gap). The number was the next free one on main
-- when written; it may need renumbering at merge. Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image reads neither column and draws the default theme):
--   ALTER TABLE public.instance_branding DROP COLUMN IF EXISTS theme_accent;
--   ALTER TABLE public.instance_branding DROP COLUMN IF EXISTS theme_preset;
--   DELETE FROM schema_migrations WHERE version = 173;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.instance_branding
    ADD COLUMN IF NOT EXISTS theme_preset TEXT CHECK (theme_preset IS NULL OR theme_preset ~ '^[a-z]{1,32}$'),
    ADD COLUMN IF NOT EXISTS theme_accent TEXT CHECK (theme_accent IS NULL OR theme_accent ~ '^#[0-9a-f]{6}$');
