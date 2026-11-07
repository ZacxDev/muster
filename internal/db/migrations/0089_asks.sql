-- muster 0089 — THE ASK LOG (internal/asklog; docs/asks.md, and the design
-- "ask telemetry, review, and progressive auto-approval", phase 1).
--
--   asks   one row per question an agent put to its owner, whatever kind:
--       a plain-text question at the end of a turn (kind 'plain_text'), an
--       AskUserQuestion picker ('ask_user_question'), an attention_ask
--       ('attention_ask'), and an opencode form or permission request
--       ('opencode_question', 'opencode_permission'). With the answer the
--       owner gave, who gave it, how long it took and how the ask ended.
--
-- PHASE 1 WRITES the kind, where, the redacted question and options, the
-- answer and its provenance, answered_by, the outcome and the two times, and
-- (with MUSTER_ASK_LOG_CLASSIFY) the stall classifier's scores. The columns
-- the later phases fill — proposed_class / proposed_answer (the LLM
-- proposer), never_reason (the never-auto list), shadow_rule_id /
-- shadow_answer_kind (shadow rules) and agreement — are created here, at their
-- empty defaults, so the design's one table is one migration; nothing writes
-- them yet.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0073's (session_episodes)
-- SHAPE. owner_isolation keeps one user from reading another's asks; there is
-- no admin read path. The one system-scope write is the retention sweep
-- (asklog.PG.Purge), which clears text and deletes old rows by age only.
--
-- 🔴 QUESTION AND ANSWER TEXT IS TRANSCRIPT CONTENT. The store redacts every
-- text column (redact.Secrets) before it is written, and the retention sweep
-- clears question, question_norm, answer and proposed_answer once a row is
-- older than MUSTER_ASK_LOG_TEXT_TTL (default 30 days), stamping
-- text_purged_at; asks_text_purged refuses a purged row that still holds
-- text. The rest of the row is deleted after 180 days.
--
-- 🔴 A CAPTURE THAT RUNS TWICE RECORDS ONE ROW: asks_one_per_source is unique
-- on the source's own id (the attention ask, the askq entry, the opencode
-- prompt) wherever there is one. A plain-text question has none ('').
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). The
-- number was the next free one when written (0088 was taken by a sibling
-- branch); it may need renumbering at merge.
--
-- 🔴 AN ASK'S EPISODE IS THE SAME OWNER'S (0006's composite-key rule): the
-- link is (episode_id, owner_id) → session_episodes (id, owner_id), so a row
-- cannot name another owner's episode; session_episodes gains the unique
-- index on (id, owner_id) the reference needs. An episode deleted leaves its
-- asks, unlinked (ON DELETE SET NULL (episode_id)).
--
-- ROLLBACK (after rolling the image back; an older image never reads the table):
--   DROP TABLE IF EXISTS public.asks;
--   DROP INDEX IF EXISTS public.session_episodes_id_owner_key;
--   DELETE FROM schema_migrations WHERE version = 89;

SET LOCAL lock_timeout = '3s';

CREATE UNIQUE INDEX IF NOT EXISTS session_episodes_id_owner_key ON public.session_episodes (id, owner_id);

CREATE TABLE IF NOT EXISTS public.asks (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    kind            TEXT        NOT NULL,
    runtime         TEXT        NOT NULL,
    mirror_host     TEXT        NOT NULL DEFAULT '' CHECK (char_length(mirror_host) <= 200),
    mirror_session  TEXT        NOT NULL DEFAULT '' CHECK (char_length(mirror_session) <= 200),
    mirror_window   TEXT        NOT NULL DEFAULT '' CHECK (char_length(mirror_window) <= 32),
    claude_session  TEXT        NOT NULL DEFAULT '' CHECK (char_length(claude_session) <= 200),
    agent_id        BIGINT,
    chat_session_id BIGINT,
    repo            TEXT        NOT NULL DEFAULT '' CHECK (char_length(repo) <= 200),
    cwd             TEXT        NOT NULL DEFAULT '' CHECK (char_length(cwd) <= 300),
    episode_id      BIGINT,
    source_ref      TEXT        NOT NULL DEFAULT '' CHECK (char_length(source_ref) <= 200),
    question        TEXT        NOT NULL DEFAULT '' CHECK (char_length(question) <= 4000),
    question_norm   TEXT        NOT NULL DEFAULT '' CHECK (char_length(question_norm) <= 500),
    options         JSONB       NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(options) = 'array'),
    -- The stall classifier (internal/chiefnudge/jev.go), as on session_episodes.
    jev_label           TEXT             NOT NULL DEFAULT '' CHECK (jev_label IN ('', 'needs_input', 'asks_permission', 'neither')),
    jev_needs_input     DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_asks_permission DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_neither         DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_ask_score       DOUBLE PRECISION NOT NULL DEFAULT 0,
    jev_model           TEXT             NOT NULL DEFAULT '' CHECK (char_length(jev_model) <= 200),
    jev_error           TEXT             NOT NULL DEFAULT '' CHECK (char_length(jev_error) <= 100),
    -- The later phases' columns (see the header): empty until they ship.
    proposed_class     TEXT   NOT NULL DEFAULT '' CHECK (proposed_class IN ('', 'continue', 'merge', 'choice', 'info', 'permission', 'other')),
    proposed_answer    TEXT   NOT NULL DEFAULT '' CHECK (char_length(proposed_answer) <= 500),
    never_reason       TEXT   NOT NULL DEFAULT '' CHECK (char_length(never_reason) <= 32),
    shadow_rule_id     BIGINT,
    shadow_answer_kind TEXT   NOT NULL DEFAULT '' CHECK (shadow_answer_kind IN ('', 'continue', 'merge_when_green')),
    answer             TEXT   NOT NULL DEFAULT '' CHECK (char_length(answer) <= 8000),
    answered_by        TEXT   NOT NULL DEFAULT '',
    answer_provenance  TEXT   NOT NULL DEFAULT '' CHECK (answer_provenance IN ('', 'muster_send', 'pane', 'nudge', 'route')),
    agreement          TEXT   NOT NULL DEFAULT '' CHECK (agreement IN ('', 'agree', 'disagree', 'other')),
    outcome            TEXT   NOT NULL DEFAULT '',
    asked_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    answered_at     TIMESTAMPTZ,
    text_purged_at  TIMESTAMPTZ,
    CONSTRAINT asks_episode_owner_fkey FOREIGN KEY (episode_id, owner_id)
        REFERENCES public.session_episodes (id, owner_id) ON DELETE SET NULL (episode_id),
    CONSTRAINT asks_kind_check CHECK (kind IN ('plain_text', 'ask_user_question', 'attention_ask', 'opencode_question', 'opencode_permission')),
    CONSTRAINT asks_runtime_check CHECK (runtime IN ('claude-code', 'opencode', 'agent')),
    CONSTRAINT asks_answered_by_check CHECK (answered_by IN ('', 'owner', 'chief', 'shadow') OR answered_by ~ '^auto:[1-9][0-9]{0,18}$'),
    CONSTRAINT asks_outcome_check CHECK (outcome IN ('', 'answered', 'answered_in_session', 'declined', 'expired', 'cancelled',
        'superseded', 'moved_on', 'gone')),
    -- A purged row holds no text.
    CONSTRAINT asks_text_purged CHECK (text_purged_at IS NULL
        OR (question = '' AND question_norm = '' AND answer = '' AND proposed_answer = ''))
);

ALTER TABLE public.asks ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS asks_owner_asked ON public.asks (owner_id, asked_at DESC);
CREATE INDEX IF NOT EXISTS asks_owner_kind_asked ON public.asks (owner_id, kind, asked_at DESC);
CREATE INDEX IF NOT EXISTS asks_owner_shadow_rule ON public.asks (owner_id, shadow_rule_id);
CREATE UNIQUE INDEX IF NOT EXISTS asks_one_per_source ON public.asks (owner_id, kind, source_ref) WHERE source_ref <> '';
-- The retention sweep's read: rows whose text is still held, by age.
CREATE INDEX IF NOT EXISTS asks_text_unpurged ON public.asks (asked_at) WHERE text_purged_at IS NULL;

ALTER TABLE public.asks ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.asks FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.asks;
CREATE POLICY owner_isolation ON public.asks
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.asks TO muster_app;
