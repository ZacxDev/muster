-- muster 0191 — FEEDBACK ON A PAGE GOES TO THE SESSION THAT MADE IT
-- (internal/api page_feedback.go, session_wake.go; docs/pages.md "Feedback").
--
-- The page viewer's shell draws a Feedback pill: the page's owner (from their
-- own view, signed in) and a share link's viewer (through the link) type a
-- message about the page and Send it. Operator decision (2026-11-25): a share
-- link's viewer may send feedback, and it reaches the session fenced as
-- untrusted. The rest is this change's own design, not the operator's: each
-- message is kept as one of the page's responses (so page_responses reads it
-- even when no line reaches the session), and each wakes the session that
-- made the page with the message itself — the owner's words as theirs, a link
-- viewer's fenced as untrusted — one line per message, never a count.
--
--   page_responses.kind 'feedback'   a feedback message: component is
--       '__feedback' (no page's component can be: they start with a letter,
--       pages.ValidComponent), data is {"text": "<the message>"}.
--   session_wakes.kind 'page_feedback'   one per feedback response
--       (0177's session_wakes_one_per_response): its page and response, the
--       message in operator_text (never empty), the version it was sent from
--       in response_count (>= 1), no decision, component or route.
--   session_wakes.link_id   on a page_feedback wake only: the share link the
--       message came through. NULL is the OWNER's own message (their words,
--       queued only while the sign-in that sent it is fresh, as a
--       page_own_response's); set, it is a LINK VIEWER's — a stranger's
--       words, fenced as untrusted on the line, asking no sign-in.
--
-- Widens 0116's page_responses kind CHECK and 0183's session_wakes kind and
-- subject CHECKs in place (same names). Every statement is idempotent.
-- --rls-on re-applies 0116's and 0177's bodies (db.RLSOn), whose CHECKs are
-- inline in CREATE TABLE IF NOT EXISTS, so it never narrows these again.
--
-- Must apply after its predecessor, 0190 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. 🔴 SETTLE THE NEW WAKES FIRST: an older image's renderWake has no
-- case for page_feedback and would paste an empty line. The messages
-- themselves are page responses and are NOT lost by deleting their wakes: a
-- session still reads them with page_responses. But an older image's
-- page_responses keeps them only while the kind CHECK allows them, so the
-- rows go too, below — export them first if they matter:
--   SELECT id, page_id, link_id, by_owner, data->>'text' FROM public.page_responses WHERE kind = 'feedback';
-- Then, before the older image starts:
--   UPDATE public.send_queue SET state = 'cancelled', finished_at = now(),
--       detail = 'cancelled: session wakes rolled back', held_until = NULL, hold_token = NULL
--     WHERE state = 'queued' AND session_wake_id IN (SELECT id FROM public.session_wakes WHERE kind = 'page_feedback');
--   DELETE FROM public.session_wakes WHERE kind = 'page_feedback';
--   DELETE FROM public.page_responses WHERE kind = 'feedback';
-- Then restore 0183's subject CHECK (re-run the session_wakes_subject
-- statements of 0183_session_wakes_plugin_refused.sql) and 0178's kind CHECK
-- (its session_wakes_kind_check statements), and
--   ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_link_feedback;
--   ALTER TABLE public.session_wakes DROP COLUMN IF EXISTS link_id;
--   ALTER TABLE public.page_responses DROP CONSTRAINT IF EXISTS page_responses_feedback_component;
--   ALTER TABLE public.page_responses DROP CONSTRAINT IF EXISTS page_responses_kind_check;
--   ALTER TABLE public.page_responses ADD CONSTRAINT page_responses_kind_check CHECK (kind IN ('submit', 'upload'));
--   DELETE FROM schema_migrations WHERE version = 191;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.page_responses DROP CONSTRAINT IF EXISTS page_responses_kind_check;
ALTER TABLE public.page_responses ADD CONSTRAINT page_responses_kind_check CHECK (kind IN ('submit', 'upload', 'feedback'));
ALTER TABLE public.page_responses DROP CONSTRAINT IF EXISTS page_responses_feedback_component;
ALTER TABLE public.page_responses ADD CONSTRAINT page_responses_feedback_component
    CHECK (kind <> 'feedback' OR component = '__feedback');

ALTER TABLE public.session_wakes ADD COLUMN IF NOT EXISTS link_id BIGINT;
ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_link_feedback;
ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_link_feedback
    CHECK (link_id IS NULL OR (kind = 'page_feedback' AND link_id > 0));

ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_kind_check;
ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_kind_check CHECK (kind IN ('ask_answered', 'ask_dismissed',
    'draft_decided', 'draft_outcome', 'page_own_response', 'page_responses',
    'officer_draft_decided', 'officer_draft_outcome', 'stepup_decided', 'page_feedback'));

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
    OR (kind = 'officer_draft_decided' AND draft_id IS NOT NULL AND officer_item_id IS NOT NULL AND ask_id IS NULL
        AND page_id IS NULL AND response_id IS NULL AND decision IN ('approved', 'rejected', 'plugin_refused') AND route_id = ''
        AND component = '')
    OR (kind = 'officer_draft_outcome' AND draft_id IS NOT NULL AND officer_item_id IS NOT NULL AND ask_id IS NULL
        AND page_id IS NULL AND response_id IS NULL AND decision IN ('sent', 'failed', 'unconfirmed') AND route_id = ''
        AND component = '' AND operator_text = '' AND response_count >= 1)
    OR (kind = 'stepup_decided' AND ask_id IS NOT NULL AND draft_id IS NULL AND page_id IS NULL AND response_id IS NULL
        AND officer_item_id IS NULL AND decision IN ('confirmed', 'confirmed_failed', 'refused', 'dismissed', 'settled_approved', 'settled_rejected')
        AND component <> '' AND route_id = '' AND operator_text = '')
    -- A feedback message (0191): its page, its response, the version it was
    -- sent from (response_count) and its words; no decision, component or
    -- route.
    OR (kind = 'page_feedback' AND page_id IS NOT NULL AND response_id IS NOT NULL AND ask_id IS NULL AND draft_id IS NULL
        AND officer_item_id IS NULL AND decision = '' AND component = '' AND route_id = '' AND response_count >= 1
        AND operator_text <> ''));
