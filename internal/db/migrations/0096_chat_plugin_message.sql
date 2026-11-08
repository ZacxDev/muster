-- muster 0096 — WHICH PLUGIN MESSAGE A CHAT ROW RELAYS (PR #527, round 0 Q1).
--
--   chat_messages.plugin_message_id   the plugin message (plugin_messages.id)
--       a USER row relays to the chief: written by internal/plugins Deliverer
--       — the delivered turn (Deliver) and a message handed over without a turn
--       (Handed) — and by nothing else. The agent page draws such a row as a
--       collapsed system row, never as the owner's bubble (internal/api
--       relayOf, ui.relayRow). NULL for every other row, and for every row
--       written before this migration: nothing is backfilled. For those the
--       page falls back to reading muster's own relay preface
--       (plugins.ParseRelayTurn) on an untyped user row.
--
-- Not a foreign key, as typed_by (0081) is not: a chat row is its owner's under
-- row-level security, and the link is a label for drawing the row — a plugin
-- message that is ever removed must not take the conversation's history with
-- it, nor be blocked by it. No index: nothing reads by it; it is read with the
-- row.
--
-- Must apply after 0095 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the column inherits the
-- table's row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0096 never writes or reads the column and runs
-- against this schema unchanged (its relay rows are drawn by the preface
-- fallback). To remove it:
--   ALTER TABLE chat_messages DROP COLUMN IF EXISTS plugin_message_id;
--   DELETE FROM schema_migrations WHERE version = 96;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_messages ADD COLUMN IF NOT EXISTS plugin_message_id BIGINT;
