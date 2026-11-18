-- muster 0102 — WHAT A PLUGIN TOKEN MAY OPEN (docs/design/previews.md,
-- "Capabilities on a plugin token"): plugins.capabilities, a set.
--
--   messages   every /plugin/v1/* route a plugin token opened before this
--              migration (inbound messages, drafts, tasks, task writes).
--   viewer     reserved here for the viewer seams (the session-view chip and
--              hand-off codes); no route takes it yet.
--
-- 🔴 EVERY EXISTING TOKEN KEEPS WHAT IT HAD. The column's DEFAULT is
-- '{messages}', and ADD COLUMN with a constant default fills every existing
-- row with it, so a token minted before this migration still reaches every
-- route it reached (TestMigration0102KeepsEveryTokenItsRoutes).
--
-- The CHECKs: only known capabilities (<@), and at least one, so a token that
-- opens nothing cannot be minted. A route checks its capability in
-- requirePluginToken (internal/api/plugins.go); a capability no route names
-- opens nothing.
--
-- A new column on an owned table: the table's row-level security covers it;
-- not re-applied by db.RLSOn. Idempotent.
--
-- ROLLBACK. 🔴 An older image ignores the column, so it would admit ANY live
-- token on every /plugin/v1/* route, a viewer-only token included. Revoke every
-- token without 'messages' FIRST, then roll the image back, then:
--   UPDATE public.plugins SET revoked_at = now()
--    WHERE revoked_at IS NULL AND NOT ('messages' = ANY (capabilities));
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_nonempty;
--   ALTER TABLE public.plugins DROP COLUMN IF EXISTS capabilities;
--   DELETE FROM schema_migrations WHERE version = 102;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS capabilities TEXT[] NOT NULL DEFAULT '{messages}';

ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_capabilities_known
    CHECK (capabilities <@ ARRAY['messages', 'viewer']::text[]);

ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_nonempty;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_capabilities_nonempty
    CHECK (cardinality(capabilities) >= 1);
