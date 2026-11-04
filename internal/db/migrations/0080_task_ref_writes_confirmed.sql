-- muster 0080 — A QUEUED STATUS WRITE IS CONFIRMED ONCE (internal/notes/pgstore_refs.go
-- ConfirmRefStatusWrite; internal/api/push_task.go reviewSourceLabel;
-- docs/plugins.md#writes-on-a-reference-are-queued).
--
-- A status a person or agent sets on a reference is queued (0072), and the row
-- moves only when a snapshot reports it. The ready-for-review push then names
-- the write's author ("agent worker-7 via hq") rather than the upstream — so the
-- move has to be correlated with the queued write it confirms:
--
--   task_ref_writes.confirmed_at  when a snapshot's status move was matched to
--                                 this write (same task, same status), or when
--                                 a later move spent it (a write already done
--                                 upstream, queued before the write that move
--                                 matched, or any when it matched none). NULL:
--                                 not yet confirmed.
--
-- 🔴 A WRITE CONFIRMS AT MOST ONE MOVE. Without the mark, a write done days ago
-- would name its author on every later upstream move back to the same status;
-- with it, an unrelated later upstream change reads as the upstream's.
--
-- A nullable column with no default: no rewrite, every existing row is
-- unconfirmed (an existing row can only ever be matched by a move to its own
-- status, which is what it asked for). Not re-applied by db.RLSOn (it changes
-- no policy, grant or default); the column inherits the table's row-level
-- security. Idempotent. Must apply after 0079 (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads it, and
-- labels every snapshot move as the upstream's, which is what it did before):
--   ALTER TABLE task_ref_writes DROP COLUMN IF EXISTS confirmed_at;
--   DELETE FROM schema_migrations WHERE version = 80;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.task_ref_writes ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ;
