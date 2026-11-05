-- muster 0083 — WHO CREATED A TASK (internal/notes Note.CreatedBy;
-- internal/api/push_task.go taskRefFragment; docs/push.md).
--
-- A task notification names the task's creator. Before this, muster stored
-- WHERE a task came from (source_type, a producer id) but not WHO: a task the
-- owner filed on the board and one a header-less machine POST filed read back
-- identically, and an agent's task_create_gated filing was recorded only as a
-- line of body text and an audit row.
--
--   notes.created_by   "owner"           the owner filed it on the board
--                                        (recorded, but a push names no
--                                        creator for it: it goes to the owner)
--                      "chief"           the owner's chief filed it
--                                        (task_create_gated)
--                      "agent:<name>"    task_create_gated, when the filer
--                                        could not be confirmed as the chief
--                      "plugin:<name>"   a plugin filed it as a copy task
--                      NULL              not recorded (every row before 0083,
--                                        every POST /api/tasks, whose hook or
--                                        personal token names no caller, and
--                                        a plugin task REFERENCE, whose
--                                        creator is the upstream's own and
--                                        lives in ext_snapshot.creator)
--
-- Written once at create and never edited (the editable-field list in
-- PGStore.Update does not name it). A nullable column with no default: no
-- rewrite, every existing row reads as "not recorded". Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the column inherits the
-- table's row-level security. Idempotent. Must apply after 0082 (the contiguity
-- test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads it, and
-- its notifications name no native creator, which is what they did before):
--   ALTER TABLE notes DROP COLUMN IF EXISTS created_by;
--   DELETE FROM schema_migrations WHERE version = 83;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.notes ADD COLUMN IF NOT EXISTS created_by TEXT;
