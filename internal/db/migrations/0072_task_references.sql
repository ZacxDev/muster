-- muster 0072 — PLUGIN TASK REFERENCES (internal/notes/references.go,
-- internal/api/plugin_task_refs.go; docs/plugins.md#task-references).
--
-- A task an upstream system owns is ONE reference row on the board, not a
-- copy. The upstream is canonical: muster keeps the key, a read-only snapshot
-- it renders, and its own links (routes, agents, attention, sessions). Every
-- status or comment a person or agent writes on a reference is QUEUED for the
-- plugin (muster never calls a plugin), and the board changes only when the
-- plugin pushes a fresh snapshot.
--
--   notes.ext_key        the upstream's key (the plugin task key, e.g.
--                        `tracker:4711`); NULL on every ordinary task.
--                        Non-NULL is what MAKES a row a reference.
--   notes.ext_plugin_id  the plugin that created or converted the reference:
--                        the only plugin its queued writes are handed to
--                        (trigger-checked, no foreign key — see below).
--   notes.ext_snapshot   the last applied snapshot (title, url, status,
--                        status_label, rev, updated_at, body, comments), as the
--                        plugin sent it after muster's caps.
--   notes.ext_synced_at  when muster last APPLIED a snapshot.
--
--   task_ref_writes      one write a person or agent made on a reference, for
--                        the plugin to apply upstream: kind status|comment|
--                        refresh, state pending → claimed (leased) → done|failed.
--                        dedupe_key makes a retried producer (a route relay) one
--                        row; at most one refresh is pending or claimed per task.
--
-- 🔴 AT MOST ONE LIVE REFERENCE PER (owner, key) (notes_one_live_ref): the
-- partial unique index is the structural form of "one reference row".
--
-- 🔴 OWNERS ARE CHECKED, NOT ASSUMED. task_ref_writes is owned and under forced
-- row-level security (0064's shape); its plugin is a composite (plugin_id,
-- owner_id) key onto plugins, which this schema's re-runnable 0006 body never
-- drops (0015 declares it). Its task is a plain FK onto notes (id) plus a BEFORE
-- trigger reading the task's owner as the writing principal — 0027's shape and
-- reason (a composite FK onto notes would depend on notes_id_owner_key, which
-- 0006's body drops and re-adds). notes.ext_plugin_id is checked by the same
-- kind of trigger: a reference names one of its owner's own plugins.
--
-- 🔴 rollback/rls_off.sql turns task_ref_writes' RLS off with the rest, and
-- --rls-on re-applies this file (db.RLSOn), so every statement is idempotent.
--
-- Must apply after 0071 (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image reads a reference as
-- an ordinary task, which is what it was before it was converted):
--   DROP TABLE IF EXISTS task_ref_writes;
--   DROP TRIGGER IF EXISTS notes_ext_plugin_is_owners ON notes;
--   DROP FUNCTION IF EXISTS notes_ext_plugin_is_owners();
--   DROP FUNCTION IF EXISTS task_ref_writes_task_is_owners();
--   DROP INDEX IF EXISTS notes_one_live_ref;
--   ALTER TABLE notes DROP COLUMN IF EXISTS ext_synced_at, DROP COLUMN IF EXISTS ext_snapshot,
--     DROP COLUMN IF EXISTS ext_plugin_id, DROP COLUMN IF EXISTS ext_key;
--   DELETE FROM schema_migrations WHERE version = 72;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.notes ADD COLUMN IF NOT EXISTS ext_key TEXT;
ALTER TABLE public.notes ADD COLUMN IF NOT EXISTS ext_plugin_id BIGINT;
ALTER TABLE public.notes ADD COLUMN IF NOT EXISTS ext_snapshot JSONB;
ALTER TABLE public.notes ADD COLUMN IF NOT EXISTS ext_synced_at TIMESTAMPTZ;

ALTER TABLE public.notes DROP CONSTRAINT IF EXISTS notes_ext_key_check;
ALTER TABLE public.notes ADD CONSTRAINT notes_ext_key_check
    CHECK (ext_key IS NULL OR (length(ext_key) BETWEEN 3 AND 200 AND ext_snapshot IS NOT NULL));

-- 🔴 NO FOREIGN KEY FROM notes TO plugins, DELIBERATELY: one would make
-- `TRUNCATE plugins` (and any plugin delete) reach into `notes`, the table
-- everything else hangs off. The trigger below is the check instead: a
-- reference names an EXISTING plugin of its own owner (a missing plugin reads
-- as no owner and is refused). A plugin row deleted later leaves the id
-- dangling; its writes then fail on task_ref_writes' own key. Plugins are
-- revoked, not deleted, so that is a test-fixture case.
CREATE UNIQUE INDEX IF NOT EXISTS notes_one_live_ref
    ON public.notes (owner_id, ext_key) WHERE deleted_at IS NULL AND ext_key IS NOT NULL;

CREATE OR REPLACE FUNCTION public.notes_ext_plugin_is_owners() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.ext_plugin_id IS NOT NULL
       AND NEW.owner_id IS DISTINCT FROM (SELECT p.owner_id FROM public.plugins p WHERE p.id = NEW.ext_plugin_id) THEN
        RAISE EXCEPTION 'notes: owner % is not the owner of plugin %', NEW.owner_id, NEW.ext_plugin_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS notes_ext_plugin_is_owners ON public.notes;
CREATE TRIGGER notes_ext_plugin_is_owners BEFORE INSERT OR UPDATE OF owner_id, ext_plugin_id
    ON public.notes FOR EACH ROW EXECUTE FUNCTION public.notes_ext_plugin_is_owners();

CREATE TABLE IF NOT EXISTS public.task_ref_writes (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id    BIGINT      NOT NULL
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    note_id     BIGINT      NOT NULL REFERENCES public.notes (id) ON DELETE CASCADE,
    plugin_id   BIGINT      NOT NULL,
    kind        TEXT        NOT NULL CHECK (kind IN ('status', 'comment', 'refresh')),
    status      TEXT        NOT NULL DEFAULT '' CHECK (status IN ('', 'open', 'in_progress', 'ready_for_review', 'complete')),
    body        TEXT        NOT NULL DEFAULT '' CHECK (length(body) <= 200000),
    author      TEXT        NOT NULL DEFAULT '' CHECK (char_length(author) <= 300),
    agent       TEXT        NOT NULL DEFAULT '' CHECK (char_length(agent) <= 200),
    session     TEXT        NOT NULL DEFAULT '' CHECK (char_length(session) <= 200),
    writer      TEXT        NOT NULL DEFAULT '' CHECK (char_length(writer) <= 100),
    dedupe_key  TEXT        CHECK (dedupe_key IS NULL OR length(dedupe_key) BETWEEN 1 AND 300),
    state       TEXT        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'claimed', 'done', 'failed')),
    error       TEXT        NOT NULL DEFAULT '' CHECK (char_length(error) <= 2000),
    attempts    INTEGER     NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    CONSTRAINT task_ref_writes_plugin_owner_fkey FOREIGN KEY (plugin_id, owner_id)
        REFERENCES public.plugins (id, owner_id) ON DELETE CASCADE,
    -- A status write names a status; a comment carries a body; a refresh neither.
    CONSTRAINT task_ref_writes_shape CHECK (
        (kind = 'status' AND status <> '') OR
        (kind = 'comment' AND status = '' AND body <> '') OR
        (kind = 'refresh' AND status = '' AND body = '')),
    CONSTRAINT task_ref_writes_leased CHECK (state <> 'claimed' OR lease_until IS NOT NULL)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.task_ref_writes ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE OR REPLACE FUNCTION public.task_ref_writes_task_is_owners() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_id IS DISTINCT FROM (SELECT n.owner_id FROM public.notes n WHERE n.id = NEW.note_id) THEN
        RAISE EXCEPTION 'task_ref_writes: owner % is not the owner of task %', NEW.owner_id, NEW.note_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS task_ref_writes_task_is_owners ON public.task_ref_writes;
CREATE TRIGGER task_ref_writes_task_is_owners BEFORE INSERT OR UPDATE OF owner_id, note_id
    ON public.task_ref_writes FOR EACH ROW EXECUTE FUNCTION public.task_ref_writes_task_is_owners();

-- A retried producer's write is one row.
CREATE UNIQUE INDEX IF NOT EXISTS task_ref_writes_dedupe
    ON public.task_ref_writes (note_id, dedupe_key) WHERE dedupe_key IS NOT NULL;
-- At most one refresh in flight per task.
CREATE UNIQUE INDEX IF NOT EXISTS task_ref_writes_one_refresh
    ON public.task_ref_writes (note_id) WHERE kind = 'refresh' AND state IN ('pending', 'claimed');
-- The claim: a plugin's writes not yet done.
CREATE INDEX IF NOT EXISTS idx_task_ref_writes_claim
    ON public.task_ref_writes (plugin_id, id) WHERE state IN ('pending', 'claimed');
-- The task page's thread, and the cascade from notes.
CREATE INDEX IF NOT EXISTS idx_task_ref_writes_note ON public.task_ref_writes (note_id, id);

ALTER TABLE public.task_ref_writes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.task_ref_writes FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.task_ref_writes;
CREATE POLICY owner_isolation ON public.task_ref_writes
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.task_ref_writes TO muster_app;
