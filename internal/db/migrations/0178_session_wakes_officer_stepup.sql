-- muster 0178 — A DECISION ON WORK A MIRRORED SESSION ASKED FOR WAKES THAT
-- SESSION, TOO (internal/sessionwake; internal/api session_wake.go,
-- session_wake_requests.go; docs/attention.md "Delivery").
--
-- 0177's wakes cover what a session RAISED or WROTE itself (an ask, a reply
-- draft, a page). This adds two kinds of work a session ASKED FOR:
--
--   officer_items.requested_host / requested_session   the mirrored Claude
--       Code session that enqueued the item (officer_item_enqueue from its own
--       MCP door: the host's mirror token plus the window the host lists
--       running that session NOW, internal/api mirrorSessionOf). '' for every
--       other item: an owner's browser, the admin API (a personal or hook
--       token names no session), a plugin, a schedule. 🔴 NEVER FROM A
--       PAYLOAD: no request field or header writes these columns; only the
--       bound caller does.
--   session_wakes kinds
--       officer_draft_decided   the owner approved or rejected an outbound
--           draft an officer filed for such an item (with a rejection, the
--           owner's own reason, fenced).
--       officer_draft_outcome   that approved draft (or the retry successor
--           its chain ended on, 0175) was sent, failed, or may have been
--           sent. response_count carries the attempt number.
--       stepup_decided   the owner confirmed or refused, on their paired
--           phone, a step-up card the session's MCP call raised (or decided
--           the card's draft on another door, or dismissed the card).
--           component carries the tool's name (muster's own ledger,
--           hostStepUpTools), never its arguments or its output.
--   session_wakes.officer_item_id   an officer wake's item (the line names it).
--
-- 🔴 THE LINE IS STILL MUSTER'S WORDS AND THE OWNER'S OWN WORDS: the new kinds
-- carry ids, a decision word, an attempt count, a tool name and — only a
-- rejection — the reason the OWNER gave. A draft's body, a plugin's detail, an
-- officer's words and a tool's output are never on the line (renderWake).
--
-- Widens 0177's CHECKs in place (kind, decision, the subject rule); the table
-- keeps its row-level security, untouched here. Every statement is idempotent.
-- 🔴 --rls-on re-applies 0177's body (db.RLSOn), whose CHECKs are inline in its
-- CREATE TABLE IF NOT EXISTS, so it never narrows these again.
--
-- Must apply after its predecessor, 0177_session_wakes (the contiguity test
-- refuses a gap). 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. 🔴 SETTLE THE NEW KINDS FIRST: an older image's renderWake knows
-- none of them and would queue an EMPTY line for each. Before it starts:
--   UPDATE public.send_queue SET state = 'cancelled', finished_at = now(),
--       detail = 'cancelled: session wakes rolled back', held_until = NULL, hold_token = NULL
--     WHERE state = 'queued' AND session_wake_id IN (SELECT id FROM public.session_wakes
--       WHERE kind IN ('officer_draft_decided', 'officer_draft_outcome', 'stepup_decided'));
--   DELETE FROM public.session_wakes WHERE kind IN ('officer_draft_decided', 'officer_draft_outcome', 'stepup_decided');
-- and keep ONE decision wake per reply draft (0177's index allows no more):
-- the later, which is the draft's real state (a withdrawn approval's
-- rejection), the earlier one's queued row cancelled first:
--   UPDATE public.send_queue SET state = 'cancelled', finished_at = now(),
--       detail = 'cancelled: session wakes rolled back', held_until = NULL, hold_token = NULL
--     WHERE state = 'queued' AND session_wake_id IN (SELECT o.id FROM public.session_wakes o JOIN public.session_wakes n
--       ON n.kind = 'draft_decided' AND n.draft_id = o.draft_id AND n.id > o.id WHERE o.kind = 'draft_decided');
--   DELETE FROM public.session_wakes o USING public.session_wakes n
--     WHERE o.kind = 'draft_decided' AND n.kind = 'draft_decided' AND n.draft_id = o.draft_id AND n.id > o.id;
--   DROP INDEX IF EXISTS public.session_wakes_one_per_draft;
--   CREATE UNIQUE INDEX session_wakes_one_per_draft ON public.session_wakes (draft_id, kind) WHERE draft_id IS NOT NULL;
-- Then (an older image reads none of the rest):
--   ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_subject;
--   ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_subject CHECK (<0177's>);
--   ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_kind_check;
--   ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_kind_check CHECK (kind IN (<0177's six>));
--   ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_decision_check;
--   ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_decision_check
--       CHECK (decision IN ('', 'approved', 'rejected', 'sent', 'failed', 'unconfirmed'));
--   ALTER TABLE public.session_wakes DROP COLUMN IF EXISTS officer_item_id;
--   ALTER TABLE public.officer_items DROP CONSTRAINT IF EXISTS officer_items_requested_session_shape;
--   ALTER TABLE public.officer_items DROP COLUMN IF EXISTS requested_host;
--   ALTER TABLE public.officer_items DROP COLUMN IF EXISTS requested_session;
--   DELETE FROM schema_migrations WHERE version = 178;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.officer_items ADD COLUMN IF NOT EXISTS requested_host TEXT NOT NULL DEFAULT '';
ALTER TABLE public.officer_items ADD COLUMN IF NOT EXISTS requested_session TEXT NOT NULL DEFAULT '';
ALTER TABLE public.officer_items DROP CONSTRAINT IF EXISTS officer_items_requested_session_shape;
ALTER TABLE public.officer_items ADD CONSTRAINT officer_items_requested_session_shape CHECK (
    (requested_host = '' AND requested_session = '')
    OR (requested_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' AND requested_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'));

ALTER TABLE public.session_wakes ADD COLUMN IF NOT EXISTS officer_item_id BIGINT;

ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_kind_check;
ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_kind_check CHECK (kind IN ('ask_answered', 'ask_dismissed',
    'draft_decided', 'draft_outcome', 'page_own_response', 'page_responses',
    'officer_draft_decided', 'officer_draft_outcome', 'stepup_decided'));

ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_decision_check;
ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_decision_check CHECK (decision IN ('', 'approved', 'rejected',
    'sent', 'failed', 'unconfirmed', 'confirmed', 'confirmed_failed', 'refused', 'dismissed', 'settled_approved', 'settled_rejected'));

ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_subject;
ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_subject CHECK (
    (kind IN ('ask_answered', 'ask_dismissed') AND ask_id IS NOT NULL AND draft_id IS NULL AND page_id IS NULL AND response_id IS NULL
        AND officer_item_id IS NULL)
    OR (kind IN ('draft_decided', 'draft_outcome') AND draft_id IS NOT NULL AND ask_id IS NULL AND page_id IS NULL AND response_id IS NULL
        AND decision <> '' AND officer_item_id IS NULL)
    OR (kind = 'page_own_response' AND page_id IS NOT NULL AND response_id IS NOT NULL AND ask_id IS NULL AND draft_id IS NULL
        AND officer_item_id IS NULL)
    OR (kind = 'page_responses' AND page_id IS NOT NULL AND response_id IS NULL AND ask_id IS NULL AND draft_id IS NULL
        AND response_count >= 1 AND officer_item_id IS NULL)
    -- An officer's draft: the draft and the item the session enqueued.
    OR (kind = 'officer_draft_decided' AND draft_id IS NOT NULL AND officer_item_id IS NOT NULL AND ask_id IS NULL
        AND page_id IS NULL AND response_id IS NULL AND decision IN ('approved', 'rejected') AND route_id = '' AND component = '')
    OR (kind = 'officer_draft_outcome' AND draft_id IS NOT NULL AND officer_item_id IS NOT NULL AND ask_id IS NULL
        AND page_id IS NULL AND response_id IS NULL AND decision IN ('sent', 'failed', 'unconfirmed') AND route_id = ''
        AND component = '' AND operator_text = '' AND response_count >= 1)
    -- A step-up card (an attention ask muster filed for the session's call):
    -- the card, the tool's name, and no text of anyone's.
    OR (kind = 'stepup_decided' AND ask_id IS NOT NULL AND draft_id IS NULL AND page_id IS NULL AND response_id IS NULL
        AND officer_item_id IS NULL AND decision IN ('confirmed', 'confirmed_failed', 'refused', 'dismissed', 'settled_approved', 'settled_rejected')
        AND component <> '' AND route_id = '' AND operator_text = ''));

-- 🔴 A DRAFT'S DECISION WAKE IS ONE PER DECISION, NOT ONE PER DRAFT. The owner
-- can withdraw an approval its plugin has not claimed yet by rejecting the
-- draft (plugins.PGStore.DecideDraftFrom): that rejection is a second decision,
-- and 0177's (draft_id, kind) made its wake a duplicate of the approval's, so
-- the session was never told. A decided kind is now keyed on its decision too
-- (a draft is approved at most once and rejected at most once, so at most two);
-- an outcome kind stays one per draft, as before.
-- 🔴 SAME NAME, REDEFINED IN PLACE: --rls-on re-applies 0177's body, whose
-- CREATE UNIQUE INDEX IF NOT EXISTS session_wakes_one_per_draft then finds this
-- one and leaves it alone. Under a new name it would rebuild 0177's narrower
-- index beside this one, and fail on the first draft with two decisions.
-- (internal/sessionwake Create's conflict read uses the same key.)
DROP INDEX IF EXISTS public.session_wakes_one_per_draft;
CREATE UNIQUE INDEX session_wakes_one_per_draft ON public.session_wakes
    (draft_id, kind, (CASE WHEN kind IN ('draft_decided', 'officer_draft_decided') THEN decision ELSE '' END))
    WHERE draft_id IS NOT NULL;
