-- muster 0152 — THE FLEET'S ACTION LOG (internal/fleet actions.go; docs/chief.md
-- "Fleet proposals", docs/schema.md).
--
--   fleet_actions   one row per action the chief's fleet orchestration
--       proposes on one of the owner's sessions (MUSTER_FLEET_ORCHESTRATION
--       =propose): a fixed muster-built text, or the chief's own words, that
--       only the owner's tap on its card sends. Proposed by the chief's write
--       tools (session_instruct, session_wrap, work_reassign) or by the
--       session-state pass's deterministic step (a finished session's handoff).
--   send_queue.fleet_action_id   a queued message a fleet action sent: its
--       origin, gated at the paste on the action (one origin per row).
--   fleet_owner_marks.proposals_pushed_at   when the owner was last pushed
--       "N proposals waiting" (one batched push, at most every 30 minutes).
--
-- 🔴 THE TEXT IS THE ACTION'S OWN, NEVER SESSION TEXT. text is a muster-built
-- fixed text, or the chief's own wording the owner reads whole on the card;
-- evidence holds references only (an episode id, a streak, a pull request's
-- number and head). audit_log gets one row per state change carrying ids,
-- never text.
--
-- 🔴 THE LOG IS APPEND-ONLY, BY TRIGGER (fleet_actions_append_only), in the
-- shape of 0144's book: a DELETE is refused; an UPDATE may change only the
-- action's progress — state, outcome, finished_at, the decision (decided_by,
-- decided_via, decided_at) and the links the delivery makes (send_queue_id,
-- route_id, close_after) — and a row already finished cannot change at all.
-- A GRANT cannot say this: muster_app holds DELETE on every table (0007's
-- default privileges). TRUNCATE and DROP fire no row trigger; the app role
-- holds neither.
--
-- 🔴 AT MOST ONE OPEN ACTION PER (owner, session, class)
-- (fleet_actions_one_open): a second proposal of the same class for the same
-- session while one is unfinished is refused (`already_proposed`).
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0147's shape: one owner's
-- actions are invisible to every other principal. PERSONAL share class (no
-- team access): a team's sessions are not in an owner's fleet.
-- rollback/rls_off.sql turns it off with the rest, and --rls-on re-applies
-- this file (db.RLSOn), so every statement is idempotent.
--
-- Must apply after its predecessor, 0151_session_route_target_host (the contiguity
-- test refuses a gap). 🔴 RENUMBER IT (and db.fleetActionsMigrationVersion) at
-- merge if main moved.
--
-- ROLLBACK. 🔴 DRAIN THE FLEET'S QUEUED ROWS FIRST: an older image's queue
-- worker does not know send_queue.fleet_action_id, so it would paste a queued
-- fleet row (the chief's line and text) as if a person had queued it, with no
-- gate on the action. Before the older image starts, cancel them (a row
-- already 'sending' is past cancelling; let it finish):
--   UPDATE public.send_queue SET state = 'cancelled', finished_at = now(),
--       detail = 'cancelled: fleet proposals rolled back', held_until = NULL, hold_token = NULL
--     WHERE fleet_action_id IS NOT NULL AND state = 'queued';
-- Then, after rolling the image back (an older image reads none of the rest):
--   ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_one_origin;
--   ALTER TABLE public.send_queue ADD CONSTRAINT send_queue_one_origin
--       CHECK (num_nonnulls(route_id, chief_nudge_id, chief_forward_dispatch) <= 1);
--   ALTER TABLE public.send_queue DROP COLUMN IF EXISTS fleet_action_id;
--   ALTER TABLE public.fleet_owner_marks DROP COLUMN IF EXISTS proposals_pushed_at;
--   DROP TABLE public.fleet_actions;   (its trigger goes with it)
--   DROP FUNCTION public.fleet_actions_append_only();
--   DELETE FROM schema_migrations WHERE version = 152;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.fleet_actions (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    class           TEXT        NOT NULL CHECK (class IN ('continue', 'handoff', 'stop_note', 'custom', 'wrap', 'reassign', 'close')),
    -- What authorised it. Every class is `propose` in this phase; the policy
    -- columns are the later phases' (NULL until then).
    tier_resolved   TEXT        NOT NULL DEFAULT 'propose' CHECK (tier_resolved IN ('propose')),
    policy_version  BIGINT,
    policy_row_id   BIGINT,
    proposer        TEXT        NOT NULL CHECK (proposer IN ('chief', 'sweep_code')),
    -- The session, as the stall sweep keys it (session_episodes' kind and key),
    -- and where it is.
    subject_kind    TEXT        NOT NULL CHECK (subject_kind IN ('mirror', 'agent')),
    subject_key     TEXT        NOT NULL CHECK (length(subject_key) BETWEEN 1 AND 200),
    mirror_host     TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_host) <= 200),
    mirror_session  TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_session) <= 200),
    mirror_window   TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_window) <= 32),
    agent_id        BIGINT,
    chat_session_id BIGINT,
    -- The stop it was proposed in, and the idle streak it was read on: both
    -- re-read at the tap and at the paste. No foreign key: an episode is never
    -- deleted, and the log must not depend on it being kept.
    episode_id      BIGINT      NOT NULL,
    streak_key      TEXT        NOT NULL DEFAULT '' CHECK (length(streak_key) <= 200),
    evidence        JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(evidence) = 'object' AND length(evidence::text) <= 4096),
    reason          TEXT        NOT NULL DEFAULT '' CHECK (char_length(reason) <= 300),
    -- What will be sent (after muster's attribution line): a fixed text in
    -- full, or the chief's own (custom, at most 4 KiB).
    text            TEXT        NOT NULL CHECK (octet_length(text) BETWEEN 1 AND 8192),
    text_sha256     TEXT        NOT NULL CHECK (text_sha256 ~ '^[0-9a-f]{64}$'),
    -- A reassign's task (0: none) and the session it goes to.
    task_id         BIGINT,
    to_subject      TEXT        NOT NULL DEFAULT '' CHECK (length(to_subject) <= 200),
    state           TEXT        NOT NULL CHECK (state IN ('proposed', 'approved', 'rejected', 'expired', 'queued', 'sent', 'failed')
                                                OR state ~ '^suppressed:[a-z_]{1,40}$'),
    outcome         TEXT        NOT NULL DEFAULT '' CHECK (char_length(outcome) <= 300),
    decided_by      BIGINT,
    decided_via     TEXT        NOT NULL DEFAULT '' CHECK (decided_via IN ('', 'web', 'web_edit', 'web_bulk', 'sweep')),
    decided_at      TIMESTAMPTZ,
    send_queue_id   BIGINT,
    route_id        TEXT,
    -- A wrap's close step: the streak after which its handoff turn has ended.
    close_after     TEXT        NOT NULL DEFAULT '' CHECK (length(close_after) <= 200),
    finished_at     TIMESTAMPTZ,
    CONSTRAINT fleet_actions_finished_is_final CHECK (
        (state IN ('proposed', 'approved', 'queued')) = (finished_at IS NULL) OR (state = 'sent' AND class = 'wrap'))
);

ALTER TABLE public.fleet_actions ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

-- At most one open action per session and class (`already_proposed`).
CREATE UNIQUE INDEX IF NOT EXISTS fleet_actions_one_open
    ON public.fleet_actions (owner_id, subject_kind, subject_key, class) WHERE finished_at IS NULL;
-- The digest's read and the pass's: an owner's open actions.
CREATE INDEX IF NOT EXISTS fleet_actions_open ON public.fleet_actions (owner_id, created_at)
    WHERE finished_at IS NULL;
-- fleet_actions_list's read: newest first.
CREATE INDEX IF NOT EXISTS fleet_actions_recent ON public.fleet_actions (owner_id, created_at DESC);

ALTER TABLE public.fleet_actions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.fleet_actions FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.fleet_actions;
CREATE POLICY owner_isolation ON public.fleet_actions
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.fleet_actions TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.fleet_actions_id_seq TO muster_app;

-- The log is append-only (the header).
CREATE OR REPLACE FUNCTION public.fleet_actions_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'fleet_actions is append-only: an action cannot be deleted (id %)', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.owner_id IS DISTINCT FROM OLD.owner_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.class IS DISTINCT FROM OLD.class
       OR NEW.tier_resolved IS DISTINCT FROM OLD.tier_resolved OR NEW.policy_version IS DISTINCT FROM OLD.policy_version
       OR NEW.policy_row_id IS DISTINCT FROM OLD.policy_row_id
       OR NEW.proposer IS DISTINCT FROM OLD.proposer OR NEW.subject_kind IS DISTINCT FROM OLD.subject_kind
       OR NEW.subject_key IS DISTINCT FROM OLD.subject_key OR NEW.mirror_host IS DISTINCT FROM OLD.mirror_host
       OR NEW.mirror_session IS DISTINCT FROM OLD.mirror_session
       OR NEW.mirror_window IS DISTINCT FROM OLD.mirror_window OR NEW.agent_id IS DISTINCT FROM OLD.agent_id
       OR NEW.chat_session_id IS DISTINCT FROM OLD.chat_session_id
       OR NEW.episode_id IS DISTINCT FROM OLD.episode_id OR NEW.streak_key IS DISTINCT FROM OLD.streak_key
       OR NEW.evidence IS DISTINCT FROM OLD.evidence
       OR NEW.reason IS DISTINCT FROM OLD.reason OR NEW.text IS DISTINCT FROM OLD.text
       OR NEW.text_sha256 IS DISTINCT FROM OLD.text_sha256 OR NEW.task_id IS DISTINCT FROM OLD.task_id
       OR NEW.to_subject IS DISTINCT FROM OLD.to_subject THEN
        RAISE EXCEPTION 'fleet_actions is append-only: only an action''s progress may change (id %)', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.finished_at IS NOT NULL THEN
        RAISE EXCEPTION 'fleet_actions is append-only: a finished action cannot change (id %)', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NEW.finished_at IS NOT NULL AND NEW.finished_at < OLD.created_at THEN
        RAISE EXCEPTION 'fleet_actions is append-only: an action cannot finish before it was created (id %)', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS fleet_actions_append_only ON public.fleet_actions;
CREATE TRIGGER fleet_actions_append_only BEFORE UPDATE OR DELETE ON public.fleet_actions
    FOR EACH ROW EXECUTE FUNCTION public.fleet_actions_append_only();

-- The batched push's clock (A4).
ALTER TABLE public.fleet_owner_marks ADD COLUMN IF NOT EXISTS proposals_pushed_at TIMESTAMPTZ;

-- The send queue's new origin: a message a fleet action sent.
ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS fleet_action_id BIGINT;

-- 0093's one-origin rule, widened to the fleet action.
ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_one_origin;
ALTER TABLE public.send_queue ADD CONSTRAINT send_queue_one_origin
    CHECK (num_nonnulls(route_id, chief_nudge_id, chief_forward_dispatch, fleet_action_id) <= 1);
