-- muster 0183 — A PLUGIN'S REFUSAL OF WORK A MIRRORED SESSION ASKED FOR GOES
-- TO THAT SESSION (internal/api session_wake_requests.go, plugin_outbound.go;
-- docs/attention.md "Delivery").
--
-- 0178 woke the session that enqueued an officer item with the OWNER's
-- decision on the officer's draft for it. A draft its plugin refused at its
-- pre-approval check never reached the owner, so it woke nobody, and the
-- officer redrafted it unasked. Operator decision (2026-11-23): like an owner's
-- rejection, a plugin's refusal of such an item goes only to the session that
-- asked, which is told why, and the officer files no redraft for it. If the
-- line is never delivered, the refusal goes back to the officer once.
--
--   session_wakes.decision 'plugin_refused'   on an officer_draft_decided
--       wake only: the plugin's check refused the draft. operator_text carries
--       the PLUGIN's reason (an outside party's words, untrusted, fenced on
--       the line, and never treated as the owner's: no sign-in is asked for
--       it).
--   session_wakes.retry_at   on a plugin_refused wake only: the time the
--       plugin said the draft could be tried again (its refusal's retry_at,
--       RFC 3339, cut to the second), NULL when it named none. The line says
--       it, and a refusal handed back to the officer carries it into the
--       draft.refused item as a direct refusal does (the Re-file default).
--
-- Widens 0178's CHECKs in place (decision, the subject rule) for that one
-- kind; a reply draft's wakes (draft_decided, draft_outcome) may not carry the
-- new word. Every statement is idempotent. --rls-on re-applies 0177's body
-- (db.RLSOn), whose CHECKs are inline in its CREATE TABLE IF NOT EXISTS, so it
-- never narrows these again.
--
-- Must apply after its predecessor, 0182 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. 🔴 SETTLE THE NEW WAKES FIRST: an older image's renderWake reads a
-- plugin_refused wake as a rejection and would tell the session "you
-- rejected" it.
-- 🔴 AN UNDELIVERED REFUSAL IS LOST BY THE DELETE BELOW. Its item is already
-- closed (the refusal was the session's), so a plugin_refused wake still
-- queued, delivering or held reaches NOBODY once its row is gone: no session
-- line and no draft.refused item. Before rolling back, list them
--   SELECT id, draft_id, officer_item_id, state FROM public.session_wakes
--     WHERE decision = 'plugin_refused' AND state IN ('queued', 'delivering');
-- and for each, either let it settle (delivered, or "Not delivered", which
-- hands it back to the officer) or re-file the refusal for the officer by
-- hand (the officer page's draft.refused Re-file, or a new officer item
-- naming the draft). Then, before the older image starts:
--   UPDATE public.send_queue SET state = 'cancelled', finished_at = now(),
--       detail = 'cancelled: session wakes rolled back', held_until = NULL, hold_token = NULL
--     WHERE state = 'queued' AND session_wake_id IN (SELECT id FROM public.session_wakes WHERE decision = 'plugin_refused');
--   DELETE FROM public.session_wakes WHERE decision = 'plugin_refused';
-- Then restore 0178's two CHECKs: re-run the session_wakes_decision_check and
-- session_wakes_subject statements of 0178_session_wakes_officer_stepup.sql,
--   ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_retry_at_plugin_refused;
--   ALTER TABLE public.session_wakes DROP COLUMN IF EXISTS retry_at;
-- and
--   DELETE FROM schema_migrations WHERE version = 183;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_wakes ADD COLUMN IF NOT EXISTS retry_at TIMESTAMPTZ;
ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_retry_at_plugin_refused;
ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_retry_at_plugin_refused
    CHECK (retry_at IS NULL OR decision = 'plugin_refused');

ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_decision_check;
ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_decision_check CHECK (decision IN ('', 'approved', 'rejected',
    'sent', 'failed', 'unconfirmed', 'confirmed', 'confirmed_failed', 'refused', 'dismissed', 'settled_approved', 'settled_rejected',
    'plugin_refused'));

ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_subject;
ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_subject CHECK (
    (kind IN ('ask_answered', 'ask_dismissed') AND ask_id IS NOT NULL AND draft_id IS NULL AND page_id IS NULL AND response_id IS NULL
        AND officer_item_id IS NULL)
    OR (kind IN ('draft_decided', 'draft_outcome') AND draft_id IS NOT NULL AND ask_id IS NULL AND page_id IS NULL AND response_id IS NULL
        AND decision NOT IN ('', 'plugin_refused') AND officer_item_id IS NULL)
    OR (kind = 'page_own_response' AND page_id IS NOT NULL AND response_id IS NOT NULL AND ask_id IS NULL AND draft_id IS NULL
        AND officer_item_id IS NULL)
    OR (kind = 'page_responses' AND page_id IS NOT NULL AND response_id IS NULL AND ask_id IS NULL AND draft_id IS NULL
        AND response_count >= 1 AND officer_item_id IS NULL)
    -- An officer's draft: the draft and the item the session enqueued. A
    -- plugin's refusal is one of its decisions (0183).
    OR (kind = 'officer_draft_decided' AND draft_id IS NOT NULL AND officer_item_id IS NOT NULL AND ask_id IS NULL
        AND page_id IS NULL AND response_id IS NULL AND decision IN ('approved', 'rejected', 'plugin_refused') AND route_id = ''
        AND component = '')
    OR (kind = 'officer_draft_outcome' AND draft_id IS NOT NULL AND officer_item_id IS NOT NULL AND ask_id IS NULL
        AND page_id IS NULL AND response_id IS NULL AND decision IN ('sent', 'failed', 'unconfirmed') AND route_id = ''
        AND component = '' AND operator_text = '' AND response_count >= 1)
    OR (kind = 'stepup_decided' AND ask_id IS NOT NULL AND draft_id IS NULL AND page_id IS NULL AND response_id IS NULL
        AND officer_item_id IS NULL AND decision IN ('confirmed', 'confirmed_failed', 'refused', 'dismissed', 'settled_approved', 'settled_rejected')
        AND component <> '' AND route_id = '' AND operator_text = ''));
