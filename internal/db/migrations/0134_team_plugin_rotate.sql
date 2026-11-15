-- muster 0134 — A TEAM VIEWER PLUGIN MARKED ROTATE (docs/design/team-sharing.md,
-- F "2b.3b"; #696 round 0, (b)).
--
--   plugins.rotate_at              (used by 2b.3b)
--       Set, as mirror_tokens.rotate_at is (0133), when an operator or a
--       manager of the plugin's TEAM leaves it, drops to viewer or is
--       disabled — in the same transaction (internal/users team_hosts.go).
--       A team viewer plugin's token was shown to a manager once, and a
--       person keeps a copy after they leave: mark it, never auto-revoke
--       (D-1113 #6). Nothing clears it: a plugin marked rotate stays marked
--       until it is revoked. A person's own plugin is never marked.
--
-- Inert without its code: the column is nullable and nothing older reads it.
-- Must apply after its predecessor, 0133 (the contiguity test refuses a gap).
--
-- ROLLBACK, in this order:
--   1. BEFORE the image rollback, revoke every team's plugin rows. An image
--      older than 2b.3b has no owner filter on its personal plugin paths, so
--      a team's rows (which a member's session reads and a manager's writes)
--      would show on members' own /plugins pages and as chips of their own
--      windows, and a manager could revoke them, toggle draft_checks or bind
--      them to an officer there, unaudited:
--        UPDATE public.plugins SET revoked_at = now()
--         WHERE revoked_at IS NULL
--           AND owner_id IN (SELECT id FROM public.users WHERE kind = 'team');
--   2. Roll the image back.
--   3. Then (an older image never reads the column):
--   ALTER TABLE public.plugins DROP COLUMN IF EXISTS rotate_at;
--   DELETE FROM schema_migrations WHERE version = 134;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS rotate_at TIMESTAMPTZ;
