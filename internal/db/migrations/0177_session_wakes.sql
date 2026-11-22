-- muster 0177 — A DECISION WAKES THE MIRRORED SESSION IT IS FOR
-- (internal/sessionwake; internal/api session_wake.go; docs/attention.md
-- "Delivery", docs/schema.md).
--
--   session_wakes   one row per line muster owes a MIRRORED Claude Code
--       session because its owner decided something it was waiting on: an
--       ask it raised was answered or dismissed, a reply draft it wrote (a
--       route's answer) was approved or rejected and later sent or not, or a
--       page it made got a response. The row is delivered through the
--       window's send queue (send_queue.session_wake_id, below), never pasted
--       mid-turn, exactly once.
--   send_queue.session_wake_id   a queued message a wake became: its origin,
--       gated at the paste on the wake (one origin per row).
--
-- 🔴 THE TEXT IS MUSTER'S OWN LINE AND THE OWNER'S OWN WORDS, NEVER A THIRD
-- PARTY'S. The row stores the parts of the line, never the line: kind, ids,
-- a decision word, a count, a component name (its own CHECKed shape) and
-- operator_text — the owner's answer, the reason they gave, or a response
-- they sent from their own view. internal/api renderWake is the one function
-- that turns a row into the pasted text; a response another viewer sent can
-- only become a count (kind page_responses carries no operator_text: the
-- CHECK below), so a stranger's words are never typed into a window.
--
-- 🔴 THE STATES ARE THE ANSWER DELIVERY'S (0031), each move one conditional
-- UPDATE on the state it leaves:
--   queued       owed; not in the send queue yet (no window holds the
--                session now, or the sweep has not reached it). detail says
--                why it waits.
--   delivering   handed to send_queue row send_queue_id, which waits for the
--                window to be idle and pastes it (the queue's own exactly-once
--                claim and lease).
--   delivered    the paste was confirmed.
--   unconfirmed  it may or may not have arrived (the host did not confirm
--                the paste, the text sits unsent in Claude's input box), or it
--                was not delivered within a day (detail "Not delivered: …").
--                NEVER resent on its own: the owner's Retry queues it again.
--   waited       the session read it itself (attention_wait returned the
--                answer or the dismissal), so it is never pasted.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0158's shape: one owner's
-- wakes are invisible to every other principal. PERSONAL share class (no team
-- access): a team's sessions get no wakes in this phase. rollback/rls_off.sql
-- turns it off with the rest, and --rls-on re-applies this file (db.RLSOn), so
-- every statement is idempotent.
--
-- Must apply after its predecessor, 0176_attention_asks_multi (the contiguity
-- test refuses a gap). 🔴 RENUMBER IT (and db.sessionWakesMigrationVersion) at
-- merge if main moved.
--
-- ROLLBACK. 🔴 DRAIN THE WAKES' QUEUED ROWS FIRST: an older image's queue
-- worker does not know send_queue.session_wake_id, so it would paste a queued
-- wake (muster's line and the owner's answer) as if a person had queued it,
-- with no gate on the wake. Before the older image starts, cancel them (a row
-- already 'sending' is past cancelling; let it finish):
--   UPDATE public.send_queue SET state = 'cancelled', finished_at = now(),
--       detail = 'cancelled: session wakes rolled back', held_until = NULL, hold_token = NULL
--     WHERE session_wake_id IS NOT NULL AND state = 'queued';
-- Then, after rolling the image back (an older image reads none of the rest):
--   ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_one_origin;
--   ALTER TABLE public.send_queue ADD CONSTRAINT send_queue_one_origin
--       CHECK (num_nonnulls(route_id, chief_nudge_id, chief_forward_dispatch, fleet_action_id) <= 1);
--   DROP INDEX IF EXISTS public.send_queue_one_live_per_wake;
--   ALTER TABLE public.send_queue DROP COLUMN IF EXISTS session_wake_id;
--   DROP TABLE public.session_wakes;
--   DELETE FROM schema_migrations WHERE version = 177;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.session_wakes (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind            TEXT        NOT NULL CHECK (kind IN ('ask_answered', 'ask_dismissed', 'draft_decided',
                                                         'draft_outcome', 'page_own_response', 'page_responses')),
    -- What it is about: one subject per kind (session_wakes_subject below).
    -- No foreign keys: a wake is a record of a line owed, and must not stop an
    -- ask, a draft or a page from being deleted.
    ask_id          BIGINT,
    draft_id        BIGINT,
    page_id         BIGINT,
    response_id     BIGINT,
    -- The parts of the line (renderWake): muster's own words or the owner's.
    route_id        TEXT        NOT NULL DEFAULT '' CHECK (route_id = '' OR route_id ~ '^rt_[0-9a-f]{24}$'),
    decision        TEXT        NOT NULL DEFAULT '' CHECK (decision IN ('', 'approved', 'rejected', 'sent', 'failed', 'unconfirmed')),
    response_count  INTEGER     NOT NULL DEFAULT 0 CHECK (response_count BETWEEN 0 AND 100000),
    component       TEXT        NOT NULL DEFAULT '' CHECK (component = '' OR component ~ '^[A-Za-z0-9_-]{1,64}$'),
    operator_text   TEXT        NOT NULL DEFAULT '' CHECK (octet_length(operator_text) <= 16384),
    -- The session it goes to: a mirrored Claude Code session on one host.
    mirror_host     TEXT        NOT NULL CHECK (mirror_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    mirror_session  TEXT        NOT NULL CHECK (mirror_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'),
    state           TEXT        NOT NULL DEFAULT 'queued'
                    CHECK (state IN ('queued', 'delivering', 'delivered', 'unconfirmed', 'waited')),
    detail          TEXT        NOT NULL DEFAULT '' CHECK (char_length(detail) <= 300),
    attempts        INTEGER     NOT NULL DEFAULT 0,
    -- When it was last queued (created, or the owner's Retry): a day after
    -- it, a wake still not handed to the queue gives up (unconfirmed).
    queued_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    send_queue_id   BIGINT,
    delivered_at    TIMESTAMPTZ,
    CONSTRAINT session_wakes_subject CHECK (
        (kind IN ('ask_answered', 'ask_dismissed') AND ask_id IS NOT NULL AND draft_id IS NULL AND page_id IS NULL AND response_id IS NULL)
        OR (kind IN ('draft_decided', 'draft_outcome') AND draft_id IS NOT NULL AND ask_id IS NULL AND page_id IS NULL AND response_id IS NULL
            AND decision <> '')
        OR (kind = 'page_own_response' AND page_id IS NOT NULL AND response_id IS NOT NULL AND ask_id IS NULL AND draft_id IS NULL)
        OR (kind = 'page_responses' AND page_id IS NOT NULL AND response_id IS NULL AND ask_id IS NULL AND draft_id IS NULL
            AND response_count >= 1)),
    -- 🔴 Another viewer's response is a COUNT, never words: the one kind that
    -- carries other people's responses holds no text at all.
    CONSTRAINT session_wakes_count_has_no_text CHECK (kind <> 'page_responses' OR (operator_text = '' AND component = '')),
    -- A wake in the queue names its row.
    CONSTRAINT session_wakes_delivering_has_row CHECK (state <> 'delivering' OR send_queue_id IS NOT NULL),
    CONSTRAINT session_wakes_delivered_has_time CHECK ((state = 'delivered') = (delivered_at IS NOT NULL))
);

ALTER TABLE public.session_wakes ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

-- One wake per decision: an ask's answer (or dismissal) once, a draft's
-- decision once and its outcome once, an owner's response once.
CREATE UNIQUE INDEX IF NOT EXISTS session_wakes_one_per_ask
    ON public.session_wakes (ask_id, kind) WHERE ask_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS session_wakes_one_per_draft
    ON public.session_wakes (draft_id, kind) WHERE draft_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS session_wakes_one_per_response
    ON public.session_wakes (response_id) WHERE response_id IS NOT NULL;
-- Other viewers' responses to one page fold into ONE queued count line.
CREATE UNIQUE INDEX IF NOT EXISTS session_wakes_one_queued_count
    ON public.session_wakes (page_id) WHERE kind = 'page_responses' AND state = 'queued';
-- The sweep's read: every owed or in-flight wake.
CREATE INDEX IF NOT EXISTS session_wakes_open ON public.session_wakes (queued_at)
    WHERE state IN ('queued', 'delivering');

ALTER TABLE public.session_wakes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.session_wakes FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.session_wakes;
CREATE POLICY owner_isolation ON public.session_wakes
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.session_wakes TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.session_wakes_id_seq TO muster_app;

-- The send queue's origin column, and 0158's one-origin rule widened to it.
ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS session_wake_id BIGINT;
ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_one_origin;
ALTER TABLE public.send_queue ADD CONSTRAINT send_queue_one_origin
    CHECK (num_nonnulls(route_id, chief_nudge_id, chief_forward_dispatch, fleet_action_id, session_wake_id) <= 1);
-- 🔴 AT MOST ONE LIVE QUEUE ROW PER WAKE (queued, being sent, or sent): two
-- sweeps handing one wake to the queue at once cannot both insert. A row that
-- failed or was cancelled frees it for the wake's next attempt.
CREATE UNIQUE INDEX IF NOT EXISTS send_queue_one_live_per_wake
    ON public.send_queue (session_wake_id) WHERE session_wake_id IS NOT NULL AND state IN ('queued', 'sending', 'sent');
