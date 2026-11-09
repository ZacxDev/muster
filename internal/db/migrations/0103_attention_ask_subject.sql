-- muster 0103 — A CHIEF'S QUESTION REMEMBERS THE SESSION IT WAS ASKED ABOUT,
-- AND A WITHDRAWN QUESTION SAYS WHY (docs/design/chief-in-agent-view.md,
-- "Round 4: four defects seen live"; internal/api chief_proposal.go).
--
-- The owner asks their chief, from a session's view, to continue or end that
-- session. The chief needs a fact first (which repository?) and asks them —
-- ask_owner's card or attention_ask. The owner answers by typing a new line on
-- the view instead of on the card, and the chief files the proposal. The
-- question was answered, but nothing linked it to the proposal, so it stayed
-- open until it expired. Now:
--
--   about_key       the session key (chat_messages.about, 0098) of the owner's
--                   view line the chief was answering when it raised the ask,
--                   NULL for every other ask. Set ONLY by muster (internal/api
--                   chiefAskSubject), never from the asker's arguments, and
--                   only when that turn's own input IS the owner's view line
--                   (no plugin message, relayed answer or note after it).
--   settled_reason  why muster withdrew the ask (status 'cancelled'), NULL for
--                   an ask cancelled any other way and for every other status.
--                   Written ONLY by attention.WithdrawAbout: when a proposal of
--                   the chief's about session K is filed, its still-open asks
--                   whose about_key is K are withdrawn, with this reason.
--
-- 🔴 THE LINK IS STRUCTURAL, NEVER TEXT: no question is matched by what it
-- says. An ask with no about_key is never withdrawn this way.
--
-- New columns on an owned table: the table's row-level security covers them;
-- not re-applied by db.RLSOn. Idempotent.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). The
-- number was the next free one on main when written; it may need renumbering
-- at merge.
--
-- ROLLBACK (after rolling the image back; an older image never reads them):
--   ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS about_key, DROP COLUMN IF EXISTS settled_reason;
--   DELETE FROM schema_migrations WHERE version = 103;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks
    ADD COLUMN IF NOT EXISTS about_key text CHECK (about_key IS NULL OR char_length(about_key) BETWEEN 1 AND 200),
    ADD COLUMN IF NOT EXISTS settled_reason text CHECK (settled_reason IS NULL OR
        (status = 'cancelled' AND char_length(settled_reason) BETWEEN 1 AND 300));
