-- muster 0114 — THE CHIEF'S PRIMARY AND SIDE CHATS (docs/chief.md#primary-and-side-chats).
--
--   chat_sessions.archived_at        when the session was archived, NULL while it
--       is not. An archived session is hidden from the chief's chat switcher;
--       its history is kept and it can be unarchived. The PRIMARY chat (the
--       agent's is_default session, 0030) is never archived: the
--       chat_sessions_primary_not_archived CHECK refuses the pair, so a fresh
--       primary clears is_default on the old one in the same transaction that
--       archives it.
--   chief_turn_usage.session_id      the chat session a chief turn ran in (the
--       chat_sessions row, by id; NULL for a turn in no stored session — a
--       recap or a stall check — and for every row before this migration).
--       What the chief chat's context chip reads: the latest row of its session.
--   chief_turn_usage.model           the model the turn's session ran on, as
--       muster knew it when the turn began ('' unknown).
--   chief_turn_usage.context_tokens  the context the turn's LAST model request
--       was sent (input + cache read + cache creation of that request alone;
--       the token columns are the turn's sums). NULL when no usage was reported,
--       and for every row before this migration. The chip reads it.
--
-- Must apply after 0113 (the contiguity test refuses a gap). New columns, one
-- CHECK and one index on owned tables: their row-level security covers them;
-- not re-applied by db.RLSOn. Idempotent.
--
-- ROLLBACK. 🔴 Drop the CHECK whenever the image is rolled back, even if the
-- columns stay: an older image elects and moves defaults without knowing
-- archived_at, and with the CHECK in place its election onto an archived
-- session fails every delivery to that agent:
--   ALTER TABLE chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_primary_not_archived;
-- Then, if the columns go too:
--   DROP INDEX IF EXISTS chief_turn_usage_session_at;
--   ALTER TABLE chief_turn_usage DROP COLUMN IF EXISTS context_tokens;
--   ALTER TABLE chief_turn_usage DROP COLUMN IF EXISTS model;
--   ALTER TABLE chief_turn_usage DROP COLUMN IF EXISTS session_id;
--   ALTER TABLE chat_sessions DROP COLUMN IF EXISTS archived_at;
--   DELETE FROM schema_migrations WHERE version = 114;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_sessions ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;

ALTER TABLE public.chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_primary_not_archived;
ALTER TABLE public.chat_sessions ADD CONSTRAINT chat_sessions_primary_not_archived
    CHECK (NOT (is_default AND archived_at IS NOT NULL));

ALTER TABLE public.chief_turn_usage ADD COLUMN IF NOT EXISTS session_id BIGINT;
ALTER TABLE public.chief_turn_usage ADD COLUMN IF NOT EXISTS model TEXT NOT NULL DEFAULT ''
    CHECK (char_length(model) <= 200);

ALTER TABLE public.chief_turn_usage ADD COLUMN IF NOT EXISTS context_tokens BIGINT
    CHECK (context_tokens IS NULL OR context_tokens >= 0);

CREATE INDEX IF NOT EXISTS chief_turn_usage_session_at
    ON public.chief_turn_usage (session_id, id) WHERE session_id IS NOT NULL;
