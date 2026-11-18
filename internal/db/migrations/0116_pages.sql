-- muster 0116 — SHAREABLE PAGES (internal/pages; docs/pages.md, and the design
-- docs/design/pages.md).
--
--   pages           one page an agent made for its owner: its title, which
--       version is current, and who made it (a dispatched agent, or a mirrored
--       session's HOST — the host is what a mirror token proves).
--   page_versions   every version of a page, IMMUTABLE once written: its title
--       and its blocks (markdown, built-in components, custom html). A
--       republish adds a version; a rollback moves pages.current_version.
--   page_links      a share link: a 256-bit capability (/s/<token>). 🔴 ONLY
--       ITS SHA-256 IS STORED (token_sha256, the lookup): the token itself is
--       shown once — to the agent that asked for it, or to the owner who made
--       it — and is in no column, so reading this table yields no live link
--       (an agent's link is in its own transcript: page_share_request's result).
--       Its state is 'pending' (an agent asked; the owner has not decided),
--       'live', 'denied' or 'revoked'; ask_id is the owner card that decides a
--       pending one.
--   page_responses  what a viewer sent through a link: a component's submit
--       ('submit', data) or a file ('upload', the uploads row's public id).
--   uploads.object_key  may also be 'pages/<page id>/<public id>': a file
--       sent through a page's link is kept under its page's own prefix, which
--       the bucket's 'uploads/' lifecycle rule does not reach, until the page
--       is deleted (which deletes its objects). The key is still the random id
--       behind a fixed prefix; nothing a viewer chose is part of it.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0089's (asks) SHAPE. The one
-- system-scope read is the link lookup by token hash (pages.PGStore.LinkByToken,
-- System-ledgered), which learns the owner the request then acts as.
--
-- 🔴 A CHILD NAMES ITS PAGE BY (page_id, owner_id) (0006's composite-key rule),
-- so no row can name another owner's page; pages gains the unique index on
-- (id, owner_id) the references need. A response names its link the same way.
--
-- 🔴 A VERSION IS NEVER REWRITTEN: the store has no UPDATE of page_versions
-- (or of page_responses), so what a link once served at a version stays what
-- it was. The grants are the usual four, so row-level security alone scopes
-- every statement (the RLS walk drives UPDATE on every owned table).
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN pages EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT.
--
-- Must apply after 0115 (the contiguity test refuses a gap). 🔴 RENUMBER IT
-- WITH db.pagesMigrationVersion if it is renumbered at merge.
--
-- ROLLBACK (after rolling the image back; an older image never reads these):
--   DROP TABLE IF EXISTS public.page_responses;
--   DROP TABLE IF EXISTS public.page_links;
--   DROP TABLE IF EXISTS public.page_versions;
--   DROP TABLE IF EXISTS public.pages;
--   DELETE FROM public.uploads WHERE object_key LIKE 'pages/%';
--   ALTER TABLE public.uploads DROP CONSTRAINT IF EXISTS uploads_object_key_check;
--   ALTER TABLE public.uploads ADD CONSTRAINT uploads_object_key_check CHECK (object_key = 'uploads/' || public_id);
--   DELETE FROM schema_migrations WHERE version = 116;
-- 🔴 AND THE OBJECTS: the statements above remove the pages/% upload ROWS, not
-- their bytes. Delete them with the store's own tool (a shell command, not
-- SQL): mc rm --recursive --force <alias>/<bucket>/pages/

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.pages (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    title           TEXT        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    current_version INTEGER     NOT NULL DEFAULT 1 CHECK (current_version >= 1),
    agent_id        BIGINT,
    mirror_host     TEXT        NOT NULL DEFAULT '' CHECK (char_length(mirror_host) <= 200),
    creator_label   TEXT        NOT NULL DEFAULT '' CHECK (char_length(creator_label) <= 200),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A page has one creator: an agent, or a mirrored host.
    CONSTRAINT pages_one_creator CHECK ((agent_id IS NOT NULL) <> (mirror_host <> ''))
);

ALTER TABLE public.pages ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE UNIQUE INDEX IF NOT EXISTS pages_id_owner_key ON public.pages (id, owner_id);
CREATE INDEX IF NOT EXISTS pages_owner_updated ON public.pages (owner_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS public.page_versions (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id    BIGINT      NOT NULL
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    page_id     BIGINT      NOT NULL,
    version     INTEGER     NOT NULL CHECK (version >= 1),
    title       TEXT        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    blocks      JSONB       NOT NULL CHECK (jsonb_typeof(blocks) = 'array'),
    size_bytes  INTEGER     NOT NULL CHECK (size_bytes BETWEEN 0 AND 524288),
    created_by  TEXT        NOT NULL DEFAULT '' CHECK (char_length(created_by) <= 200),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT page_versions_page_fkey FOREIGN KEY (page_id, owner_id)
        REFERENCES public.pages (id, owner_id) ON DELETE CASCADE,
    CONSTRAINT page_versions_one_per_number UNIQUE (page_id, version)
);

ALTER TABLE public.page_versions ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE TABLE IF NOT EXISTS public.page_links (
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    page_id       BIGINT      NOT NULL,
    token_sha256  TEXT        NOT NULL CHECK (token_sha256 ~ '^[0-9a-f]{64}$'),
    state         TEXT        NOT NULL CHECK (state IN ('pending', 'live', 'denied', 'revoked')),
    ask_id        BIGINT,
    requested_by  TEXT        NOT NULL DEFAULT '' CHECK (char_length(requested_by) <= 200),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at    TIMESTAMPTZ,
    CONSTRAINT page_links_page_fkey FOREIGN KEY (page_id, owner_id)
        REFERENCES public.pages (id, owner_id) ON DELETE CASCADE,
    -- A pending link has not been decided; every other state has.
    CONSTRAINT page_links_decided CHECK ((state = 'pending') = (decided_at IS NULL))
);

ALTER TABLE public.page_links ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE UNIQUE INDEX IF NOT EXISTS page_links_token_sha256_key ON public.page_links (token_sha256);
CREATE UNIQUE INDEX IF NOT EXISTS page_links_id_owner_key ON public.page_links (id, owner_id);
CREATE INDEX IF NOT EXISTS page_links_page ON public.page_links (page_id, created_at DESC);
CREATE INDEX IF NOT EXISTS page_links_ask ON public.page_links (owner_id, ask_id) WHERE ask_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS public.page_responses (
    id               BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id         BIGINT      NOT NULL
                     DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                     REFERENCES public.users (id),
    page_id          BIGINT      NOT NULL,
    link_id          BIGINT      NOT NULL,
    version          INTEGER     NOT NULL CHECK (version >= 1),
    component        TEXT        NOT NULL CHECK (component ~ '^[A-Za-z0-9_-]{1,64}$'),
    kind             TEXT        NOT NULL CHECK (kind IN ('submit', 'upload')),
    data             JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (pg_column_size(data) <= 40000),
    upload_public_id TEXT        NOT NULL DEFAULT '' CHECK (char_length(upload_public_id) <= 64),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT page_responses_page_fkey FOREIGN KEY (page_id, owner_id)
        REFERENCES public.pages (id, owner_id) ON DELETE CASCADE,
    CONSTRAINT page_responses_link_fkey FOREIGN KEY (link_id, owner_id)
        REFERENCES public.page_links (id, owner_id) ON DELETE CASCADE,
    -- An upload names its file; a submit does not.
    CONSTRAINT page_responses_upload_names_file CHECK ((kind = 'upload') = (upload_public_id <> ''))
);

ALTER TABLE public.page_responses ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS page_responses_page ON public.page_responses (page_id, id);
CREATE INDEX IF NOT EXISTS page_responses_link ON public.page_responses (link_id, created_at);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['pages', 'page_versions', 'page_links', 'page_responses'] LOOP
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

-- A page's files: 'pages/<page id>/<public id>' beside 'uploads/<public id>'.
ALTER TABLE public.uploads DROP CONSTRAINT IF EXISTS uploads_object_key_check;
ALTER TABLE public.uploads ADD CONSTRAINT uploads_object_key_check CHECK (
    object_key = 'uploads/' || public_id
    OR (object_key ~ '^pages/[1-9][0-9]{0,18}/[A-Za-z0-9_-]{22}$' AND right(object_key, 23) = '/' || public_id));

GRANT SELECT, INSERT, UPDATE, DELETE ON public.pages TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.page_versions TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.page_links TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.page_responses TO muster_app;
