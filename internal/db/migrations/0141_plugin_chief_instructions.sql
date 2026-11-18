-- muster 0141 — A PLUGIN'S INSTRUCTION SECTION FOR ITS OWNER'S CHIEF
-- (docs/plugins.md#chief-instructions; internal/plugins chief_instructions.go).
--
--   plugins.capabilities 'chief_extension'   the plugin may store ONE markdown
--       section for its owner's chief (PUT /plugin/v1/chief/instructions).
--       Opt-in at mint, and toggled on the plugin's /plugins card, as
--       draft_checks is. A token without it gets 403 on the route.
--
--   plugin_chief_instructions   one row per plugin (plugin_chief_instructions_one_per_plugin):
--       text          the section as the plugin sent it, 1..4096 bytes.
--       sha           its sha256, hex — pinned to the text by
--                     plugin_chief_instructions_sha_is_text, so no writer can
--                     store a hash that names other words.
--       approved_sha  the sha the owner approved on /plugins, or NULL (pending).
--                     🔴 IT IS EITHER NULL OR THIS ROW'S OWN sha
--                     (plugin_chief_instructions_approved_is_current): a plugin
--                     that changes its text clears the approval in the same
--                     write, so a change is pending again, and text A → B → A
--                     is not approved again without the owner.
--       approved_at   when the owner approved it; NULL exactly when approved_sha is.
--       updated_at    the last time the plugin stored text.
--   The composite foreign key onto plugins (plugin_id, owner_id) means a row
--   cannot name another owner's plugin; ON DELETE CASCADE goes with the plugin
--   row. A plugin's REVOKE deletes its row in the revoke's transaction
--   (internal/plugins Revoke), and turning chief_extension off deletes it too.
--
-- Only an approved section of a live plugin that still has chief_extension is
-- rendered into its owner's chief's CLAUDE.md, at that chief's next provision
-- or restart (internal/agentprovision). It grants the chief nothing: the tool
-- list and allow list are unchanged.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, PERSONAL (owner_isolation only,
-- no team policy): the plugin's token acts as its owner (authz.ViaPlugin), the
-- owner's /plugins session approves, and nobody else — a team member included
-- — reads or writes the row. Re-applied by db.RLSOn.
--
-- Must apply after 0140 (the contiguity test refuses a gap); 🔴 RENUMBER IT if
-- another migration takes 0141 first (and db/rollback.go's
-- pluginChiefInstructionsMigrationVersion with it). Idempotent.
--
-- ROLLBACK (roll the image back first; an older image reads neither the
-- capability nor the table, and renders no section):
--   DROP TABLE IF EXISTS public.plugin_chief_instructions;
--   UPDATE public.plugins SET revoked_at = now() WHERE capabilities = '{chief_extension}' AND revoked_at IS NULL;
--   UPDATE public.plugins SET capabilities = '{messages}' WHERE capabilities = '{chief_extension}';
--   UPDATE public.plugins SET capabilities = array_remove(capabilities, 'chief_extension')
--    WHERE 'chief_extension' = ANY (capabilities);
--   ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
--   ALTER TABLE public.plugins ADD CONSTRAINT plugins_capabilities_known
--     CHECK (capabilities <@ ARRAY['messages', 'viewer', 'draft_checks']::text[]);
--   DELETE FROM schema_migrations WHERE version = 141;
-- (A plugin whose only capability was chief_extension cannot keep an empty
-- set, plugins_capabilities_nonempty, nor a capability the narrowed
-- constraint refuses: it is revoked first, and its revoked row then names
-- messages, which a revoked token never opens.)

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_capabilities_known
    CHECK (capabilities <@ ARRAY['messages', 'viewer', 'draft_checks', 'chief_extension']::text[]);

CREATE TABLE IF NOT EXISTS public.plugin_chief_instructions (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    plugin_id    BIGINT      NOT NULL,
    text         TEXT        NOT NULL CHECK (octet_length(text) BETWEEN 1 AND 4096),
    sha          TEXT        NOT NULL CHECK (sha ~ '^[0-9a-f]{64}$'),
    approved_sha TEXT,
    approved_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT plugin_chief_instructions_one_per_plugin UNIQUE (plugin_id),
    CONSTRAINT plugin_chief_instructions_plugin_owner_fkey FOREIGN KEY (plugin_id, owner_id)
        REFERENCES public.plugins (id, owner_id) ON DELETE CASCADE,
    CONSTRAINT plugin_chief_instructions_sha_is_text
        CHECK (sha = encode(sha256(convert_to(text, 'UTF8')), 'hex')),
    CONSTRAINT plugin_chief_instructions_approved_is_current
        CHECK (approved_sha IS NULL OR approved_sha = sha),
    CONSTRAINT plugin_chief_instructions_approval_has_time
        CHECK ((approved_sha IS NULL) = (approved_at IS NULL))
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.plugin_chief_instructions ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.plugin_chief_instructions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.plugin_chief_instructions FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.plugin_chief_instructions;
CREATE POLICY owner_isolation ON public.plugin_chief_instructions
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.plugin_chief_instructions TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.plugin_chief_instructions_id_seq TO muster_app;
