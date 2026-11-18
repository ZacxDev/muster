-- muster 0069 — THE OWNER'S TURN IN FLIGHT (agents.owner_turn_*).
--
--   * agents.owner_turn_message_id / owner_turn_by / owner_turn_expires_at /
--     owner_turn_lane: set while a chat turn the agent's OWNER typed into
--     muster's signed-in chat (internal/api runEnteredTurn, for handleAgentWS
--     and handleAgentSend only) is running in the agent's ccd — from the
--     moment ccd ACCEPTS the text (agents.MarkTurnAccepted: its 103 after the
--     CLI's UserPromptSubmit, naming the lane) until muster's turn call
--     returns, whatever it returns. A runtime that refuses the text (409 busy,
--     503 not_ready) never marks. owner_turn_lane is the ccd lane (the chat
--     session key) the turn runs in, '' for ccd's primary CLI.
--
-- 🔴 WHAT READS IT: task_dispatch's owner_asked (internal/api
-- chief_owner_release.go). A gated task the chief filed is released only while
-- the chief is running a turn its owner typed. NULL is the safe value: no
-- marker, no release.
--
-- 🔴 ON THE ROW, NOT IN A PROCESS, because muster may run more than one replica
-- and restart: the turn may run on one replica while the chief's MCP call lands
-- on another. The expiry is the turn's own budget (agentTurnBudget), so a crash
-- between set and clear cannot leave it set for longer than a turn could last.
-- Reads compare it with now() in Postgres, as the session claim does.
--
-- One marker per agent: a second owner turn overwrites the first, and a clear
-- names its own message id, so it never clears another turn's marker.
--
-- The columns inherit the table's row-level security; no policy changes.
--
-- Rollback: an image from before 0069 never sets or reads them, so owner_asked
-- does not exist there. Then:
--   ALTER TABLE public.agents DROP COLUMN owner_turn_message_id,
--     DROP COLUMN owner_turn_by, DROP COLUMN owner_turn_expires_at,
--     DROP COLUMN owner_turn_lane;
--   DELETE FROM schema_migrations WHERE version = 69;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS owner_turn_message_id BIGINT;
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS owner_turn_by BIGINT;
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS owner_turn_expires_at TIMESTAMPTZ;
-- The ccd lane (chat session key) the owner's turn runs in; '' = ccd's primary
-- CLI. owner_asked requires every OTHER CLI in the pod to be idle.
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS owner_turn_lane TEXT;
