-- muster 0056 — mail_resumes: every time muster decided to RESUME an ended
-- Claude Code session because agent mail arrived for it (internal/api's
-- mail_resume.go): the message, the session, the sender, the host asked, the
-- window it started and how it ended. One row per plugin message, so a message
-- resumes a session at most ONCE however often it is dispatched; the rate
-- limits (per session and per host, per hour) are counted from these rows.
-- docs/plugins.md#resuming-an-ended-session.
--
-- Owned and under forced row-level security in 0048's shape; re-applied by
-- db.RLSOn, so every statement is idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE IF EXISTS mail_resumes;
--   DELETE FROM schema_migrations WHERE version = 56;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.mail_resumes (
    id                 BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id           BIGINT      NOT NULL
                       DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                       REFERENCES public.users (id),
    plugin_message_id  BIGINT      NOT NULL CHECK (plugin_message_id > 0),
    claude_session     TEXT        NOT NULL CHECK (char_length(claude_session) BETWEEN 1 AND 200),
    sender             TEXT        NOT NULL CHECK (char_length(sender) BETWEEN 1 AND 320),
    -- Why muster routes the message to that session: 'thread' or 'pr'.
    via                TEXT        NOT NULL CHECK (via IN ('thread', 'pr')),
    -- The host that STARTED the window (set with spawned_at); NULL until one did.
    host               TEXT        CHECK (char_length(host) BETWEEN 1 AND 200),
    window_id          TEXT        CHECK (char_length(window_id) BETWEEN 2 AND 32),
    state              TEXT        NOT NULL DEFAULT 'claimed'
                       CHECK (state IN ('claimed', 'spawned', 'delivered', 'timed_out', 'refused')),
    detail             TEXT        CHECK (char_length(detail) <= 1000),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    spawned_at         TIMESTAMPTZ,
    finished_at        TIMESTAMPTZ,
    CONSTRAINT mail_resumes_spawn_shape CHECK ((spawned_at IS NULL) = (host IS NULL))
);

-- One resume per message (ClaimMailResume's ON CONFLICT names exactly this).
CREATE UNIQUE INDEX IF NOT EXISTS mail_resumes_one ON public.mail_resumes (owner_id, plugin_message_id);
-- The two rate limits' counts.
CREATE INDEX IF NOT EXISTS mail_resumes_by_session ON public.mail_resumes (owner_id, claude_session, spawned_at)
    WHERE spawned_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS mail_resumes_by_host ON public.mail_resumes (owner_id, host, spawned_at)
    WHERE spawned_at IS NOT NULL;

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.mail_resumes ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.mail_resumes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.mail_resumes FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.mail_resumes;
CREATE POLICY owner_isolation ON public.mail_resumes
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.mail_resumes TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.mail_resumes_id_seq TO muster_app;
