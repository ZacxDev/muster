-- muster 0146 — THE CHIEF'S STALL CHECKS ARE COUNTED (internal/api
-- chief_nudge.go askChiefForNudge; docs/usage.md; docs/chief-nudges.md).
--
--   chief_turn_usage.trigger   gains 'nudge_check': one stall check of the
--       chief-nudge sweep — the fresh, tool-less, throwaway chief call that
--       decides one stalled session — recorded with the tokens its runtime
--       reported, the chief's model and its duration, like every other
--       trigger. Before this the checks never passed chatTurn and wrote no
--       row, so their cost was invisible on the Usage page.
--
-- The constraint is the column's own CHECK from 0107, named by Postgres
-- (chief_turn_usage_trigger_check); it is dropped and re-added wider. No data
-- changes: every existing row holds a value the wider check still allows.
-- Idempotent; not re-applied by db.RLSOn (no table, no policy).
--
-- Not ADD CONSTRAINT … NOT VALID then VALIDATE: the migrator runs each file
-- in ONE transaction (internal/db migrate.go), and the DROP and ADD already
-- take ACCESS EXCLUSIVE until it commits, so a VALIDATE in the same
-- transaction scans under that same lock and shortens nothing. The scan is
-- bounded by the 3 s lock_timeout below only for the wait, not the scan; the
-- table holds a few numbers per chief turn.
--
-- Must apply after its predecessor, 0145 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT AT MERGE IF MAIN MOVED: 0145 is held by another open change,
-- and a sibling change takes 0147; take the next free number then.
--
-- ROLLBACK. An image from before 0146 never writes 'nudge_check', and reads
-- the trigger only to group it (it shows such rows as "other"), so it runs
-- against this schema unchanged. To restore the narrower check, the rows
-- that use the new value must go first (chief_turn_usage is under FORCED
-- row-level security; run it with muster.scope = 'all', or as the login
-- role):
--   DELETE FROM public.chief_turn_usage WHERE trigger = 'nudge_check';
--   ALTER TABLE public.chief_turn_usage DROP CONSTRAINT IF EXISTS chief_turn_usage_trigger_check;
--   ALTER TABLE public.chief_turn_usage ADD CONSTRAINT chief_turn_usage_trigger_check CHECK (trigger IN ('plugin',
--       'task_event', 'owner_chat', 'view', 'answer', 'session_question', 'hand_back', 'machine', 'kickoff', 'other'));
--   DELETE FROM schema_migrations WHERE version = 146;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chief_turn_usage DROP CONSTRAINT IF EXISTS chief_turn_usage_trigger_check;
ALTER TABLE public.chief_turn_usage ADD CONSTRAINT chief_turn_usage_trigger_check CHECK (trigger IN ('plugin',
    'task_event', 'owner_chat', 'view', 'answer', 'session_question', 'hand_back', 'machine', 'kickoff', 'nudge_check', 'other'));
