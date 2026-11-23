-- muster 0185 — WHERE AN OFFICER'S NEEDS-YOU ITEM CAME FROM (internal/api
-- officer_origin.go; docs/officers-builtin.md "Show session").
--
--   attention_asks.origin_*          an ask an officer's instance raised
--   plugin_reply_drafts.origin_*     a draft an officer's instance filed (a
--                                    refusal of that draft links through it)
--
--   origin_agent_id    the officer INSTANCE (agents.id) whose tool call made
--                      the row — the bound caller, never a value the caller
--                      supplied
--   origin_session_id  that instance's chat session (chat_sessions.id) the
--                      call was made in: the conversation "Show session" opens
--   origin_message_id  the prompt row (chat_messages) of the TURN the call
--                      was made in — the per-turn record, not "the newest
--                      message" — the moment it scrolls to (no FK: a message
--                      is only a position; NULL when the turn is not known)
--
-- When the call was made is the row's own created_at: no column of its own.
--
-- All three are NULL on every row made before this migration, and on every
-- row no officer instance made: "Show session" then opens the instance's
-- session as a whole, or is not drawn.
--
-- The shape: a session only with its agent, a message only with its
-- session (the trigger below). A row whose agent is deleted keeps the row and
-- loses all three.
--
-- 🔴 OWNER ISOLATION, IN THE DATABASE TOO. The application writes only its
-- bound caller; this trigger refuses, whatever wrote it, an origin agent that
-- is not the row's owner's own agent, a session that is not that agent's, or
-- a message that is not that session's. So no row can ever name another
-- owner's officer session. (Row-level security already keeps another owner
-- from reading the row; this keeps the row from pointing anywhere else.)
--
-- Must apply after its predecessor (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. An older image never reads the columns (its column lists are
-- named, never `*`). To remove them:
--   DROP TRIGGER IF EXISTS attention_asks_origin_is_owners ON public.attention_asks;
--   DROP TRIGGER IF EXISTS plugin_reply_drafts_origin_is_owners ON public.plugin_reply_drafts;
--   DROP FUNCTION IF EXISTS public.officer_origin_is_owners();
--   ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS origin_agent_id, DROP COLUMN IF EXISTS origin_session_id,
--       DROP COLUMN IF EXISTS origin_message_id;
--   ALTER TABLE public.plugin_reply_drafts DROP COLUMN IF EXISTS origin_agent_id, DROP COLUMN IF EXISTS origin_session_id,
--       DROP COLUMN IF EXISTS origin_message_id;

ALTER TABLE public.attention_asks
    ADD COLUMN IF NOT EXISTS origin_agent_id   BIGINT REFERENCES public.agents (id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS origin_session_id BIGINT REFERENCES public.chat_sessions (id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS origin_message_id BIGINT;

ALTER TABLE public.plugin_reply_drafts
    ADD COLUMN IF NOT EXISTS origin_agent_id   BIGINT REFERENCES public.agents (id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS origin_session_id BIGINT REFERENCES public.chat_sessions (id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS origin_message_id BIGINT;

CREATE OR REPLACE FUNCTION public.officer_origin_is_owners() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- No agent (none recorded, or its agent deleted: the FK sets it NULL, and
    -- the agent's sessions go with it): nothing else of the origin stands.
    IF NEW.origin_agent_id IS NULL THEN
        NEW.origin_session_id := NULL;
        NEW.origin_message_id := NULL;
        RETURN NEW;
    END IF;
    IF NEW.owner_id IS DISTINCT FROM (SELECT a.owner_id FROM public.agents a WHERE a.id = NEW.origin_agent_id) THEN
        RAISE EXCEPTION '%: origin agent % is not owner %''s', TG_TABLE_NAME, NEW.origin_agent_id, NEW.owner_id USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.origin_session_id IS NOT NULL
       AND NEW.origin_agent_id IS DISTINCT FROM (SELECT s.agent_id FROM public.chat_sessions s WHERE s.id = NEW.origin_session_id) THEN
        RAISE EXCEPTION '%: origin session % is not agent %''s', TG_TABLE_NAME, NEW.origin_session_id, NEW.origin_agent_id USING ERRCODE = 'check_violation';
    END IF;
    -- No session (none recorded, or deleted: its FK sets it NULL): no moment.
    IF NEW.origin_session_id IS NULL THEN
        NEW.origin_message_id := NULL;
    END IF;
    IF NEW.origin_message_id IS NOT NULL THEN
        IF NEW.origin_session_id IS DISTINCT FROM (SELECT m.session_id FROM public.chat_messages m WHERE m.id = NEW.origin_message_id) THEN
            RAISE EXCEPTION '%: origin message % is not session %''s', TG_TABLE_NAME, NEW.origin_message_id, NEW.origin_session_id USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS attention_asks_origin_is_owners ON public.attention_asks;
CREATE TRIGGER attention_asks_origin_is_owners
    BEFORE INSERT OR UPDATE OF owner_id, origin_agent_id, origin_session_id, origin_message_id
    ON public.attention_asks FOR EACH ROW EXECUTE FUNCTION public.officer_origin_is_owners();

DROP TRIGGER IF EXISTS plugin_reply_drafts_origin_is_owners ON public.plugin_reply_drafts;
CREATE TRIGGER plugin_reply_drafts_origin_is_owners
    BEFORE INSERT OR UPDATE OF owner_id, origin_agent_id, origin_session_id, origin_message_id
    ON public.plugin_reply_drafts FOR EACH ROW EXECUTE FUNCTION public.officer_origin_is_owners();
