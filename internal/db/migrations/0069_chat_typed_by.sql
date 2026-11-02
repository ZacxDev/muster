-- muster 0069 — WHO TYPED a chat message (chat_messages.typed_by).
--
--   * chat_messages.typed_by: the user who typed this message into muster's
--     own signed-in chat — the agent page's socket or its composer POST
--     (internal/api runEnteredTurn, called by handleAgentWS and
--     handleAgentSend only). NULL for EVERY other row: a plugin delivery, an
--     attention answer relayed into the chat, a route note or hand-back, a
--     kickoff, a nudge, a forward, `chief ask` over the machine token, a job,
--     an officer or relay turn, and every assistant row. Every row written
--     before this migration is NULL too: nothing older can be claimed as typed.
--
-- 🔴 WHAT READS IT: task_dispatch's gate release (internal/api
-- chief_owner_release.go). A gated task — the only way a plugin message
-- becomes work — is released by the chief only when the newest user-side
-- message in the chief's chat was typed by its owner. So NULL is the SAFE
-- value: a writer that forgets the column can never release a gate, and the
-- only way to write a non-NULL value is the signed-in chat (the writer ledger
-- TestOnlyTheOwnersChatDoorsStampTypedBy pins it).
--
-- No foreign key and no CHECK, deliberately: the reader compares typed_by with
-- the chief's owner itself, and a constraint here would turn a mismatch into a
-- failed INSERT — which every chat writer discards — so the owner's message
-- would be LOST rather than merely not count.
--
-- The column inherits the table's ownership, grants and row-level security;
-- no policy changes.
--
-- Rollback: an image from before 0069 never writes or reads typed_by, so it
-- simply cannot release a gate by an owner's message (the board's release
-- still works). Then:
--   DROP INDEX IF EXISTS public.idx_chat_messages_agent_user_turns;
--   ALTER TABLE public.chat_messages DROP COLUMN typed_by;
--   DELETE FROM schema_migrations WHERE version = 69;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_messages ADD COLUMN IF NOT EXISTS typed_by BIGINT;

-- The gate release reads an agent's newest user-side rows (agents.PGStore
-- LatestUserTurns).
CREATE INDEX IF NOT EXISTS idx_chat_messages_agent_user_turns
    ON public.chat_messages (agent_id, id) WHERE role = 'user';
