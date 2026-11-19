-- muster 0161 — A PAGE'S AUTHOR ASSETS (internal/pages asset.go; docs/pages.md
-- "Images and video").
--
--   page_assets   an image or video the page's AUTHOR (the agent or mirrored
--       session that made it) attached to the page: its bytes are an uploads
--       row under the page's own prefix (object_key pages/<page id>/<public
--       id>, as a viewer's file already is), named here by public_id, with
--       the type SNIFFED from the bytes (one of six: PNG, JPEG, WebP, GIF,
--       MP4, WebM) and its size (at most 15 MiB; a page's assets at most
--       100 MiB in all, counted under the page row's lock). A block refers to
--       one as `asset:<public_id>`.
--   page_version_assets   which assets each version refers to, written with
--       the version (the store reads them out of its blocks) and never
--       rewritten. It is what serves an asset: a share link serves only an
--       asset its page's CURRENT version refers to, the owner's view only one
--       the version it shows refers to.
--
-- 🔴 A VERSION CAN REFER ONLY TO ITS OWN PAGE'S ASSETS, AND KEEPS THEM ALIVE.
-- page_version_assets names its asset by (page_id, asset_id, owner_id), so a
-- version of one page cannot name another page's asset (or another owner's):
-- the foreign key refuses it. That key is NO ACTION, so an asset a version
-- still refers to cannot be deleted; deleting the PAGE deletes both sides in
-- one statement (each cascades from pages), which NO ACTION allows.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0116's SHAPE (the page
-- tables' share class). rollback/rls_off.sql turns it off with the rest, and
-- --rls-on re-applies this file (db.RLSOn), so every statement is idempotent.
--
-- Numbered 0161 because open pull requests claim 0157-0160 (main was at
-- 0156). Until they merge, the tree has a gap, which the contiguity test
-- refuses (a gap bricks a deploy that applies a higher number first). 🔴
-- RENUMBER IT (and db.pageAssetsMigrationVersion) at merge to the next free
-- number after main's newest.
--
-- ROLLBACK (after rolling the image back; an older image never reads them):
--   DROP TABLE IF EXISTS public.page_version_assets;
--   DROP TABLE IF EXISTS public.page_assets;
--   DROP INDEX IF EXISTS public.page_versions_page_version_owner_key;
--   DELETE FROM schema_migrations WHERE version = 161;
-- 🔴 AND THE OBJECTS: an asset's bytes stay under pages/<page id>/ (and its
-- uploads row stays, as a viewer's file's does) until its page is deleted.
-- An older image deletes a page's whole prefix with it, assets included, but
-- its uploads rows only for viewers' files: an asset's row is left, unserved.

SET LOCAL lock_timeout = '3s';

-- What page_version_assets' version key names (page_versions has its
-- (page_id, version) unique already; the owner makes it composite).
CREATE UNIQUE INDEX IF NOT EXISTS page_versions_page_version_owner_key
    ON public.page_versions (page_id, version, owner_id);

CREATE TABLE IF NOT EXISTS public.page_assets (
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    page_id       BIGINT      NOT NULL,
    public_id     TEXT        NOT NULL CHECK (public_id ~ '^[A-Za-z0-9_-]{22}$'),
    filename      TEXT        NOT NULL DEFAULT '' CHECK (char_length(filename) <= 255),
    content_type  TEXT        NOT NULL CHECK (content_type IN
                  ('image/png', 'image/jpeg', 'image/webp', 'image/gif', 'video/mp4', 'video/webm')),
    size_bytes    BIGINT      NOT NULL CHECK (size_bytes BETWEEN 1 AND 15728640),
    created_by    TEXT        NOT NULL DEFAULT '' CHECK (char_length(created_by) <= 200),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT page_assets_page_fkey FOREIGN KEY (page_id, owner_id)
        REFERENCES public.pages (id, owner_id) ON DELETE CASCADE
);

ALTER TABLE public.page_assets ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE UNIQUE INDEX IF NOT EXISTS page_assets_public_id_key ON public.page_assets (public_id);
CREATE UNIQUE INDEX IF NOT EXISTS page_assets_page_public_owner_key ON public.page_assets (page_id, public_id, owner_id);

CREATE TABLE IF NOT EXISTS public.page_version_assets (
    owner_id    BIGINT  NOT NULL
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    page_id     BIGINT  NOT NULL,
    version     INTEGER NOT NULL CHECK (version >= 1),
    asset_id    TEXT    NOT NULL CHECK (asset_id ~ '^[A-Za-z0-9_-]{22}$'),
    PRIMARY KEY (page_id, version, asset_id),
    CONSTRAINT page_version_assets_version_fkey FOREIGN KEY (page_id, version, owner_id)
        REFERENCES public.page_versions (page_id, version, owner_id) ON DELETE CASCADE,
    -- NO ACTION: a version keeps its asset (above).
    CONSTRAINT page_version_assets_asset_fkey FOREIGN KEY (page_id, asset_id, owner_id)
        REFERENCES public.page_assets (page_id, public_id, owner_id)
);

ALTER TABLE public.page_version_assets ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS page_version_assets_asset ON public.page_version_assets (page_id, asset_id);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['page_assets', 'page_version_assets'] LOOP
        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', t);
        EXECUTE format($p$CREATE POLICY owner_isolation ON public.%I
            AS PERMISSIVE FOR ALL TO PUBLIC
            USING (
                owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR current_setting('muster.scope', true) = 'all'
            )
            WITH CHECK (
                owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR current_setting('muster.scope', true) = 'all'
            )$p$, t);
    END LOOP;
END $$;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.page_assets TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.page_version_assets TO muster_app;
