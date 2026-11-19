-- muster 0161 — THE MORNING REPORT (internal/morning; docs/features.md#morning-report,
-- docs/schema.md).
--
--   morning_reports   one row per owner per local day: the report the
--       morning job built for that day — its window, whether it was a quiet
--       day, what got done and what was stuck as the job read them (body), how
--       many questions were open then, and when its one push went out.
--
-- 🔴 THE PRIMARY KEY IS THE IDEMPOTENCY: (owner_id, day). The job inserts a
-- day's row before it pushes and skips a day that has one, so a restart, a
-- second tick or a second replica in a rolling update builds nothing twice;
-- pushed_at is claimed by one conditional UPDATE before the send, so the push
-- goes out at most once (a crash between the claim and the send loses that
-- day's push rather than sending it twice).
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0147's shape: one owner's
-- reports are invisible to every other principal. PERSONAL share class (no
-- team access): a report lists its owner's own sessions, tasks and questions.
-- rollback/rls_off.sql turns it off with the rest, and --rls-on re-applies
-- this file (db.RLSOn), so every statement is idempotent.
--
-- body holds muster's own rows only (pull-request titles and refs, task titles, session
-- names and the plain reason each is listed), never a transcript or a model's
-- words, and is bounded below.
--
-- Must apply after its predecessor, 0160_officer_chat_cards
-- (the contiguity test refuses a gap). 🔴 RENUMBER IT (and
-- db.morningReportsMigrationVersion) at merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE public.morning_reports;
--   DELETE FROM schema_migrations WHERE version = 161;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.morning_reports (
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    -- The report's local date (in the server's report time zone).
    day           DATE        NOT NULL,
    -- The window it covers: the previous day's report time to this one.
    window_since  TIMESTAMPTZ NOT NULL,
    window_until  TIMESTAMPTZ NOT NULL CHECK (window_until > window_since),
    -- A quiet day: nothing happened, so nothing was pushed.
    quiet         BOOLEAN     NOT NULL,
    -- How many questions were open when it was built (the push's count; the
    -- page reads the open questions live).
    questions     INTEGER     NOT NULL DEFAULT 0 CHECK (questions >= 0),
    body          JSONB       NOT NULL DEFAULT '{}'::jsonb
                  CHECK (jsonb_typeof(body) = 'object' AND pg_column_size(body) <= 262144),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- When its one push was claimed (NULL: none — a quiet day, or not yet).
    pushed_at     TIMESTAMPTZ,
    PRIMARY KEY (owner_id, day)
);

ALTER TABLE public.morning_reports ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.morning_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.morning_reports FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.morning_reports;
CREATE POLICY owner_isolation ON public.morning_reports
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.morning_reports TO muster_app;
