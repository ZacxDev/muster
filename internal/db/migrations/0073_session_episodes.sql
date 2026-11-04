-- muster 0073 — SESSION EPISODES (internal/sessionstate;
-- docs/design/session-state.md, "Episodes" and "Claiming, pushing and
-- clearing").
--
--   session_episodes   one row per STOP of one of the owner's sessions: a
--       mirrored Claude Code or opencode window (session_kind 'mirror',
--       session_key its Claude session id or runtime-qualified key, as the
--       stall sweep keys it) or a dispatched agent's conversation ('agent',
--       chiefnudge.AgentKey). It opens when the session enters needs_you or
--       stalled and ends only after sustained work (end_reason 'worked') or
--       when its window is gone from a host that is online and reporting
--       status ('gone'). A host that dropped never ends one.
--
-- 🔴 THE PARTIAL UNIQUE INDEX IS THE CLAIM. At most one OPEN episode per
-- (owner, kind, key): opening one is INSERT … ON CONFLICT DO NOTHING, and only
-- the inserting process owns it — the pattern of chief_nudges_one_per_streak
-- (0064). Two replicas during a rolling deploy cannot both open (and later
-- both push) the same stop.
--
-- Columns this release writes but nothing reads yet: pushed_at and cleared_at
-- (the episode push, the design's next PR), and the classifier's columns,
-- which the attention section and check-ins will read rather than
-- re-classifying. `asked` is the leave rule's input: true when the stall
-- classifier says the session asked, AND when it failed or never ran — a
-- failure can never quiet a session.
--
-- Must apply after 0072 (the contiguity test refuses a gap). Owned, under the
-- same row-level security as every owned table, and re-applied by db.RLSOn.
-- Idempotent.
--
-- ROLLBACK. An image from before 0073 never reads or writes the table; it can
-- run against this schema unchanged. To remove it: DROP TABLE
-- public.session_episodes (nothing references it).

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.session_episodes (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    session_kind    TEXT        NOT NULL CHECK (session_kind IN ('mirror', 'agent')),
    session_key     TEXT        NOT NULL CHECK (length(session_key) BETWEEN 1 AND 200),
    seq             INTEGER     NOT NULL CHECK (seq >= 1),
    state           TEXT        NOT NULL CHECK (state IN ('needs_you', 'stalled')),
    waiting         BOOLEAN     NOT NULL DEFAULT false,
    asked           BOOLEAN     NOT NULL DEFAULT true,
    runtime         TEXT        NOT NULL DEFAULT '' CHECK (length(runtime) <= 32),
    mirror_host     TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_host) <= 200),
    mirror_session  TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_session) <= 200),
    mirror_window   TEXT        NOT NULL DEFAULT '' CHECK (length(mirror_window) <= 32),
    window_index    INTEGER     NOT NULL DEFAULT 0,
    agent_id        BIGINT,
    chat_session_id BIGINT,
    -- The streak the episode was opened (or last classified) on: an agent
    -- conversation's "msg:<id>", a window's "since:<ms>".
    streak_key      TEXT        NOT NULL DEFAULT '' CHECK (length(streak_key) <= 100),
    -- The stall classifier (internal/chiefnudge/jev.go), as on chief_nudges.
    -- classified_streak '' = not classified yet.
    classified_streak   TEXT             NOT NULL DEFAULT '' CHECK (length(classified_streak) <= 100),
    jev_label           TEXT             NOT NULL DEFAULT '' CHECK (jev_label IN ('', 'needs_input', 'asks_permission', 'neither')),
    jev_needs_input     DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_asks_permission DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_neither         DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_ask_score       DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_model           TEXT             NOT NULL DEFAULT '' CHECK (char_length(jev_model) <= 200),
    jev_error           TEXT             NOT NULL DEFAULT '' CHECK (char_length(jev_error) <= 100),
    opened_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    state_since     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- When the session was first seen working, continuously, during this
    -- episode (NULL: not working). Persisted so a restart cannot reset it.
    busy_since      TIMESTAMPTZ,
    -- The latest sighting of that run of work: an episode ends after a run
    -- whose first and latest sightings are sessionstate.EndAfter apart.
    busy_seen_at    TIMESTAMPTZ,
    ended_at        TIMESTAMPTZ,
    end_reason      TEXT        NOT NULL DEFAULT '' CHECK (end_reason IN ('', 'worked', 'gone')),
    pushed_at       TIMESTAMPTZ,
    cleared_at      TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT session_episodes_end_reason CHECK ((ended_at IS NULL) = (end_reason = '')),
    CONSTRAINT session_episodes_seq UNIQUE (owner_id, session_kind, session_key, seq)
);

ALTER TABLE public.session_episodes ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE UNIQUE INDEX IF NOT EXISTS session_episodes_one_open
    ON public.session_episodes (owner_id, session_kind, session_key) WHERE ended_at IS NULL;

ALTER TABLE public.session_episodes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.session_episodes FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.session_episodes;
CREATE POLICY owner_isolation ON public.session_episodes
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.session_episodes TO muster_app;
