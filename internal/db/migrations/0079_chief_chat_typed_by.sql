-- muster 0079 — WHO TYPED A CHAT TURN, AND WHERE A NUDGE'S QUOTE CAME FROM
-- (docs/design/session-state.md, "Covering words", step 3b-1; internal/api
-- chief_chat_quotes.go).
--
--   chat_messages.typed_by   the user who TYPED this turn into muster's chat:
--       written at the one place a person's line becomes a turn
--       (runEnteredTurn), from the owner's signed-in chat doors only
--       (ownerTypedBy: the chat page's socket and the composer's POST, on a
--       browser session — web or phone). NULL for every other writer: a
--       relayed answer, a session question or `chief ask` over a token, a
--       plugin message, a task event, an agent's or muster's own turn, and every
--       assistant row. TestEveryChatWriterIsClassified pins the writers.
--       NULL on every row written before this migration too: nothing is
--       backfilled, so no older turn counts as typed (operator decision 13).
--   chief_nudges.quote_source where a nudge's quote was checked: '' (no quote,
--       or a row from before 0079), 'session' (the candidate session's own
--       owner messages, as always) or 'chief_chat' (an owner-typed turn of the
--       owner's Chief chat). The Chief nudges tab shows it; the paste gate reads
--       it (MUSTER_CHIEF_NUDGE_CHIEF_CHAT_QUOTES_LIVE_OWNERS).
--
-- typed_by is not a foreign key: a chat row is its owner's under row-level
-- security, and users are never deleted (disabled_at). The partial index serves
-- the sweep's one read — an owner's typed turns on the chief, newest first —
-- and holds no row of any other writer.
--
-- Must apply after 0078 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); both columns inherit
-- their tables' row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0079 never writes or reads either column and
-- runs against this schema unchanged (a decision it records has quote_source
-- ''). To remove them:
--   DROP INDEX IF EXISTS chat_messages_typed_by;
--   ALTER TABLE chat_messages DROP COLUMN IF EXISTS typed_by;
--   ALTER TABLE chief_nudges DROP COLUMN IF EXISTS quote_source;
--   DELETE FROM schema_migrations WHERE version = 79;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_messages ADD COLUMN IF NOT EXISTS typed_by BIGINT;

CREATE INDEX IF NOT EXISTS chat_messages_typed_by
    ON public.chat_messages (agent_id, typed_by, id) WHERE typed_by IS NOT NULL;

ALTER TABLE public.chief_nudges ADD COLUMN IF NOT EXISTS quote_source TEXT NOT NULL DEFAULT '';

ALTER TABLE public.chief_nudges DROP CONSTRAINT IF EXISTS chief_nudges_quote_source_check;
ALTER TABLE public.chief_nudges ADD CONSTRAINT chief_nudges_quote_source_check
    CHECK (quote_source IN ('', 'session', 'chief_chat'));
