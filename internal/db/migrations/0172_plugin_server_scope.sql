-- muster 0172 — SERVER-INSTALLED APP PLUGINS (docs/design/meetings.md, "Core
-- seam: server-installed app plugins"; docs/plugins.md#server-apps). Core PR
-- 1h. The design calls it 0171; 0171 was taken first (the step-up card), so it
-- is 0172.
--
--   plugins.scope        'person' (every row before this one) or 'server': an
--       app an ADMIN installed for the whole server, owned by nobody.
--   plugins.owner_id     now NULL exactly on a server row (plugins_scope_owner).
--       🔴 A NULL owner matches no row-level security arm: 0127's team_read,
--       team_insert, team_update and team_delete each admit owner_id =
--       muster.uid, owner_id = ANY (muster_team_ids(...)) or muster.scope =
--       'all', and a NULL owner_id equals neither of the first two whatever
--       the principal's teams. So every read of a server row is a deliberate
--       authz.System read (internal/plugins server.go, each in the System
--       ledger); a code path that forgets the server arm sees nothing.
--   plugins.installed_by  the admin who installed it from /admin/apps (NULL
--       from the command line); plugins.installed_via 'page' or 'cli'. Set on
--       server rows only (plugins_server_installer). BOOKKEEPING ONLY: no
--       /plugin/v1 handler reads it, and it is never an owner, a principal or
--       a push recipient.
--   plugins_server_caps   a server row holds `app` and at most `tasks` besides,
--       and stores data_scope 'none' (it brings no messages).
--   plugins_server_name_live   one live server app per name.
--
--   plugin_handoffs.server_plugin_id   the server app a code was minted for;
--       plugin_id is then NULL (plugin_handoffs_one_plugin: exactly one of the
--       two). The composite FK (plugin_id, owner_id) is kept: under MATCH
--       SIMPLE a NULL plugin_id skips it, and it still guards every personal
--       code. The code row is OWNED BY THE PERSON who minted it.
--   plugin_handoffs.signed_in_at       the minting session's sign-in time;
--       never NULL on a server code (plugin_handoffs_server_signin).
--
--   plugin_server_users   one row per (server app, person): the pairwise
--       random subject the app knows the person by, the sign-in time of the
--       session whose code was last redeemed (signin_at, copied from the code,
--       NEVER from now(); NULL on a row only Hide made), granted_at (display
--       only, no gate reads it), act_until (the grant's end) and hidden_at
--       (the person hid the app on Account -> Apps).
--   plugin_server_tasks   the tasks a server app filed for a person, by key:
--       a server app's POST /plugin/v1/tasks finds a task ONLY through this
--       ledger. The task FK is the simple one (0053's shape): a composite
--       (task_id, owner_id) onto notes (id, owner_id) would depend on
--       notes_id_owner_key, which 0006's body drops and re-adds whenever it is
--       re-run. The store checks the task's owner at every ledger write.
--
-- Both new tables are OWNED under forced row-level security, in 0015's
-- owner_isolation shape, and are NOT in 0127's team list: a team never owns
-- one, so a team member's principal reads no other member's rows. The redeem,
-- the act-for door and Hide write plugin_server_users under authz.System with
-- an explicit owner_id. Re-applied by db.RLSOn.
--
-- 🔴 GUARDED AGAINST db.RLSOn, which re-applies 0015's, 0104's and 0141's
-- bodies on every RLS cycle (and this one's):
--   - it re-states neither plugins_capabilities_known (0141, 0167, 0168) nor
--     plugins_viewer_link (0104, 0168);
--   - 0104's CREATE TABLE IF NOT EXISTS plugin_handoffs is a no-op on the
--     existing table, so its inline plugin_id NOT NULL and composite FK are
--     never re-added; 0015's likewise for plugins.owner_id NOT NULL;
--   - 0015's re-applied body only SETs DEFAULT owner_id, which a server row's
--     explicit NULL overrides;
--   - each new constraint is added only while absent (0168's guarded form), so
--     a later migration that widens one is not narrowed back.
-- internal/db TestServerAppConstraintsSurviveRLSCycle cycles RLS with a server
-- row, a server code and a grant present.
--
-- Must apply after 0171_attention_step_up_card (the contiguity test refuses a
-- gap). 🔴 RENUMBER IT at merge if main moved. Idempotent.
--
-- ROLLBACK (uninstall every server app FIRST, then roll the image back: an
-- older image reads a NULL owner_id into an int64 and fails):
--   DELETE FROM public.plugin_handoffs WHERE server_plugin_id IS NOT NULL;
--   DROP TABLE IF EXISTS public.plugin_server_tasks;
--   DROP TABLE IF EXISTS public.plugin_server_users;
--   DELETE FROM public.plugins WHERE scope = 'server';
--   ALTER TABLE public.plugin_handoffs DROP CONSTRAINT IF EXISTS plugin_handoffs_one_plugin;
--   ALTER TABLE public.plugin_handoffs DROP CONSTRAINT IF EXISTS plugin_handoffs_server_signin;
--   DROP INDEX IF EXISTS public.plugin_handoffs_server_plugin;
--   ALTER TABLE public.plugin_handoffs DROP COLUMN IF EXISTS server_plugin_id, DROP COLUMN IF EXISTS signed_in_at;
--   ALTER TABLE public.plugin_handoffs ALTER COLUMN plugin_id SET NOT NULL;
--   DROP INDEX IF EXISTS public.plugins_server_name_live;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_scope_owner;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_server_installer;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_server_caps;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_scope_check;
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_installed_via_check;
--   ALTER TABLE public.plugins DROP COLUMN IF EXISTS installed_by, DROP COLUMN IF EXISTS installed_via, DROP COLUMN IF EXISTS scope;
--   ALTER TABLE public.plugins ALTER COLUMN owner_id SET NOT NULL;
--   DELETE FROM schema_migrations WHERE version = 172;
-- (A server code's plugin_id is NULL, so the codes go before plugin_id is
-- NOT NULL again; a server row's owner_id is NULL, so the rows go before
-- owner_id is.)

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'person';
ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS installed_by BIGINT REFERENCES public.users (id);
ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS installed_via TEXT;
ALTER TABLE public.plugins ALTER COLUMN owner_id DROP NOT NULL;

ALTER TABLE public.plugin_handoffs ADD COLUMN IF NOT EXISTS server_plugin_id BIGINT
    REFERENCES public.plugins (id) ON DELETE CASCADE;
ALTER TABLE public.plugin_handoffs ADD COLUMN IF NOT EXISTS signed_in_at TIMESTAMPTZ;
ALTER TABLE public.plugin_handoffs ALTER COLUMN plugin_id DROP NOT NULL;
CREATE INDEX IF NOT EXISTS plugin_handoffs_server_plugin ON public.plugin_handoffs (server_plugin_id);

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugins_scope_check' AND conrelid = 'public.plugins'::regclass) THEN
    ALTER TABLE public.plugins ADD CONSTRAINT plugins_scope_check CHECK (scope IN ('person', 'server'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugins_scope_owner' AND conrelid = 'public.plugins'::regclass) THEN
    ALTER TABLE public.plugins ADD CONSTRAINT plugins_scope_owner CHECK ((scope = 'person') = (owner_id IS NOT NULL));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugins_installed_via_check' AND conrelid = 'public.plugins'::regclass) THEN
    ALTER TABLE public.plugins ADD CONSTRAINT plugins_installed_via_check CHECK (installed_via IS NULL OR installed_via IN ('page', 'cli'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugins_server_installer' AND conrelid = 'public.plugins'::regclass) THEN
    ALTER TABLE public.plugins ADD CONSTRAINT plugins_server_installer
        CHECK ((scope = 'server') = (installed_via IS NOT NULL)
           AND (installed_by IS NULL OR scope = 'server'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugins_server_caps' AND conrelid = 'public.plugins'::regclass) THEN
    ALTER TABLE public.plugins ADD CONSTRAINT plugins_server_caps
        CHECK (scope <> 'server' OR (capabilities <@ ARRAY['app', 'tasks']::text[]
                                    AND 'app' = ANY (capabilities) AND data_scope = 'none'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugin_handoffs_one_plugin' AND conrelid = 'public.plugin_handoffs'::regclass) THEN
    ALTER TABLE public.plugin_handoffs ADD CONSTRAINT plugin_handoffs_one_plugin
        CHECK ((plugin_id IS NULL) <> (server_plugin_id IS NULL));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'plugin_handoffs_server_signin' AND conrelid = 'public.plugin_handoffs'::regclass) THEN
    ALTER TABLE public.plugin_handoffs ADD CONSTRAINT plugin_handoffs_server_signin
        CHECK (server_plugin_id IS NULL OR signed_in_at IS NOT NULL);
  END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS plugins_server_name_live
    ON public.plugins (name) WHERE scope = 'server' AND revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS public.plugin_server_users (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    plugin_id   BIGINT      NOT NULL REFERENCES public.plugins (id) ON DELETE CASCADE,
    owner_id    BIGINT      NOT NULL
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id) ON DELETE CASCADE,
    subject     TEXT        NOT NULL CHECK (subject ~ '^[A-Za-z0-9_-]{22}$'),
    signin_at   TIMESTAMPTZ,
    granted_at  TIMESTAMPTZ,
    act_until   TIMESTAMPTZ NOT NULL,
    hidden_at   TIMESTAMPTZ,
    CONSTRAINT plugin_server_users_person UNIQUE (plugin_id, owner_id),
    CONSTRAINT plugin_server_users_subject UNIQUE (plugin_id, subject)
);

CREATE TABLE IF NOT EXISTS public.plugin_server_tasks (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    plugin_id   BIGINT      NOT NULL REFERENCES public.plugins (id) ON DELETE CASCADE,
    owner_id    BIGINT      NOT NULL
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id) ON DELETE CASCADE,
    key         TEXT        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 200),
    task_id     BIGINT      NOT NULL REFERENCES public.notes (id) ON DELETE CASCADE,
    CONSTRAINT plugin_server_tasks_key UNIQUE (plugin_id, owner_id, key)
);

-- Stated outside CREATE TABLE so a re-applied body (db.RLSOn, after the
-- rls_off rollback set them to 1) restores the principal default.
ALTER TABLE public.plugin_server_users ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.plugin_server_tasks ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['plugin_server_users', 'plugin_server_tasks'] LOOP
        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', t);
        EXECUTE format($p$
            CREATE POLICY owner_isolation ON public.%I
                AS PERMISSIVE FOR ALL TO PUBLIC
                USING (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )
                WITH CHECK (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )
        $p$, t);
    END LOOP;
END
$$;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.plugin_server_users TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.plugin_server_tasks TO muster_app;
