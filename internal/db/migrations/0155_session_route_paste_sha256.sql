-- muster 0155 — A ROUTE RECORDS THE SHA-256 OF THE BLOCK IT PASTED
-- (internal/plugins routes.go Route.PasteSHA256; internal/api
-- session_routes.go injectRoute writes it, route_answer_cards.go
-- routedPasteOnRecord reads it).
--
--   session_routes.paste_sha256   lower-case hex SHA-256 of the routed block
--       muster queued into the target session, as plugins.RoutedPasteSHA256
--       normalises it (CRLF and lone CR folded to LF, the Claude Code CLI's
--       `<pasted_content` escaping undone, trailing white space trimmed), set
--       with send_queue_id when the paste is queued; NULL for a route that
--       pasted nothing or was made before this migration.
--
-- WHY: a mirrored session's view draws a routed paste as a "From …" card. The
-- block's SHAPE is not evidence muster wrote it — an agent or any process on
-- the host can type one into the pane. The view draws the card only for a
-- prompt whose route exists, targets that session, was delivered (its
-- send-queue row sent, the prompt no earlier than the delivery), and whose
-- text hashes to this value; anything else is the ordinary bubble.
--
-- Not ADD COLUMN, then ADD CONSTRAINT … NOT VALID and VALIDATE: the migrator
-- runs each file in ONE transaction (internal/db migrate.go; 0146 records the
-- same reasoning), so the ALTER's ACCESS EXCLUSIVE lock is held to commit
-- either way and a separate VALIDATE would shorten nothing. The new column is
-- NULL on every existing row, so the check's scan finds nothing to refuse, and
-- session_routes holds only routes (each ends within hours).
--
-- Idempotent; not re-applied by db.RLSOn (no new table or policy: 0021's
-- policy and grants cover the column).
--
-- Must apply after its predecessor, 0154_plugin_autostart (the contiguity test
-- refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   ALTER TABLE public.session_routes DROP COLUMN IF EXISTS paste_sha256;
--   DELETE FROM schema_migrations WHERE version = 155;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS paste_sha256 TEXT
    CHECK (paste_sha256 IS NULL OR paste_sha256 ~ '^[0-9a-f]{64}$');
