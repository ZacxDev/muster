-- muster 0186 — A HEADS-UP ROUTE OF A MESSAGE THAT CAN BE ANSWERED
-- (internal/plugins/routes.go Route.HeadsUp; route_to_session's heads_up):
-- the chief hands a REPLYABLE plugin message to the session already on what
-- it is about as a one-way heads-up, because the message also asks the owner
-- to decide something. The session gets the context; its answer comes back to
-- the chief only, never as a reply to the sender, so the owner's decision is
-- not answered for them and the sender is not answered twice.
-- docs/plugins.md#a-heads-up-for-a-message-whose-answer-is-yours.
--
--   session_routes.heads_up   true on such a route. It is ONE-WAY too
--       (one_way, 0087): every path that sends a one-way route's answer to the
--       chief and refuses it a reply draft applies unchanged. heads_up only
--       says the message itself CAN be answered — by the owner, through the
--       chief — so the paste says "A HEADS-UP, NOT A REQUEST TO REPLY" where a
--       no_reply message's says "NO REPLY POSSIBLE", and a hand-back hands back
--       a message the chief can still draft a reply to. Two rules read it too:
--       a later chief route of a message that had a heads-up is a heads-up
--       again (plugins.PGStore.CreateRouteFrom), and a RELAYED heads-up does
--       not count as the chief having acted on the message (retry.go
--       liveRoute), so the "not acted on" ask still comes.
--
-- 🔴 A HEADS-UP IS ONE-WAY (session_routes_heads_up_check): never a two-way
-- route whose answer would become a reply draft. Added NOT VALID, then
-- VALIDATEd, so the check's scan holds only a SHARE UPDATE EXCLUSIVE lock;
-- every existing row has heads_up false, so it passes.
--
-- NOT NULL DEFAULT false: no rewrite, and every existing route reads as not a
-- heads-up, which every one of them is. Must apply after 0185 (the contiguity
-- test refuses a gap). Not re-applied by db.RLSOn (it changes no policy, grant
-- or default); the column inherits the table's row-level security. Idempotent.
-- 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. An older image does not read heads_up (its column lists are
-- named, never `*`): it reads such a route as an ordinary one-way route, so
-- its answer still goes to the chief only and no draft is made. What it gets
-- wrong: the paste says "NO REPLY POSSIBLE", a hand-back offers settling
-- instead of a reply draft, a later route of the message may be two-way, and
-- a relayed heads-up counts as acted on (no "not acted on" ask). To remove it:
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_heads_up_check;
--   ALTER TABLE session_routes DROP COLUMN IF EXISTS heads_up;
--   DELETE FROM schema_migrations WHERE version = 186;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS heads_up BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_heads_up_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_heads_up_check
    CHECK (NOT heads_up OR one_way) NOT VALID;
ALTER TABLE public.session_routes VALIDATE CONSTRAINT session_routes_heads_up_check;
