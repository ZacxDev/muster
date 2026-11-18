-- muster 0153 — AUTO-START: A PLUGIN WHOSE TASKS START AN AGENT WITH NO TAP
-- (docs/plugins.md#auto-start; internal/plugins autostart.go; internal/api
-- plugin_autostart.go).
--
--   plugins.autostart_target     where an auto-started agent runs:
--       'pod/claude-code' or 'pod/gateway' (agent_dispatch's pod targets).
--   plugins.autostart_repo       the repository it works in, owner/name.
--   plugins.autostart_daily_cap  at most this many auto-starts of this plugin's
--       tasks in any 24 hours (1..10), ON TOP OF the chief's dispatch budget.
--       🔴 10 IS plugins.MaxChiefDispatchesPerDay (the chief's own daily cap,
--       which bounds it anyway) spelled in SQL, and plugins.MaxAutostartDailyCap
--       is defined as it: TestTheAutostartCapCheckIsTheChiefsDailyCap fails if
--       this CHECK and the Go constant diverge.
--   plugins.autostart_at         when the owner turned it on.
--
-- 🔴 ALL FOUR OR NONE (plugins_autostart_whole): a row is either off (every
-- column NULL — the default for every plugin, before and after this
-- migration) or on with a fixed target, a fixed repository and a cap. A task
-- the plugin files chooses none of them.
--
-- 🔴 ONLY THE OWNER'S SIGNED-IN BROWSER WRITES THESE COLUMNS, behind a fresh
-- sign-in (internal/plugins SetAutostart refuses every principal but
-- authz.ViaSession, and internal/api checks the step-up). A plugin's token acts
-- as its owner under row-level security, so the database alone cannot tell the
-- plugin from the owner: the Go store's refusal is the rule, and no
-- /plugin/v1/* route writes them at all.
--
-- No new table or policy: 0015's owner policy and grants on plugins cover the
-- columns, so db.RLSOn re-applies nothing for it. Idempotent.
--
-- Must apply after its predecessor, 0152_page_owner_view (the contiguity test
-- refuses a gap). Numbered 0153 at merge: 0152 is the pages owner view's. 🔴 RENUMBER IT if another migration takes
-- 0153 first.
--
-- ROLLBACK (after rolling the image back; an older image never reads them, so
-- nothing auto-starts once it runs):
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_autostart_whole;
--   ALTER TABLE public.plugins DROP COLUMN IF EXISTS autostart_at,
--     DROP COLUMN IF EXISTS autostart_daily_cap, DROP COLUMN IF EXISTS autostart_repo,
--     DROP COLUMN IF EXISTS autostart_target;
--   DELETE FROM schema_migrations WHERE version = 153;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugins
    ADD COLUMN IF NOT EXISTS autostart_target TEXT
        CHECK (autostart_target IS NULL OR autostart_target IN ('pod/claude-code', 'pod/gateway')),
    ADD COLUMN IF NOT EXISTS autostart_repo TEXT
        CHECK (autostart_repo IS NULL OR autostart_repo ~ '^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$'),
    ADD COLUMN IF NOT EXISTS autostart_daily_cap INTEGER
        CHECK (autostart_daily_cap IS NULL OR autostart_daily_cap BETWEEN 1 AND 10),
    ADD COLUMN IF NOT EXISTS autostart_at TIMESTAMPTZ;

ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_autostart_whole;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_autostart_whole CHECK (
    (autostart_target IS NULL AND autostart_repo IS NULL AND autostart_daily_cap IS NULL AND autostart_at IS NULL)
 OR (autostart_target IS NOT NULL AND autostart_repo IS NOT NULL AND autostart_daily_cap IS NOT NULL AND autostart_at IS NOT NULL));
