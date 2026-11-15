-- muster 0135 — ONE REUSABLE SHARE LINK PER PAGE (docs/pages.md#sharing).
--
--   page_links.reusable       the page's one link the owner's Share button
--       hands out every time (internal/pages PGStore.ShareLink). At most one
--       is LIVE per page (page_links_one_reusable).
--   page_links.token_sealed   a reusable link's TOKEN, encrypted at rest
--       (AES-256-GCM under a key derived from MUSTER_CONNECTOR_ENCRYPTION_KEY,
--       bound to the row's owner, page and token hash — internal/pages
--       seal.go), so Share can show the same address again. 🔴 THE LOOKUP IS
--       STILL token_sha256: nothing reads this column to admit a viewer, and
--       it is wiped (NULL) the moment the link stops being live. A link made
--       before this migration has none, and stays hash-only ("legacy"): it
--       still serves and can still be revoked, but its address cannot be shown
--       again.
--   page_links.made_by_agent  the link was made by an agent (its request,
--       approved or — MUSTER_PAGES_AGENT_SHARE=auto — made live at once),
--       not by the owner. Backfilled from requested_by: the owner's own links
--       were always recorded as 'you'.
--
-- Inert without its code: an older image reads none of the columns, and every
-- link it makes is reusable = false, token_sealed NULL.
-- Must apply after its predecessor, 0134 (the contiguity test refuses a gap).
--
-- ROLLBACK, in this order:
--   1. Roll the image back.
--   2. (An older image's revoke does not wipe the column.)
--   UPDATE public.page_links SET token_sealed = NULL WHERE state <> 'live';
--   DROP INDEX IF EXISTS public.page_links_one_reusable;
--   ALTER TABLE public.page_links DROP CONSTRAINT IF EXISTS page_links_sealed_is_reusable;
--   ALTER TABLE public.page_links DROP COLUMN IF EXISTS token_sealed;
--   ALTER TABLE public.page_links DROP COLUMN IF EXISTS reusable;
--   ALTER TABLE public.page_links DROP COLUMN IF EXISTS made_by_agent;
--   DELETE FROM schema_migrations WHERE version = 135;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.page_links ADD COLUMN IF NOT EXISTS reusable BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE public.page_links ADD COLUMN IF NOT EXISTS token_sealed BYTEA;
ALTER TABLE public.page_links ADD COLUMN IF NOT EXISTS made_by_agent BOOLEAN NOT NULL DEFAULT false;

-- Only a reusable link carries a sealed token.
ALTER TABLE public.page_links DROP CONSTRAINT IF EXISTS page_links_sealed_is_reusable;
ALTER TABLE public.page_links ADD CONSTRAINT page_links_sealed_is_reusable
    CHECK (token_sealed IS NULL OR (reusable AND octet_length(token_sealed) BETWEEN 28 AND 256));

-- One live reusable link per page: two Shares racing make one link, not two.
CREATE UNIQUE INDEX IF NOT EXISTS page_links_one_reusable ON public.page_links (page_id) WHERE reusable AND state = 'live';

UPDATE public.page_links SET made_by_agent = true WHERE requested_by <> 'you' AND NOT made_by_agent;
