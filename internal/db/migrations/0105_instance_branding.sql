-- muster 0105 — THE INSTANCE'S LOGO AND NAME (internal/branding;
-- docs/branding.md).
--
-- One row, or none, for the whole server: the display name and the logo an
-- admin set on the Account page, as muster serves them — the logo and its two
-- square icons, each a PNG muster re-encoded itself (branding.Process), and
-- the ETag naming all three. No row, and a row whose columns are NULL, both
-- read as "not set": every page draws the built-in muster mark, exactly as
-- before this migration.
--
--   singleton    always true, the primary key: a second row cannot exist.
--   name         the display name, 1–64 characters, or NULL.
--   logo_png, icon192_png, icon512_png, logo_etag
--                all four set or all four NULL (instance_branding_logo_whole).
--   updated_at, updated_by
--                the last change and the admin who made it.
--
-- 🔴 NOT OWNED. The branding belongs to the server, not to a user, so there
-- is no owner_id and no owner_isolation policy. It is under FORCED row-level
-- security with one policy, scope_all — the shape 0009 gives invites:
-- muster.scope = 'all' only. A user's principal reads and writes nothing
-- here; internal/branding reads it as authz.System (the sign-in page and the
-- public /brand/ images read it with no user at all) and writes it as
-- authz.System only after checking the caller is an admin (branding.PG). The
-- admin check in Go is the gate; this policy keeps any OTHER code path, run
-- with a user's principal, from reaching the row.
--
-- rollback/rls_off.sql does not touch it and db.RLSOn does not re-apply it:
-- they turn the OWNED tables' security off and on, and an image older than
-- this migration never reads this table either way. Idempotent.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). The
-- number was the next free one on main when written; it may need renumbering
-- at merge.
--
-- ROLLBACK (after rolling the image back; an older image never reads it, and draws the built-in mark):
--   DROP TABLE IF EXISTS public.instance_branding;
--   DELETE FROM schema_migrations WHERE version = 105;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.instance_branding (
    singleton    BOOLEAN     PRIMARY KEY DEFAULT true CHECK (singleton),
    name         TEXT        CHECK (name IS NULL OR char_length(name) BETWEEN 1 AND 64),
    logo_png     BYTEA,
    icon192_png  BYTEA,
    icon512_png  BYTEA,
    logo_etag    TEXT        CHECK (logo_etag IS NULL OR logo_etag ~ '^[0-9a-f]{16}$'),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by   BIGINT      REFERENCES public.users (id),
    CONSTRAINT instance_branding_logo_whole CHECK (
        (logo_png IS NULL AND icon192_png IS NULL AND icon512_png IS NULL AND logo_etag IS NULL)
        OR (logo_png IS NOT NULL AND icon192_png IS NOT NULL AND icon512_png IS NOT NULL AND logo_etag IS NOT NULL)
    )
);

ALTER TABLE public.instance_branding ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.instance_branding FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS scope_all ON public.instance_branding;
CREATE POLICY scope_all ON public.instance_branding AS PERMISSIVE FOR ALL TO PUBLIC
    USING (current_setting('muster.scope', true) = 'all')
    WITH CHECK (current_setting('muster.scope', true) = 'all');

GRANT SELECT, INSERT, UPDATE, DELETE ON public.instance_branding TO muster_app;
