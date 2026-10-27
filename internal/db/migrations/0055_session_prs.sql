-- muster 0055 — session_prs: which of an owner's mirrored Claude Code sessions
-- AUTHORED which pull request (a `gh pr create` naming it) or pushed which
-- branch (a `git push` naming the repository and branch), read from the
-- session's own transcript. A pull-request review mail is routed to the most
-- recent such session. docs/plugins.md#a-pull-request-review-mail.
--
-- Owned and under forced row-level security in 0048's shape; re-applied by
-- db.RLSOn, so every statement is idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE IF EXISTS session_prs;
--   DELETE FROM schema_migrations WHERE version = 55;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.session_prs (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    kind            TEXT        NOT NULL CHECK (kind IN ('gh-pr-create', 'git-push')),
    repo            TEXT        NOT NULL CHECK (char_length(repo) BETWEEN 3 AND 200
                                                AND repo ~ '^[a-z0-9_.-]+/[a-z0-9_.-]+$'),
    pr              BIGINT      CHECK (pr > 0),
    head_ref        TEXT        CHECK (char_length(head_ref) BETWEEN 1 AND 255),
    claude_session  TEXT        NOT NULL CHECK (char_length(claude_session) BETWEEN 1 AND 200),
    host            TEXT        NOT NULL CHECK (char_length(host) BETWEEN 1 AND 200),
    first_seen      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A created pull request names its number and no branch; a push its
    -- branch and no number.
    CONSTRAINT session_prs_kind_shape CHECK (
        (kind = 'gh-pr-create' AND pr IS NOT NULL AND head_ref IS NULL) OR
        (kind = 'git-push' AND head_ref IS NOT NULL AND pr IS NULL))
);

-- One row per (owner, session, what it authored): seeing it again moves
-- last_seen (RecordPRSessions' ON CONFLICT names exactly these expressions).
CREATE UNIQUE INDEX IF NOT EXISTS session_prs_one ON public.session_prs
    (owner_id, claude_session, kind, repo, (coalesce(pr, 0)), (coalesce(head_ref, '')));
-- ResolvePRSession's two lookups.
CREATE INDEX IF NOT EXISTS session_prs_by_pr ON public.session_prs (owner_id, repo, pr) WHERE pr IS NOT NULL;
CREATE INDEX IF NOT EXISTS session_prs_by_ref ON public.session_prs (owner_id, repo, head_ref) WHERE head_ref IS NOT NULL;

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.session_prs ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.session_prs ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.session_prs FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.session_prs;
CREATE POLICY owner_isolation ON public.session_prs
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.session_prs TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.session_prs_id_seq TO muster_app;
