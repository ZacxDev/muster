-- muster 0092 — AN OWNER'S ANSWER RECORDS ITS DOOR: BROWSER OR PHONE
-- (internal/attention answerVia; docs/attention.md, "Where an answer came from";
-- docs/design/chief-dispatch.md 2.8, property 1).
--
-- 0034 recorded one door, 'mcp' (a mirrored session's attention_answer), and
-- left every other answer NULL: "the /attention view". That was one fact while
-- the view was the only other door. The owner's paired phone is a second one
-- (device_actions.go), and a mirrored dispatch's held card binds a start to an
-- answer ONLY when it came through one of the owner's own doors — a signed-in
-- browser session or their phone — the rule #485's ownerTapped states for a
-- release. An answer read back from the row can only be held to that rule if
-- the row says which door it came through, so it now does:
--
--   answered_via  NULL      an answer recorded before this migration (no
--                           backfill), or one settled in session (the owner's
--                           message to the asker, answered_in_session)
--                 'browser' the Attention view, from a signed-in browser
--                           session (authz.ViaSession)
--                 'device'  the owner's paired phone (authz.ViaDevice)
--                 'mcp'     attention_answer, from a mirrored session (0034,
--                           unchanged)
--
-- The store stamps 'browser' or 'device' from the answering principal itself
-- (attention.Answer); any other principal leaves it NULL, which no binding
-- accepts as an owner's door.
--
-- 🔴 THE CHECK STAYS ALL-OR-NOTHING FOR 'mcp' AND ADMITS THE TWO NEW DOORS ONLY
-- ON AN 'answered' ROW WITH NO HOST OR SESSION, so a bug cannot write a browser
-- answer that names a mirror session, nor a door on an unanswered row.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default) and
-- idempotent: DROP/ADD CONSTRAINT.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). The
-- number was the next free one on main when written; it may need renumbering
-- at merge.
--
-- AN OWNER CARD IS MARKED, AND REMEMBERS THE CHAT SESSION IT WAS ASKED IN.
-- ask_owner's stored card is an attention ask raised by the chief (internal/api
-- raiseOwnerCard), and three rules hold for it that hold for no other ask:
--
--   owner_card     true only for that card. answerSQL moves it only through the
--                  owner's own doors ('browser', 'device'), never attention_answer
--                  ('mcp'); and a message the owner types to the chief settles
--                  at most ONE open card (the oldest), never every one, as #496's
--                  chat drew one card per answer (AnswerInSession).
--   asked_session  the chat session key the chief asked in, so an answer given
--                  on the Attention view or the phone is delivered there, not to
--                  the chief's default session (NULL: unknown, the default).
--
-- ROLLBACK. An older image never writes 'browser', 'device', owner_card or
-- asked_session, so it runs over this schema unchanged. To restore 0034's
-- CHECK and drop the two columns, after rolling the image back (rows with the
-- new doors are cleared first, or the ADD fails):
--   UPDATE attention_asks SET answered_via = NULL WHERE answered_via IN ('browser', 'device');
--   ALTER TABLE attention_asks DROP CONSTRAINT IF EXISTS attention_asks_answered_via_check;
--   ALTER TABLE attention_asks ADD CONSTRAINT attention_asks_answered_via_check CHECK (
--     (answered_via IS NULL AND answered_via_host IS NULL AND answered_via_session IS NULL)
--     OR (answered_via IS NOT NULL AND answered_via = 'mcp' AND status = 'answered'
--       AND answered_via_host IS NOT NULL AND answered_via_session IS NOT NULL
--       AND answered_via_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
--       AND answered_via_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'
--       AND mirror_host IS DISTINCT FROM answered_via_host));
--   ALTER TABLE attention_asks DROP COLUMN IF EXISTS owner_card, DROP COLUMN IF EXISTS asked_session;
--   DELETE FROM schema_migrations WHERE version = 92;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_answered_via_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_answered_via_check CHECK (
        (answered_via IS NULL AND answered_via_host IS NULL AND answered_via_session IS NULL)
        -- 🔴 EVERY TERM IS NULL-SAFE (0034's note): the IS NOT NULLs make each
        -- term TRUE or FALSE, never NULL.
        OR (answered_via IS NOT NULL AND answered_via IN ('browser', 'device')
            AND status = 'answered'
            AND answered_via_host IS NULL AND answered_via_session IS NULL)
        OR (answered_via IS NOT NULL AND answered_via = 'mcp'
            AND status = 'answered'
            AND answered_via_host IS NOT NULL AND answered_via_session IS NOT NULL
            AND answered_via_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
            AND answered_via_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'
            -- Never an ask raised on the answering host (any of its sessions).
            AND mirror_host IS DISTINCT FROM answered_via_host));

ALTER TABLE public.attention_asks
    ADD COLUMN IF NOT EXISTS owner_card boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS asked_session text CHECK (asked_session IS NULL OR length(asked_session) BETWEEN 1 AND 200);
