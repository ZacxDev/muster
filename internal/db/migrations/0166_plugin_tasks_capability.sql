-- muster 0166 — THE `tasks` PLUGIN CAPABILITY (docs/plugins.md "Capabilities";
-- docs/design/meetings.md "Core seam: the tasks capability", decision 11).
--
--   plugins.capabilities 'tasks'   the plugin FILES TASKS and nothing else: it
--       opens POST /plugin/v1/tasks (filing and commenting by key). `messages`
--       still opens that route, so every existing token is unchanged and no
--       row is migrated. A token with `tasks` and not `messages` is refused
--       on every other /plugin/v1 route but GET /plugin/v1/self (internal/api
--       requirePluginToken), and may not route a notice to the chief nor
--       upsert a task reference (internal/api plugin_tasks.go).
--   plugin_pair_requests.capabilities  `tasks` is PAIRABLE, as `messages` is:
--       a request may name messages, draft_checks and tasks, and must name
--       messages or tasks (draft_checks still only beside messages: the
--       application's rule, internal/plugins validCapabilities).
--
-- 🔴 A LATER CAPABILITY MIGRATION MUST LIST EVERY CAPABILITY, NOT JUST ITS OWN:
-- it replaces these CHECKs whole, so a list missing 'tasks' silently refuses
-- every tasks token. internal/db TestPluginCapabilityChecksMatchGo pins the
-- live CHECKs to internal/plugins KnownCapabilities and PairableCapabilities,
-- and each migration's ROLLBACK list to the previous migration's.
--
-- Must apply after 0165 (the contiguity test refuses a gap); 🔴 RENUMBER IT if
-- another migration takes 0166 first. Idempotent. 0141's body (which db.RLSOn
-- re-applies) is guarded so that it no longer narrows the CHECK this sets.
--
-- ROLLBACK (roll the image back first; an older image does not know the
-- capability):
--   UPDATE public.plugins SET revoked_at = now() WHERE capabilities = '{tasks}' AND revoked_at IS NULL;
--   UPDATE public.plugins SET capabilities = '{messages}' WHERE capabilities = '{tasks}';
--   UPDATE public.plugins SET capabilities = array_remove(capabilities, 'tasks')
--    WHERE 'tasks' = ANY (capabilities);
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
--   ALTER TABLE public.plugins ADD CONSTRAINT plugins_capabilities_known
--     CHECK (capabilities <@ ARRAY['messages', 'viewer', 'draft_checks', 'chief_extension']::text[]);
--   DELETE FROM public.plugin_pair_requests WHERE NOT ('messages' = ANY (capabilities));
--   UPDATE public.plugin_pair_requests SET capabilities = array_remove(capabilities, 'tasks')
--    WHERE 'tasks' = ANY (capabilities);
--   ALTER TABLE public.plugin_pair_requests DROP CONSTRAINT IF EXISTS plugin_pair_requests_capabilities_check;
--   ALTER TABLE public.plugin_pair_requests ADD CONSTRAINT plugin_pair_requests_capabilities_check
--     CHECK (capabilities <@ ARRAY['messages', 'draft_checks']::text[] AND 'messages' = ANY (capabilities));
--   DELETE FROM schema_migrations WHERE version = 166;
-- (A plugin whose only capability was tasks cannot keep an empty set,
-- plugins_capabilities_nonempty: it is revoked first, and its revoked row then
-- names messages, which a revoked token never opens.)

SET LOCAL lock_timeout = '3s';

-- Each CHECK is replaced only while the live one does not yet admit 'tasks',
-- so a re-application (or a later, wider list) is left alone — as 0141 is
-- guarded, and as a later capability migration must guard its own.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugins_capabilities_known'
                   AND conrelid = 'public.plugins'::regclass AND pg_get_constraintdef(oid) LIKE '%''tasks''%') THEN
    ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
    ALTER TABLE public.plugins ADD CONSTRAINT plugins_capabilities_known
        CHECK (capabilities <@ ARRAY['messages', 'viewer', 'draft_checks', 'chief_extension', 'tasks']::text[]);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugin_pair_requests_capabilities_check'
                   AND conrelid = 'public.plugin_pair_requests'::regclass AND pg_get_constraintdef(oid) LIKE '%''tasks''%') THEN
    ALTER TABLE public.plugin_pair_requests DROP CONSTRAINT IF EXISTS plugin_pair_requests_capabilities_check;
    ALTER TABLE public.plugin_pair_requests ADD CONSTRAINT plugin_pair_requests_capabilities_check
        CHECK (capabilities <@ ARRAY['messages', 'draft_checks', 'tasks']::text[]
               AND ('messages' = ANY (capabilities) OR 'tasks' = ANY (capabilities)));
  END IF;
END $$;
