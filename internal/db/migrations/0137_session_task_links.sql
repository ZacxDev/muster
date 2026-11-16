-- muster 0137 — A SESSION'S DECLARED TASKS, AND ONE DIGEST PER BATCH OF ROUTES
-- (docs/focus.md#a-sessions-tasks, docs/chief.md#route_many).
--
--   agent_focus.task_keys     the tasks a MIRRORED session says it works
--       under, as it declared them itself with the track_task tool
--       (focus.PGStore.SetTaskLink): each a plugin task key
--       ("<namespace>:<id>", the key a plugin task reference is filed under,
--       docs/plugins.md#task-references). At most focus.MaxTaskKeys; '{}' for
--       none.
--   agent_focus.task_none     the session's "deliberately untracked: <one
--       line>" declaration, NULL when it made none. Never beside a key
--       (agent_focus_task_none_alone).
--   agent_focus.task_note     the session's optional short note on what it
--       does under those keys, NULL for none.
--   agent_focus.task_set_at   when the declaration was last written (by the
--       session itself: its one writer); NULL until the first one. A clear
--       keeps it, so "cleared at" is still known.
--   session_routes.batch_id   the route_many call a request route was made by
--       ("rb_" + 24 hex), NULL for every other route. A batch member's answer
--       is NEVER delivered to the chief by itself: the batch's one digest is
--       (internal/api route_many.go), once every member has ended.
--       The batch has no claim column: its digest is owed while any member
--       owes a hand-back, and is delivered under the chief's own claims.
--   session_routes.batch_size   how many members route_many SEALED the batch
--       with once it had made every route of the call (plugins.PGStore.
--       SealBatch), the same on every member; NULL until then. An unsealed
--       batch is never digested, so a member ending while the call still
--       makes the rest cannot start a digest of part of it.
--
-- 🔴 THE TASK LINK IS THE SESSION'S OWN WORD, AND ITS ONE WRITER IS THE
-- SESSION. It is written only through the mirror door, for the calling
-- session's own subject (internal/api track_task_tool.go); nothing derives
-- it from focus text, a transcript or a keyword match. It lives on the
-- session's focus row because that row is the session's per-owner record —
-- the same row as its name (0090) — so it is owned, under the same FORCE
-- row-level security and 0127 share class as the focus, and pruned with it.
--
-- New columns inherit agent_focus's and session_routes' policies, so nothing
-- here touches a policy, grant or default and db.RLSOn re-applies nothing.
-- Idempotent: every statement can run twice.
--
-- Must apply after its predecessor (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads them):
--   ALTER TABLE agent_focus DROP CONSTRAINT IF EXISTS agent_focus_task_keys_cap;
--   ALTER TABLE agent_focus DROP CONSTRAINT IF EXISTS agent_focus_task_none_alone;
--   ALTER TABLE agent_focus DROP CONSTRAINT IF EXISTS agent_focus_task_text_cap;
--   ALTER TABLE agent_focus DROP CONSTRAINT IF EXISTS agent_focus_task_mirror_only;
--   ALTER TABLE agent_focus DROP COLUMN IF EXISTS task_keys, DROP COLUMN IF EXISTS task_none,
--       DROP COLUMN IF EXISTS task_note, DROP COLUMN IF EXISTS task_set_at;
--   DROP INDEX IF EXISTS idx_session_routes_batch;
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_batch_id_shape;
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_batch_is_request;
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_batch_size_range;
--   ALTER TABLE session_routes DROP COLUMN IF EXISTS batch_id, DROP COLUMN IF EXISTS batch_size;
--   DELETE FROM schema_migrations WHERE version = 137;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agent_focus ADD COLUMN IF NOT EXISTS task_keys   TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE public.agent_focus ADD COLUMN IF NOT EXISTS task_none   TEXT;
ALTER TABLE public.agent_focus ADD COLUMN IF NOT EXISTS task_note   TEXT;
ALTER TABLE public.agent_focus ADD COLUMN IF NOT EXISTS task_set_at TIMESTAMPTZ;

-- focus.MaxTaskKeys, each key at most notes.MaxTagRunes (the store checks first).
ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_task_keys_cap;
ALTER TABLE public.agent_focus ADD CONSTRAINT agent_focus_task_keys_cap
    CHECK (cardinality(task_keys) <= 10);
ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_task_none_alone;
ALTER TABLE public.agent_focus ADD CONSTRAINT agent_focus_task_none_alone
    CHECK (task_none IS NULL OR cardinality(task_keys) = 0);
-- focus.MaxTaskTextRunes for the none line and the note.
ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_task_text_cap;
ALTER TABLE public.agent_focus ADD CONSTRAINT agent_focus_task_text_cap
    CHECK ((task_none IS NULL OR char_length(task_none) BETWEEN 1 AND 200)
           AND (task_note IS NULL OR char_length(task_note) BETWEEN 1 AND 200));
-- A dispatched agent's task is its own row's (agents.note_id): only a mirrored
-- session declares one here.
ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_task_mirror_only;
ALTER TABLE public.agent_focus ADD CONSTRAINT agent_focus_task_mirror_only
    CHECK (subject_kind = 'mirror' OR (cardinality(task_keys) = 0 AND task_none IS NULL AND task_set_at IS NULL));

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS batch_id TEXT;
ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS batch_size INTEGER;
ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_batch_size_range;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_batch_size_range
    CHECK (batch_size IS NULL OR (batch_id IS NOT NULL AND batch_size BETWEEN 1 AND 50));
ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_batch_id_shape;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_batch_id_shape
    CHECK (batch_id IS NULL OR batch_id ~ '^rb_[0-9a-f]{24}$');
-- Only a request route (0084: the owner's own request) is ever a batch member.
ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_batch_is_request;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_batch_is_request
    CHECK (batch_id IS NULL OR request_message_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_session_routes_batch
    ON public.session_routes (batch_id) WHERE batch_id IS NOT NULL;
