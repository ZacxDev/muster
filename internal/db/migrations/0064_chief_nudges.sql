-- muster 0064 — THE CHIEF'S STALL NUDGES (internal/chiefnudge,
-- internal/api/chief_nudge.go; docs/chief-nudges.md).
--
-- chief_nudges: one row per DECISION the stall sweep took about one of an
-- owner's idle sessions — a mirrored Claude Code window (session_kind
-- 'mirror', session_key its Claude session id) or a dispatched agent
-- ('agent', session_key "<agent id>:<chat session id>", the one conversation
-- of it that stalled: agent_id and chat_session_id) — for one idle streak (streak_key). The
-- unique key (owner, kind, key, streak) is the sweep's claim: a second sweep,
-- or a restart, never decides the same streak twice.
--
--   decision      nudge | ask | leave | invalid (an answer muster could not read)
--   mode          shadow (decided, nothing sent, no ask raised) | live
--   outcome       what became of it: would_send, would_ask, left, queued,
--                 sent, asked, relay_queued, relayed, failed, ask_closed, or
--                 suppressed:<guard> (docs/chief-nudges.md lists them)
--   anchor_key    the owner's latest message the nudge cap counts against
--   quote_*       the owner message a nudge quotes: its id, its time when the
--                 source has one, and the exact quoted text
--   ask_id        the attention ask a live ask raised
--   send_queue_id the send_queue row that carries a live nudge or relay
--   relay_text    the owner's answer, verbatim, relayed into the session
--
-- send_queue.chief_nudge_id marks a queued message the CHIEF NUDGE sweep
-- queued (a nudge, or the owner's relayed answer). Like route_id it exempts the
-- row from the sign-in gate; its delivery is gated on its decision instead
-- (internal/api sendQueueNudgeGate). A row is a person's, a route's or a
-- nudge's, never two.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0060's shape: one user never
-- reads another's decisions. rollback/rls_off.sql turns it off with the rest,
-- and --rls-on re-applies this file (db.RLSOn), so every statement is
-- idempotent.
--
-- Must apply after 0063 (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads either):
--   ALTER TABLE send_queue DROP CONSTRAINT IF EXISTS send_queue_one_origin;
--   ALTER TABLE send_queue DROP COLUMN IF EXISTS chief_nudge_id;
--   DROP TABLE IF EXISTS chief_nudges;
--   DELETE FROM schema_migrations WHERE version = 64;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.chief_nudges (
    id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id       BIGINT      NOT NULL
                   DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                   REFERENCES public.users (id),
    session_kind   TEXT        NOT NULL CHECK (session_kind IN ('mirror', 'agent')),
    session_key    TEXT        NOT NULL CHECK (length(session_key) BETWEEN 1 AND 200),
    mirror_host    TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_host) <= 200),
    mirror_session TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_session) <= 200),
    mirror_window  TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_window) <= 32),
    agent_id       BIGINT,
    chat_session_id BIGINT,
    title          TEXT        NOT NULL DEFAULT '' CHECK (char_length(title) <= 300),
    streak_key     TEXT        NOT NULL CHECK (length(streak_key) BETWEEN 1 AND 100),
    anchor_key     TEXT        NOT NULL DEFAULT '' CHECK (length(anchor_key) <= 100),
    idle_since     TIMESTAMPTZ,
    decided_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    decision       TEXT        NOT NULL CHECK (decision IN ('nudge', 'ask', 'leave', 'invalid')),
    message        TEXT        NOT NULL DEFAULT '' CHECK (char_length(message) <= 4000),
    quote_id       TEXT        NOT NULL DEFAULT '' CHECK (length(quote_id) <= 100),
    quote_at       TIMESTAMPTZ,
    quote_text     TEXT        NOT NULL DEFAULT '' CHECK (char_length(quote_text) <= 500),
    covered_action TEXT        NOT NULL DEFAULT '' CHECK (char_length(covered_action) <= 300),
    question       TEXT        NOT NULL DEFAULT '' CHECK (char_length(question) <= 4000),
    options        JSONB       NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(options) = 'array'),
    reason         TEXT        NOT NULL DEFAULT '' CHECK (char_length(reason) <= 1000),
    mode           TEXT        NOT NULL CHECK (mode IN ('shadow', 'live')),
    outcome        TEXT        NOT NULL CHECK (
                       outcome IN ('would_send', 'would_ask', 'left', 'queued', 'sent', 'asked',
                                   'relay_queued', 'relayed', 'failed', 'ask_closed')
                       OR outcome ~ '^suppressed:[a-z_]{1,40}$'),
    detail         TEXT        NOT NULL DEFAULT '' CHECK (char_length(detail) <= 1000),
    ask_id         BIGINT,
    send_queue_id  BIGINT,
    relay_text     TEXT        NOT NULL DEFAULT '' CHECK (char_length(relay_text) <= 8000),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The stall classifier (Jev, internal/chiefnudge/jev.go) on the session's
    -- last message: its label, its three probabilities, the ask score (the
    -- needs_input and asks_permission probabilities summed), the model the
    -- response says it served, the call's latency, and its error class ('' =
    -- none; all zero with no error = not run).
    jev_label           TEXT             NOT NULL DEFAULT '' CHECK (jev_label IN ('', 'needs_input', 'asks_permission', 'neither')),
    jev_needs_input     DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_asks_permission DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_neither         DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_ask_score       DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_model           TEXT             NOT NULL DEFAULT '' CHECK (char_length(jev_model) <= 200),
    jev_latency_ms      INTEGER          NOT NULL DEFAULT 0,
    jev_error           TEXT             NOT NULL DEFAULT '' CHECK (char_length(jev_error) <= 100),
    -- 🔴 A LIVE SEND ONLY: a shadow decision never holds an ask or a queued row.
    CONSTRAINT chief_nudges_shadow_sends_nothing CHECK (
        mode = 'live' OR (ask_id IS NULL AND send_queue_id IS NULL)),
    CONSTRAINT chief_nudges_one_per_streak UNIQUE (owner_id, session_kind, session_key, streak_key)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.chief_nudges ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS idx_chief_nudges_owner_decided ON public.chief_nudges (owner_id, decided_at DESC);
-- The sweep's reconcile: live decisions still in flight.
CREATE INDEX IF NOT EXISTS idx_chief_nudges_pending ON public.chief_nudges (id)
    WHERE outcome IN ('queued', 'asked', 'relay_queued');
CREATE INDEX IF NOT EXISTS idx_chief_nudges_ask ON public.chief_nudges (ask_id) WHERE ask_id IS NOT NULL;

ALTER TABLE public.chief_nudges ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.chief_nudges FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.chief_nudges;
CREATE POLICY owner_isolation ON public.chief_nudges
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.chief_nudges TO muster_app;

ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS chief_nudge_id BIGINT;
ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_one_origin;
ALTER TABLE public.send_queue ADD CONSTRAINT send_queue_one_origin
    CHECK (route_id IS NULL OR chief_nudge_id IS NULL);
