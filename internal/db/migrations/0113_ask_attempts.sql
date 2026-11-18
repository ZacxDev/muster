-- muster 0113 — ANSWER ATTEMPTS (internal/askattempt; docs/mirroring.md#answer-attempts).
--
--   ask_attempts   one row per Submit or Dismiss of a question card — a
--       mirrored Claude Code AskUserQuestion picker or an opencode question or
--       permission request — from every door that answers one (the card in a
--       session's rendered view, the Attention list, the phone's shell), with
--       how it ended: the outcome ('ok', 'refused' before anything reached the
--       host, 'failed' after) and its reason code (askattempt.Reasons, the one
--       enumerated set), the keys typed, how long it took, and which attempt on
--       that card it was (a retry is attempt 2).
--
-- 🔴 NO QUESTION OR ANSWER TEXT. A row holds the question's SHAPE only — how
-- many questions, each one's option count and whether it is multi-select, and
-- whether an answer of one's own ("Other") was used. The one exception is
-- snapshot: on a failure the host reported, the terminal pane as the host read
-- it then, REDACTED (redact.Secrets) and capped at 8000 characters by the
-- store before it is written (askattempt.PG.Record). A pane can show the
-- question and what was typed into the picker; that is why it is redacted,
-- capped, and kept only as long as the row.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0089's (asks) SHAPE.
-- owner_isolation keeps one user from reading another's attempts. The one
-- system-scope write is the retention sweep (askattempt.PG.Prune), which
-- deletes rows older than 30 days by age only.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). The
-- number was the next free one when written; it may need renumbering at merge
-- (with askAttemptsMigrationVersion in rollback.go).
--
-- ROLLBACK (after rolling the image back; an older image never reads the table):
--   DROP TABLE IF EXISTS public.ask_attempts;
--   DELETE FROM schema_migrations WHERE version = 113;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.ask_attempts (
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    runtime       TEXT        NOT NULL,
    door          TEXT        NOT NULL,
    action        TEXT        NOT NULL,
    method        TEXT        NOT NULL,
    mirror_host   TEXT        NOT NULL DEFAULT '' CHECK (char_length(mirror_host) <= 200),
    mirror_session TEXT       NOT NULL DEFAULT '' CHECK (char_length(mirror_session) <= 200),
    mirror_window TEXT        NOT NULL DEFAULT '' CHECK (char_length(mirror_window) <= 32),
    -- The card: a Claude call's tool_use id, an opencode ask's prompt id.
    card_id       TEXT        NOT NULL DEFAULT '' CHECK (char_length(card_id) <= 200),
    -- The shape: one entry per question.
    questions     INT         NOT NULL DEFAULT 0 CHECK (questions >= 0 AND questions <= 64),
    options       INT[]       NOT NULL DEFAULT '{}',
    multi_select  BOOLEAN[]   NOT NULL DEFAULT '{}',
    other_used    BOOLEAN     NOT NULL DEFAULT false,
    outcome       TEXT        NOT NULL,
    reason        TEXT        NOT NULL CHECK (reason ~ '^[a-z][a-z-]{0,39}$'),
    keys          INT         NOT NULL DEFAULT 0 CHECK (keys >= 0),
    duration_ms   BIGINT      NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    attempt       INT         NOT NULL DEFAULT 1 CHECK (attempt >= 1),
    snapshot      TEXT        NOT NULL DEFAULT '' CHECK (char_length(snapshot) <= 8000),
    CONSTRAINT ask_attempts_runtime_check CHECK (runtime IN ('claude-code', 'opencode')),
    CONSTRAINT ask_attempts_door_check CHECK (door IN ('card', 'attention', 'phone')),
    CONSTRAINT ask_attempts_action_check CHECK (action IN ('submit', 'dismiss')),
    CONSTRAINT ask_attempts_method_check CHECK (method IN ('typed', 'relayed', 'escape', 'message')),
    CONSTRAINT ask_attempts_outcome_check CHECK (outcome IN ('ok', 'refused', 'failed', 'noop')),
    CONSTRAINT ask_attempts_shape_check CHECK (cardinality(options) = questions AND cardinality(multi_select) = questions)
);

ALTER TABLE public.ask_attempts ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS ask_attempts_owner_at ON public.ask_attempts (owner_id, at DESC);
CREATE INDEX IF NOT EXISTS ask_attempts_owner_card ON public.ask_attempts (owner_id, card_id, id);
-- The retention sweep's read: rows by age.
CREATE INDEX IF NOT EXISTS ask_attempts_at ON public.ask_attempts (at);

ALTER TABLE public.ask_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.ask_attempts FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.ask_attempts;
CREATE POLICY owner_isolation ON public.ask_attempts
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.ask_attempts TO muster_app;
