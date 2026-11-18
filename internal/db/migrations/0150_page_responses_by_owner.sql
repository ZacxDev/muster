-- muster 0150 — THE OWNER'S OWN RESPONSES TO A PAGE (internal/pages;
-- docs/pages.md "The owner's view").
--
--   page_responses.by_owner   the page's OWNER sent it, from their own view
--       of the page (/pages/{id}/view), signed in — not a viewer through a
--       share link. Such a row names no link: link_id is NULL.
--   page_responses.link_id    now NULL for an owner's response, and only for
--       one (page_responses_one_responder): every other row still names the
--       link it came through, by (link_id, owner_id) as before. A composite
--       foreign key with a NULL column is not checked (MATCH SIMPLE), and that
--       is exactly the owner's row.
--
-- The owner's responses are counted against the same caps a link's are
-- (pages.MaxResponsesPerLink, MaxUploadsPerLink), over the page's owner rows.
--
-- 0116's owner_isolation policy covers the new column: it is a column of an
-- owned table, so rollback/rls_off.sql and --rls-on need nothing new.
--
-- Inert without its code: an older image never writes a NULL link_id, and
-- reads by_owner nowhere. But an older image DOES scan link_id into an int64,
-- so it must find no owner row; and THIS image reads and writes by_owner, so
-- the column stays until it is gone. Hence the order below.
--
-- Must apply after its predecessor, 0149_officer_chat_marks (the contiguity
-- test refuses a gap). 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK, in this order:
--   1. DELETE FROM public.page_responses WHERE by_owner;
--   2. Roll the image back.
--   3. DELETE FROM public.page_responses WHERE by_owner;
--      (again: the rows the new image wrote between 1 and 2)
--   4. ALTER TABLE public.page_responses DROP CONSTRAINT IF EXISTS page_responses_one_responder;
--      ALTER TABLE public.page_responses DROP COLUMN IF EXISTS by_owner;
--      ALTER TABLE public.page_responses ALTER COLUMN link_id SET NOT NULL;
--      DELETE FROM schema_migrations WHERE version = 150;
-- (An owner's upload row deleted above leaves its file under pages/<id>/,
-- which the page's own Delete still removes: it lists the prefix.)

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.page_responses ADD COLUMN IF NOT EXISTS by_owner BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE public.page_responses ALTER COLUMN link_id DROP NOT NULL;

-- A response is a link's or the owner's: never both, never neither.
ALTER TABLE public.page_responses DROP CONSTRAINT IF EXISTS page_responses_one_responder;
ALTER TABLE public.page_responses ADD CONSTRAINT page_responses_one_responder CHECK ((link_id IS NULL) = by_owner);
