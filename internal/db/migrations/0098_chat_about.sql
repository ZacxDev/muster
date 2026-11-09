-- muster 0098 — WHICH SESSION A CHIEF-CHAT LINE IS ABOUT
-- (docs/design/chief-in-agent-view.md, phase 1, section 3.1).
--
--   chat_messages.about   the KEY of the session the owner was VIEWING when they
--       typed this line to the chief from that session's rendered view (POST
--       /mirror/view/chief): a Claude Code session id, or "opencode:<ses_…>".
--       Resolved by the SERVER from the hub's owner-scoped state (internal/api
--       viewChiefSubject), never taken from the page. Written by that one door,
--       through runEnteredTurn, on the user row it stores, and by nothing else
--       (internal/api TestOnlyTheViewDoorBindsASubject pins the writers). NULL
--       for every other row and for every row written before this migration:
--       nothing is backfilled.
--
-- 🔴 THE KEY ONLY (operator decision, PR #537 round 0 D1). The window's place,
-- name, repository and dispatch are not stored: the header the chief reads
-- ("[muster] Your owner is viewing …") is built from the view as it is when the
-- line is sent, and handed to the runtime only. chat_messages.content keeps
-- only what the owner typed, so their words, quoted later as covering words
-- (chief_chat_quotes.go), are exactly what they typed.
--
-- Not a foreign key and not indexed, as plugin_message_id (0096) is not: it is
-- a label read with its row (the view's chief strip filters a session's rows by
-- it, among at most chatHistoryCap rows already read).
--
-- Must apply after 0097 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the column inherits the
-- table's row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0098 never writes or reads the column and runs
-- against this schema unchanged (its inserts name their columns). Rows written
-- by a newer image keep their key, which an older image ignores. To remove it
-- (the view's chief strip then shows nothing):
--   ALTER TABLE chat_messages DROP COLUMN IF EXISTS about;
--   DELETE FROM schema_migrations WHERE version = 98;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_messages ADD COLUMN IF NOT EXISTS about TEXT;
