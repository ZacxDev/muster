-- muster 0187 — WHEN THE OWNER LAST OPENED A CHAT SESSION
-- (internal/agents/session_viewed.go; internal/api chiefOpenSession;
-- docs/chief.md#which-session-the-chief-opens-on).
--
--   chat_sessions.viewed_at   stamped when the chat is DRAWN for its owner: the
--       agent page on that session (a real view, not a prefetch) and the chief
--       panel's body. NULL for a session nobody has opened since this column
--       existed. The chief's page, its sidebar row, home and its panel open on
--       the chief's session with the latest viewed_at — the one the owner was
--       on last, on either surface — unless it is archived (then its primary).
--
-- 🔴 NOT read_at. read_at also moves when a turn ends in an open chat (POST
-- /agents/sessions/{sid}/read) and is DEFAULT now() at insert, so "the latest
-- read_at" would name a chat nobody opened. viewed_at has exactly the two
-- writers above.
--
-- Nullable, no default: no rewrite, and every existing row reads as "never
-- opened", which leaves the chief opening on its primary until the owner opens
-- a chat — what it did before. Not re-applied by db.RLSOn (it changes no
-- policy, grant or default); the column inherits the table's row-level
-- security. Idempotent. Must apply after 0186 (the contiguity test refuses a
-- gap). 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. An older image does not read viewed_at (its column lists are
-- named, never `*`): the chief opens on its primary again. To remove it:
--   ALTER TABLE chat_sessions DROP COLUMN IF EXISTS viewed_at;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_sessions ADD COLUMN IF NOT EXISTS viewed_at TIMESTAMPTZ;
