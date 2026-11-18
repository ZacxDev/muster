-- muster 0086 — SESSION TAGS: the labels a user puts on a session
-- (internal/sessiontags), shown in the session's header, edited from the
-- session switcher's ⋯ menu ("Tags…"), and matched by the command palette and
-- the Sessions dock's filter. Per user and server-stored, so a tag follows its
-- owner across devices, and PRIVATE: nobody else sees another user's tags.
--
--   session_tags   one row per (owner, subject, tag). A subject is either a
--       dispatched agent (kind 'agent', agent_id) or a mirrored tmux SESSION
--       (kind 'mirror', mirror_host + mirror_session) — never a window: every
--       window of one tmux session carries the same tags.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0017's (pins) SHAPE.
-- owner_isolation keeps one user from reading, adding or removing another's
-- rows; the agent an agent row names is pinned to the same owner by 0015's
-- plugin_rows_owner_is_agents_owner trigger, run with 'nullable' (a mirror
-- row names no agent).
--
-- 🔴 AN AGENT'S TAGS DIE WITH ITS AGENT (ON DELETE CASCADE), as its pins do. A
-- re-dispatched agent of the same name is a new row and carries no tags. A
-- mirror row is an address, kept while its session is not listed.
--
-- 🔴 A TAG IS STORED NORMALIZED (notes.NormalizeTag: lower case, white space
-- to '-'), so the per-subject unique indexes are what make adding a tag twice,
-- in any case, one row. The per-subject count cap (sessiontags.MaxPerTarget)
-- is the store's, checked in the add's own transaction.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT, and nothing here redefines an object 0015 defines.
--
-- Must apply after its predecessor (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads the table):
--   DROP TABLE IF EXISTS public.session_tags;
--   DELETE FROM schema_migrations WHERE version = 86;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.session_tags (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id       BIGINT      NOT NULL
                   DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                   REFERENCES public.users (id),
    kind           TEXT        NOT NULL,
    agent_id       BIGINT      REFERENCES public.agents (id) ON DELETE CASCADE,
    mirror_host    TEXT,
    mirror_session TEXT,
    tag            TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT session_tags_kind_check CHECK (kind IN ('agent', 'mirror')),
    -- Exactly one subject, spelled by the kind's own columns (0017's rule,
    -- without the window).
    CONSTRAINT session_tags_subject_check CHECK (
        (kind = 'agent' AND agent_id IS NOT NULL
            AND mirror_host IS NULL AND mirror_session IS NULL)
        OR (kind = 'mirror' AND agent_id IS NULL
            AND mirror_host IS NOT NULL AND mirror_session IS NOT NULL)),
    -- The wire's own bounds (internal/mirror ValidHost / ValidSession), as 0017.
    CONSTRAINT session_tags_mirror_host_cap CHECK (char_length(mirror_host) BETWEEN 1 AND 63),
    CONSTRAINT session_tags_mirror_session_cap CHECK (octet_length(mirror_session) BETWEEN 1 AND 128),
    -- notes.MaxTagRunes, and already lower case: the store normalizes first.
    CONSTRAINT session_tags_tag_check CHECK (char_length(tag) BETWEEN 1 AND 64 AND tag = lower(tag))
);

-- One row per (owner, subject, tag): adding a tag twice is a no-op.
CREATE UNIQUE INDEX IF NOT EXISTS session_tags_owner_agent_tag
    ON public.session_tags (owner_id, agent_id, tag) WHERE kind = 'agent';
CREATE UNIQUE INDEX IF NOT EXISTS session_tags_owner_mirror_tag
    ON public.session_tags (owner_id, mirror_host, mirror_session, tag) WHERE kind = 'mirror';
-- The cascade from agents looks rows up by agent.
CREATE INDEX IF NOT EXISTS idx_session_tags_agent ON public.session_tags (agent_id) WHERE agent_id IS NOT NULL;

DROP TRIGGER IF EXISTS session_tags_owner_is_agents_owner ON public.session_tags;
CREATE TRIGGER session_tags_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.session_tags FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.session_tags ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.session_tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.session_tags FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.session_tags;
CREATE POLICY owner_isolation ON public.session_tags
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, DELETE ON public.session_tags TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.session_tags_id_seq TO muster_app;
