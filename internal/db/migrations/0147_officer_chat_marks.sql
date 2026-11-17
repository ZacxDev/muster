-- muster 0147 — AN OFFICER CHAT LANE'S MARKS (internal/officers chat.go,
-- internal/agents officer_chat.go; docs/officers.md "Chat sessions").
--
--   chat_sessions.officer_chat_seen       the highest chat seq the session's
--       lane has been given: the last member turn muster sent it, or its own
--       reply once the HQ server took it. 0 on every other session.
--   chat_sessions.officer_chat_answered   the seq of the lane's last reply
--       the HQ server took. seen > answered is a turn whose reply never
--       reached it (muster restarted mid-turn, or the post was refused): the
--       next turn asks the lane to answer again rather than re-sending the
--       member's message. 0 on every other session.
--
-- 🔴 WRITTEN BEFORE THE SEND, SO A RESTART NEVER RE-SENDS AND NEVER SKIPS. The
-- runner marks a message seen before it types it into the lane (and puts the
-- mark back when ccd refuses the turn before anything was typed); without the
-- mark a restarted muster could only re-send everything after the last reply
-- or skip it.
--
-- New columns inherit chat_sessions' policies, so nothing here touches a
-- policy, grant or default and db.RLSOn re-applies nothing. Idempotent: every
-- statement can run twice.
--
-- Must apply after its predecessor, 0146_chief_usage_nudge_check (the
-- contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads them):
--   ALTER TABLE public.chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_officer_chat_marks;
--   ALTER TABLE public.chat_sessions DROP COLUMN IF EXISTS officer_chat_seen, DROP COLUMN IF EXISTS officer_chat_answered;
--   DELETE FROM schema_migrations WHERE version = 147;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_sessions ADD COLUMN IF NOT EXISTS officer_chat_seen     BIGINT NOT NULL DEFAULT 0;
ALTER TABLE public.chat_sessions ADD COLUMN IF NOT EXISTS officer_chat_answered BIGINT NOT NULL DEFAULT 0;

ALTER TABLE public.chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_officer_chat_marks;
ALTER TABLE public.chat_sessions ADD CONSTRAINT chat_sessions_officer_chat_marks
    CHECK (officer_chat_answered >= 0 AND officer_chat_seen >= officer_chat_answered);
