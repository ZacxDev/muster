-- muster 0147 — THE FLEET'S ACTION LOG AND THE DIGEST'S MARKS (internal/fleet;
-- docs/chief.md "Fleet digest", docs/schema.md).
--
--   fleet_actions   one row per action muster takes on one of the owner's
--       sessions on the fleet's behalf. Phase 1 writes ONE class, `notify`:
--       a stop of the owner's that muster pushed to them (internal/api
--       pushEpisode, the episode push's one claim), one row per episode. It is
--       open (finished_at NULL) while its episode is open; the session-state
--       pass finishes it with the episode's end reason (outcome) once the
--       episode ends. Later phases add classes and states with their own
--       migrations: the CHECKs below name exactly what this phase writes.
--   fleet_owner_marks   one row per owner: the digest's "since you last
--       looked" baseline (seen_at: the look BEFORE the latest one; looked_at:
--       the latest), and the owner's "Fleet" side chat (fleet_chat_id), the
--       chief chat the digest's "Ask the chief about this" opens.
--
-- 🔴 IDS ONLY, NEVER SESSION TEXT. evidence holds references (an episode id,
-- a stall label, pull-request numbers), reason a muster-built line.
--
-- 🔴 THE LOG IS APPEND-ONLY, BY TRIGGER (fleet_actions_append_only), in the
-- shape of 0144's book: a DELETE is refused, an UPDATE that changes any column
-- but state, outcome and finished_at is refused, and so is any UPDATE of a row
-- already finished — its finished_at cannot move (earlier or later) or be
-- cleared, and its state and outcome are fixed. A GRANT cannot say this:
-- muster_app holds DELETE on every table (0007's default privileges, internal/db
-- approle.go's boot GRANT). TRUNCATE and DROP fire no row trigger; the app role
-- holds neither.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0075's shape: one owner's
-- actions and marks are invisible to every other principal. PERSONAL share
-- class (no team access): a team's sessions are not in an owner's fleet.
-- rollback/rls_off.sql turns it off with the rest, and --rls-on re-applies this
-- file (db.RLSOn), so every statement is idempotent.
--
-- 🔴 NUMBERED 0147, NOT 0145: 0145 and 0146 are held by two open pull requests
-- (the asks' attachments, and the fleet plan's Phase 0). The contiguity test
-- refuses the gap until both land. RENUMBER IT (and
-- db.fleetActionsMigrationVersion) at merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads the
-- tables):
--   DROP TABLE public.fleet_owner_marks;
--   DROP TABLE public.fleet_actions;   (its trigger goes with it)
--   DROP FUNCTION public.fleet_actions_append_only();
--   DELETE FROM schema_migrations WHERE version = 147;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.fleet_actions (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    class         TEXT        NOT NULL CHECK (class IN ('notify')),
    proposer      TEXT        NOT NULL CHECK (proposer IN ('sweep_code')),
    -- The session, as the stall sweep keys it (session_episodes' kind and key).
    subject_kind  TEXT        NOT NULL CHECK (subject_kind IN ('mirror', 'agent')),
    subject_key   TEXT        NOT NULL CHECK (length(subject_key) BETWEEN 1 AND 200),
    mirror_host   TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_host) <= 200),
    mirror_window TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_window) <= 32),
    agent_id      BIGINT,
    -- The stop it was taken on. No foreign key: an episode is never deleted,
    -- and the log must not depend on it being kept.
    episode_id    BIGINT,
    evidence      JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(evidence) = 'object' AND length(evidence::text) <= 4096),
    reason        TEXT        NOT NULL DEFAULT '' CHECK (char_length(reason) <= 300),
    state         TEXT        NOT NULL CHECK (state IN ('sent')),
    -- What became of it: '' while open; a notify's episode end reason after.
    outcome       TEXT        NOT NULL DEFAULT '' CHECK (char_length(outcome) <= 64),
    finished_at   TIMESTAMPTZ,
    CONSTRAINT fleet_actions_outcome_when_finished CHECK ((finished_at IS NULL) = (outcome = ''))
);

ALTER TABLE public.fleet_actions ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

-- One notify per stop: the push claim is already one per episode, and this
-- keeps a replayed claim from writing a second row.
CREATE UNIQUE INDEX IF NOT EXISTS fleet_actions_one_notify_per_episode
    ON public.fleet_actions (owner_id, episode_id) WHERE class = 'notify' AND episode_id IS NOT NULL;
-- The digest's read: an owner's open actions, by session.
CREATE INDEX IF NOT EXISTS fleet_actions_open ON public.fleet_actions (owner_id, subject_kind, subject_key)
    WHERE finished_at IS NULL;

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
       OR NEW.proposer IS DISTINCT FROM OLD.proposer OR NEW.subject_kind IS DISTINCT FROM OLD.subject_kind
       OR NEW.subject_key IS DISTINCT FROM OLD.subject_key OR NEW.mirror_host IS DISTINCT FROM OLD.mirror_host
       OR NEW.mirror_window IS DISTINCT FROM OLD.mirror_window OR NEW.agent_id IS DISTINCT FROM OLD.agent_id
       OR NEW.episode_id IS DISTINCT FROM OLD.episode_id OR NEW.evidence IS DISTINCT FROM OLD.evidence
       OR NEW.reason IS DISTINCT FROM OLD.reason THEN
        RAISE EXCEPTION 'fleet_actions is append-only: only state, outcome and finished_at may change (id %)', OLD.id
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.finished_at IS NOT NULL AND (NEW.finished_at IS DISTINCT FROM OLD.finished_at
       OR NEW.state IS DISTINCT FROM OLD.state OR NEW.outcome IS DISTINCT FROM OLD.outcome) THEN
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

CREATE TABLE IF NOT EXISTS public.fleet_owner_marks (
    owner_id      BIGINT      PRIMARY KEY
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    -- The digest's baseline: the look before the latest (NULL: none yet).
    seen_at       TIMESTAMPTZ,
    -- The latest look (the panel opened on the digest).
    looked_at     TIMESTAMPTZ,
    -- The owner's "Fleet" side chat. ON DELETE SET NULL: a deleted chat is
    -- opened again on demand.
    fleet_chat_id BIGINT      REFERENCES public.chat_sessions (id) ON DELETE SET NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE public.fleet_owner_marks ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.fleet_owner_marks ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.fleet_owner_marks FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.fleet_owner_marks;
CREATE POLICY owner_isolation ON public.fleet_owner_marks
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.fleet_owner_marks TO muster_app;
