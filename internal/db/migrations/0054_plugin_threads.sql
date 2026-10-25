-- muster 0054 — A REPLY ON A THREAD A SESSION STARTED GOES BACK TO THAT SESSION
-- (internal/plugins/threads.go). docs/plugins.md#a-reply-on-a-thread-a-session-started.
--
--   plugin_messages.thread   the upstream's thread id the plugin reported; NULL: none.
--   plugin_threads           (owner, thread) → the mirrored Claude Code session
--                            (and its host) that STARTED the thread, reported by
--                            that session's PostToolUse hook. The first report
--                            wins: a later one for the same thread changes
--                            nothing (the primary key; INSERT … ON CONFLICT DO
--                            NOTHING).
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0048's shape: one owner can
-- neither read nor bind another's threads. rollback/rls_off.sql turns it off
-- with the rest, and --rls-on re-applies this file (db.RLSOn), so every
-- statement is idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads these):
--   DROP TABLE IF EXISTS plugin_threads;
--   ALTER TABLE plugin_messages DROP COLUMN IF EXISTS thread;
--   DELETE FROM schema_migrations WHERE version = 54;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS thread TEXT;
ALTER TABLE public.plugin_messages DROP CONSTRAINT IF EXISTS plugin_messages_thread_check;
ALTER TABLE public.plugin_messages ADD CONSTRAINT plugin_messages_thread_check
    CHECK (thread IS NULL OR char_length(thread) BETWEEN 1 AND 200);

CREATE TABLE IF NOT EXISTS public.plugin_threads (
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    thread          TEXT        NOT NULL CHECK (char_length(thread) BETWEEN 1 AND 200),
    host            TEXT        NOT NULL CHECK (char_length(host) BETWEEN 1 AND 200),
    claude_session  TEXT        NOT NULL CHECK (char_length(claude_session) BETWEEN 1 AND 200),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, thread)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.plugin_threads ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.plugin_threads ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.plugin_threads FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.plugin_threads;
CREATE POLICY owner_isolation ON public.plugin_threads
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT ON public.plugin_threads TO muster_app;
