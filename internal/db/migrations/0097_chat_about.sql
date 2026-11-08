-- muster 0097 — WHAT A CHIEF-CHAT LINE IS ABOUT (docs/design/chief-in-agent-view.md,
-- phase 1, section 3.1).
--
--   chat_messages.about   the session the owner was VIEWING when they typed
--       this line to the chief from that session's rendered view (POST
--       /mirror/view/chief): {session_key, runtime, host, tmux_session,
--       tmux_window, tmux, title, dispatch, repo, base, branch, model},
--       resolved by the SERVER from the hub's owner-scoped state
--       (internal/api viewChiefSubject), never taken from the page. Written by
--       that one door, through runEnteredTurn, on the user row it stores, and
--       by nothing else (internal/api TestEveryChatWriterIsClassified and
--       TestOnlyTheViewDoorBindsASubject pin the writers). NULL for every other
--       row and for every row written before this migration: nothing is
--       backfilled.
--
-- chat_messages.content keeps only what the owner typed; the header the chief
-- reads ("[muster] Your owner is viewing …") is rendered from this column when
-- the turn is delivered, so the owner's words, quoted later as covering words
-- (chief_chat_quotes.go), are exactly what they typed.
--
-- Not a foreign key and not indexed, as plugin_message_id (0096) is not: it is
-- a label read with its row (the view's chief strip filters a session's rows
-- by its session_key, among at most chatHistoryCap rows already read).
--
-- Must apply after 0096 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the column inherits the
-- table's row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0097 never writes or reads the column and runs
-- against this schema unchanged (its inserts name their columns). Rows written
-- by a newer image keep their about, which an older image ignores; the chief's
-- conversation already holds the header it was delivered with. To remove it
-- (the view's chief strip then shows nothing, and the chief reads no header):
--   ALTER TABLE chat_messages DROP COLUMN IF EXISTS about;
--   DELETE FROM schema_migrations WHERE version = 97;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_messages ADD COLUMN IF NOT EXISTS about JSONB;
