-- muster 0153 — A ROUTE RECORDS THE SHA-256 OF THE BLOCK IT PASTED
-- (internal/plugins routes.go Route.PasteSHA256; internal/api
-- session_routes.go injectRoute writes it, routed_prompt_cards.go reads it).
--
--   session_routes.paste_sha256   lower-case hex SHA-256 of the routed block
--       muster queued into the target session (plugins.RoutedPasteSHA256: the
--       text with trailing white space trimmed), set with send_queue_id when
--       the paste is queued; NULL for a route that pasted nothing (a dispatched
--       agent's target) or was made before this migration.
--
-- WHY: a mirrored session's view draws a routed paste as a "From your chief"
-- card. The block's SHAPE is not evidence muster wrote it — an agent or any
-- process on the host can type one into the pane. The view draws the card only
-- for a prompt whose route exists, targets that session, and whose text hashes
-- to this value; anything else is the ordinary bubble.
--
-- Idempotent; not re-applied by db.RLSOn (no new table or policy: 0021's
-- policy and grants cover the column).
--
-- Must apply after its predecessor, 0152_page_owner_view (the contiguity test
-- refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   ALTER TABLE public.session_routes DROP COLUMN IF EXISTS paste_sha256;
--   DELETE FROM schema_migrations WHERE version = 153;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS paste_sha256 TEXT
    CHECK (paste_sha256 IS NULL OR paste_sha256 ~ '^[0-9a-f]{64}$');
