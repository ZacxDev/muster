-- muster 0153 — A PLUGIN'S TASK FILED BEFORE 0083 IS STAMPED AS ONE
-- (internal/api task_kickoff.go; notes.Note.PluginOrigin; docs/plugins.md,
-- "muster fences your task's text").
--
-- muster fences a plugin's task text wherever it hands it to an agent, and
-- knows a copy task is a plugin's by notes.created_by = 'plugin:<label>'
-- (0083). Every task POST /plugin/v1/tasks filed BEFORE 0083 has created_by
-- NULL, so it read as the owner's and was handed over unfenced. This stamps
-- each one that can be attributed:
--
--   created_by IS NULL, ext_key IS NULL (a reference is fenced by ext_key
--   already), the task carries a once-key tag — a namespaced tag whose
--   namespace is not one muster reserves (gate, auto, initiative, project,
--   superseded-by): the key a plugin files and finds its task by
--   (notes.CreateOnceByTag) — AND one of the OWNER's plugins left its mark on
--   that task: a comment written as that plugin (author 'plugin:<name>', the
--   relay comment POST /plugin/v1/tasks writes) or a task event it raised
--   (plugin_messages.task_id). The stamp is 'plugin:' || that plugin's label,
--   what the create writes today; two such plugins: the lowest id.
--
-- 🔴 WHAT IT CANNOT SEE: a pre-0083 task a plugin filed with no comment and no
-- routed notice left no trace of the plugin in the database (the namespaces
-- are deployment configuration, not rows), so it stays unattributed and
-- unfenced (docs/limits.md). And the rule leans toward fencing: an owner's
-- own task that carries a key-shaped tag a plugin commented on is stamped too
-- — the plugin's text is in its thread either way.
--
-- REVERSIBLE: every stamped row is recorded in muster_backfill.notes_created_by_0153
-- (a schema of its own, so no application grant, row-level-security ledger or
-- default privilege reaches it), and only rows still holding the value
-- stamped are put back.
--
-- Idempotent (a re-run stamps nothing: every candidate's created_by is no
-- longer NULL). Not re-applied by db.RLSOn (no policy, grant or default on
-- public changes). Must apply after 0152_page_owner_view (the contiguity test
-- refuses a gap).
--
-- An older image reads the stamp as the plugin creator a notification names,
-- which is what it is, so the rollback needs no image rollback first.
-- ROLLBACK:
--   UPDATE public.notes n SET created_by = NULL
--     FROM muster_backfill.notes_created_by_0153 b
--    WHERE n.id = b.note_id AND n.created_by = b.created_by;
--   DROP TABLE IF EXISTS muster_backfill.notes_created_by_0153;
--   DROP SCHEMA IF EXISTS muster_backfill;   -- refuses while anything else is in it
--   DELETE FROM schema_migrations WHERE version = 153;

SET LOCAL lock_timeout = '3s';

CREATE SCHEMA IF NOT EXISTS muster_backfill;
REVOKE ALL ON SCHEMA muster_backfill FROM PUBLIC;

CREATE TABLE IF NOT EXISTS muster_backfill.notes_created_by_0153 (
    note_id    BIGINT PRIMARY KEY,
    created_by TEXT   NOT NULL
);

WITH stamped AS (
    SELECT DISTINCT ON (n.id) n.id, 'plugin:' || p.label AS created_by
    FROM public.notes n
    JOIN public.plugins p ON p.owner_id = n.owner_id
    WHERE n.created_by IS NULL
      AND n.ext_key IS NULL
      AND EXISTS (
          SELECT 1 FROM unnest(n.tags) AS tag
          WHERE position(':' IN tag) > 1
            AND split_part(tag, ':', 1) NOT IN ('gate', 'auto', 'initiative', 'project', 'superseded-by'))
      AND (EXISTS (SELECT 1 FROM public.note_comments c
                   WHERE c.note_id = n.id AND c.author = 'plugin:' || p.name)
        OR EXISTS (SELECT 1 FROM public.plugin_messages m
                   WHERE m.task_id = n.id AND m.plugin_id = p.id))
    ORDER BY n.id, p.id
), recorded AS (
    INSERT INTO muster_backfill.notes_created_by_0153 (note_id, created_by)
    SELECT id, created_by FROM stamped
    ON CONFLICT (note_id) DO NOTHING
    RETURNING note_id, created_by
)
UPDATE public.notes n SET created_by = r.created_by
FROM recorded r
WHERE n.id = r.note_id AND n.created_by IS NULL;
