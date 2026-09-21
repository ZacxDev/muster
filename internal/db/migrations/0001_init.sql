-- muster 0001 — the whole schema, as one statement of its END STATE.
--
-- 🔴 THIS IS NOT A REPLAY. muster's tables were extracted from a project whose
-- migration directory reached this same shape across 38 files, with the two
-- products' tables interleaved INSIDE individual files — the first of them
-- created four tables that moved here and three that did not. Porting that
-- sequence would have carried the entanglement across and made muster's
-- migration 0001 a document about somebody else's history. This file states
-- what the tables ARE; every later muster migration is a change to it.
--
-- 🔴 EVERY `id` IS `GENERATED ALWAYS AS IDENTITY`, AND THAT IS LOAD-BEARING FOR
-- A MIGRATION PATH THAT DOES NOT EXIST YET. Existing rows arrive by
-- `pg_dump --data-only`, whose `COPY` path is EXEMPT from the ALWAYS
-- restriction and therefore works. A hand-written `INSERT` migrator is not
-- exempt: it fails on EVERY row without `OVERRIDING SYSTEM VALUE` plus a manual
-- `setval`. Weakening a column to `BY DEFAULT` to make some future importer
-- easier would silently remove the guarantee that nothing but the sequence ever
-- mints an id — so if an importer needs it, give the importer `COPY`, not this
-- file a relaxation.
--
-- ⚠ ONE TABLE HAS NO IDENTITY COLUMN AND IT IS DELIBERATE: `task_sessions` is
-- keyed by `(note_id, session_id)` because a session is not an entity muster
-- owns — it is an opaque identifier minted elsewhere. `github_connection` has
-- no identity either; it is a singleton pinned by a CHECK.
--
-- ⚠ THE `session_id`/`source_session_id` COLUMNS ARE DELIBERATELY FK-FREE.
-- They name sessions in another system. A foreign key would assert muster is
-- the authority on what a session is, and it is not; best-effort resolution is
-- the designed behaviour, not a missing constraint.

-- ---------------------------------------------------------------------------
-- TASKS
--
-- `notes` is the task table. The name is historical and is kept because every
-- id in it is a task number people cite; a rename would be a data migration
-- that buys a word.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS notes (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    directory         TEXT        NOT NULL DEFAULT '',
    body              TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    status            TEXT        NOT NULL DEFAULT 'open',
    model             TEXT        NOT NULL DEFAULT '',
    repo              TEXT        NOT NULL DEFAULT '',
    repo_branch       TEXT        NOT NULL DEFAULT '',
    -- The privilege profiles a dispatch off this task should grant. Bare ids
    -- rather than a join table: the set is read as a unit and written as a
    -- unit, and it is ADVISORY — a task may name a profile that was since
    -- deleted, and a dangling id must degrade to "not granted", never to a
    -- failed dispatch.
    grant_profiles    BIGINT[]    NOT NULL DEFAULT '{}',
    -- Provenance. Nullable because most tasks have none, and '' would be a
    -- second spelling of the same absence.
    source_type       TEXT,
    source_session_id TEXT,
    tags              TEXT[]      NOT NULL DEFAULT '{}',
    title             TEXT        NOT NULL DEFAULT '',
    -- Soft delete. A task id is cited in commits, chat and other systems, so a
    -- hard DELETE turns every one of those references into a 404 with no
    -- explanation.
    deleted_at        TIMESTAMPTZ,
    CONSTRAINT notes_status_check
        CHECK (status IN ('open', 'in_progress', 'ready_for_review', 'complete'))
);

CREATE INDEX IF NOT EXISTS idx_notes_directory  ON notes (directory);
CREATE INDEX IF NOT EXISTS idx_notes_status     ON notes (status);
CREATE INDEX IF NOT EXISTS idx_notes_updated_at ON notes (updated_at DESC);
-- The list view's index. PARTIAL on `deleted_at IS NULL` because that view
-- never shows deleted rows, so the index should not carry them either.
CREATE INDEX IF NOT EXISTS idx_notes_live_updated_at
    ON notes (updated_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS notes_tags_gin ON notes USING GIN (tags);

CREATE TABLE IF NOT EXISTS note_comments (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    note_id    BIGINT      NOT NULL REFERENCES notes (id) ON DELETE CASCADE,
    -- Opaque. A comment author may be a person, an agent or an integration;
    -- muster does not own an identity table and must not pretend to.
    author     TEXT        NOT NULL DEFAULT '',
    body       TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_note_comments_note ON note_comments (note_id, created_at);

CREATE TABLE IF NOT EXISTS note_attachments (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    note_id      BIGINT      NOT NULL REFERENCES notes (id) ON DELETE CASCADE,
    filename     TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes   BIGINT      NOT NULL,
    -- ⚠ BYTES IN THE DATABASE, NOT AN OBJECT-STORE KEY. That is a real choice
    -- with a real ceiling, made so a stranger's muster needs Postgres and
    -- nothing else. If you put large media here you will feel it in every
    -- backup.
    data         BYTEA       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_note_attachments_note_id ON note_attachments (note_id);

-- Which sessions touched which task, and how.
--
-- ⚠ `project`/`cwd`/`host` ARE DENORMALISED ON PURPOSE. They are written once,
-- from whatever the caller knew at the time, so the row stays readable when the
-- system that minted `session_id` is gone or unreachable. Do not "fix" this
-- into a join.
CREATE TABLE IF NOT EXISTS task_sessions (
    note_id       BIGINT      NOT NULL REFERENCES notes (id) ON DELETE CASCADE,
    session_id    TEXT        NOT NULL,
    role          TEXT        NOT NULL,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    project       TEXT        NOT NULL DEFAULT '',
    cwd           TEXT        NOT NULL DEFAULT '',
    host          TEXT        NOT NULL DEFAULT '',
    -- Whether this session has read the task's DETAIL, as opposed to seeing it
    -- in a list. Effectively write-once.
    detail_seen   BOOLEAN     NOT NULL DEFAULT FALSE,
    PRIMARY KEY (note_id, session_id),
    CONSTRAINT task_sessions_role_check
        CHECK (role IN ('created', 'worked', 'read'))
);

-- The reverse lookup: "what has this session been doing".
CREATE INDEX IF NOT EXISTS idx_task_sessions_reverse
    ON task_sessions (session_id, last_seen_at DESC);

-- ---------------------------------------------------------------------------
-- AGENTS
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS agents (
    id                       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name                     TEXT        NOT NULL UNIQUE,
    namespace                TEXT        NOT NULL,
    display_name             TEXT        NOT NULL DEFAULT '',
    repo                     TEXT        NOT NULL DEFAULT '',
    repo_branch              TEXT        NOT NULL DEFAULT '',
    -- ON DELETE SET NULL, not CASCADE: deleting a task must not delete the
    -- agent that worked it. This is the only FK in the schema whose direction
    -- makes that distinction matter.
    note_id                  BIGINT      REFERENCES notes (id) ON DELETE SET NULL,
    note_text                TEXT        NOT NULL DEFAULT '',
    pending_note             TEXT        NOT NULL DEFAULT '',
    status                   TEXT        NOT NULL DEFAULT 'pending',
    -- The agent's OWN callback credential — one value per agent, never the
    -- shared server token. A blank value means "not issued yet".
    hooks_token              TEXT        NOT NULL DEFAULT '',
    kicked_off               BOOLEAN     NOT NULL DEFAULT FALSE,
    last_output              TEXT        NOT NULL DEFAULT '',
    error_message            TEXT        NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    model                    TEXT        NOT NULL DEFAULT '',
    -- Kickoff bookkeeping: which instance was supposed to receive the first
    -- message, how many times it restarted underneath us, and a lease so two
    -- reconcilers cannot both deliver it.
    kickoff_pod              TEXT        NOT NULL DEFAULT '',
    kickoff_restarts         INTEGER     NOT NULL DEFAULT 0,
    kickoff_attempts         INTEGER     NOT NULL DEFAULT 0,
    kickoff_error            TEXT        NOT NULL DEFAULT '',
    kickoff_claim_owner      TEXT        NOT NULL DEFAULT '',
    kickoff_claim_expires_at TIMESTAMPTZ,
    CONSTRAINT agents_status_check
        CHECK (status IN ('pending', 'provisioning', 'running', 'stopped', 'error'))
);

CREATE INDEX IF NOT EXISTS idx_agents_status     ON agents (status);
CREATE INDEX IF NOT EXISTS idx_agents_created_at ON agents (created_at DESC);
-- 🔴 UNIQUE, AND PARTIAL ON `<> ''`. The uniqueness is what makes presenting a
-- token an identification rather than a guess. The partial predicate is what
-- lets MANY agents sit at the default '' — without it the second
-- not-yet-issued agent would collide with the first.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_hooks_token
    ON agents (hooks_token) WHERE hooks_token <> '';

CREATE TABLE IF NOT EXISTS chat_sessions (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id    BIGINT      NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    -- Globally unique, not just per-agent: the key is how a runtime addresses a
    -- conversation, and it arrives without an agent id attached.
    session_key TEXT        NOT NULL UNIQUE,
    title       TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, session_key)
);

CREATE INDEX IF NOT EXISTS idx_chat_sessions_agent ON chat_sessions (agent_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS chat_messages (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id   BIGINT      NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    role       TEXT        NOT NULL,
    content    TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Nullable: messages predating a session, and messages a runtime sends
    -- without one, still belong to the agent.
    session_id BIGINT      REFERENCES chat_sessions (id) ON DELETE CASCADE,
    -- A message part. 'text' is prose; tool parts carry the three columns
    -- below. Deliberately NOT a CHECK: the set of part kinds is a property of
    -- whatever runtime is attached, and a constraint here would make adding a
    -- runtime a schema migration.
    kind       TEXT        NOT NULL DEFAULT 'text',
    tool_id    TEXT        NOT NULL DEFAULT '',
    tool_name  TEXT        NOT NULL DEFAULT '',
    tool_ok    BOOLEAN     NOT NULL DEFAULT TRUE
);

CREATE INDEX IF NOT EXISTS idx_chat_messages_agent   ON chat_messages (agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages (session_id, created_at);

-- ---------------------------------------------------------------------------
-- RUNBOOKS
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS runbooks (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name         TEXT        NOT NULL UNIQUE,
    display_name TEXT        NOT NULL DEFAULT '',
    description  TEXT        NOT NULL DEFAULT '',
    spec         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A run is the AUDIT RECORD of a dispatch, so it outlives its inputs.
--
-- 🔴 `runbook_name` AND `rendered_body` ARE COPIES ON PURPOSE, AND
-- `runbook_id` IS `ON DELETE SET NULL` FOR THE SAME REASON. Deleting a runbook,
-- or editing its template, must not rewrite history: what was actually
-- dispatched is the text in `rendered_body`, not whatever the template says
-- today. `agent_id` and `note_id` carry NO foreign key for the same reason —
-- an audit row that vanishes when its agent is torn down is not an audit row.
CREATE TABLE IF NOT EXISTS runbook_runs (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    runbook_id    BIGINT      REFERENCES runbooks (id) ON DELETE SET NULL,
    runbook_name  TEXT        NOT NULL DEFAULT '',
    agent_id      BIGINT      NOT NULL,
    note_id       BIGINT,
    params        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    rendered_body TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_runbook_runs_runbook ON runbook_runs (runbook_id);
CREATE INDEX IF NOT EXISTS idx_runbook_runs_agent   ON runbook_runs (agent_id);

-- ---------------------------------------------------------------------------
-- PRIVILEGE
--
-- The request -> approve -> apply -> audit -> revoke ledger. The RULES a
-- profile carries are driver-specific; the WORKFLOW is not, which is why it is
-- in the core schema and the rules are opaque JSON.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS privilege_profiles (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name         TEXT        NOT NULL UNIQUE,
    display_name TEXT        NOT NULL DEFAULT '',
    description  TEXT        NOT NULL DEFAULT '',
    -- 🔴 OPAQUE TO THE DATABASE, AND IT MUST STAY OPAQUE TO THE CORE. A
    -- provisioner that cannot interpret a spec must REFUSE the grant, not
    -- ignore the parts it does not understand: a UI reading "granted" for a
    -- policy nobody applied is a security defect that reads as coverage.
    spec         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- What is granted RIGHT NOW. The UNIQUE pair is what makes a grant idempotent.
CREATE TABLE IF NOT EXISTS agent_privileges (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id   BIGINT      NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    profile_id BIGINT      NOT NULL REFERENCES privilege_profiles (id) ON DELETE CASCADE,
    granted_by TEXT        NOT NULL DEFAULT '',
    granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, profile_id)
);

CREATE INDEX IF NOT EXISTS idx_agent_privileges_agent   ON agent_privileges (agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_privileges_profile ON agent_privileges (profile_id);

-- What was ASKED FOR. Separate from the grant so a denial leaves a record.
--
-- ⚠ `profile` IS THE PROFILE'S NAME, NOT ITS ID, and `agent_name` duplicates
-- the agent's. Both are copies so the request stays readable after the profile
-- is renamed or the agent is gone — the same reasoning as `runbook_runs`.
CREATE TABLE IF NOT EXISTS privilege_requests (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id   BIGINT      NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    agent_name TEXT        NOT NULL DEFAULT '',
    profile    TEXT        NOT NULL,
    reason     TEXT        NOT NULL DEFAULT '',
    status     TEXT        NOT NULL DEFAULT 'pending',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at TIMESTAMPTZ,
    decided_by TEXT        NOT NULL DEFAULT '',
    CONSTRAINT privilege_requests_status_check
        CHECK (status IN ('pending', 'approved', 'denied'))
);

CREATE INDEX IF NOT EXISTS idx_privilege_requests_status  ON privilege_requests (status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_privilege_requests_created ON privilege_requests (created_at DESC);

-- ---------------------------------------------------------------------------
-- GITHUB CONNECTION
--
-- 🔴 A SINGLETON, PINNED BY A CHECK RATHER THAN BY CONVENTION. `id = 1` is the
-- only legal value, so an UPSERT on 1 is the whole API and a second connection
-- cannot appear by accident.
--
-- 🔴 `token_ciphertext` IS BYTEA AND THE NAME IS A PROMISE. Nothing may write a
-- plaintext token into this column. muster holds the key; a restore into a
-- deployment with a different key must fail to decrypt rather than appear to
-- work — which is the reason the column is not TEXT, where a plaintext value
-- would be indistinguishable from a ciphertext one.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS github_connection (
    id               INTEGER     PRIMARY KEY DEFAULT 1,
    login            TEXT        NOT NULL,
    scopes           TEXT        NOT NULL DEFAULT '',
    token_ciphertext BYTEA       NOT NULL,
    connected_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT github_connection_id_check CHECK (id = 1)
);
