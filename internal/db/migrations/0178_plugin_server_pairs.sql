-- muster 0178 — LINKED SERVER APPS (docs/design/contacts.md, "The core change:
-- linked server apps"; docs/plugins.md#linked-server-apps).
--
--   plugin_server_pairs   a LINKED PAIR of server apps, as TWO rows, one per
--       app: (plugin_id, partner_id) and (partner_id, plugin_id). The two
--       apps know each person by ONE subject (internal/plugins subjectFor:
--       when either app first needs a subject for a person, it takes the one
--       that person already has under the partner, and draws a new random one
--       only when there is none). A pair is made only at install, `muster-
--       server app install … --link-to <id>`, in the install's own
--       transaction, so the new app knows nobody when it is linked; it ends
--       with `muster-server app unlink <id>` or an uninstall of either app.
--
-- 🔴 EACH APP IS IN AT MOST ONE PAIR, BY CONSTRAINTS ALONE:
--   - the PRIMARY KEY (plugin_id): an app has at most one row;
--   - plugin_server_pairs_mutual, a foreign key (partner_id, plugin_id) onto
--     (plugin_id, partner_id): every row's mirror exists. So if C -> B were
--     added beside B -> A, its mirror B -> C would be a second row for B,
--     which the key refuses. ON DELETE CASCADE: deleting either row deletes
--     its mirror, so one DELETE ends a pair;
--   - plugin_server_pairs_not_self: an app is never its own partner.
-- The two rows are written by one INSERT, at whose end the mutual key is
-- checked.
--
-- 🔴 NOTHING HERE REWRITES A SUBJECT. plugin_server_users is not changed: its
-- UNIQUE (plugin_id, subject) still holds, a shared subject appearing once
-- under each of the two apps.
--
-- 🔴 NOT OWNED: no owner_id, like a server plugins row (0172). Row-level
-- security is enabled and forced with one policy, scope_all (the system scope
-- only, 0174's shape), so a person's principal reads and writes nothing; every
-- statement on it in internal/plugins is an explicit authz.System one. Neither
-- --rls-off nor db.RLSOn touches it.
--
-- The foreign keys onto plugins cascade only on a DELETE of a plugins row,
-- which nothing does: an uninstall is an UPDATE (revoked_at), so
-- UninstallServerApp deletes the pair itself, in its own transaction.
--
-- Must apply after its predecessor, 0177_session_wakes (the contiguity test
-- refuses a gap). 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads it). It
-- leaves every subject as it is: subjects already shared stay shared, and
-- people who first open either app afterwards get independent ones.
-- ⚠ An image rolled back WITHOUT the DROP below leaves the table in place,
-- and an older image's uninstall does not know it: the uninstalled app's
-- pair row (and its mirror) stays, so the survivor still reads as linked
-- once the newer image is back. Clear it with `muster-server app unlink
-- <the survivor's id>`.
--   DROP TABLE IF EXISTS public.plugin_server_pairs;
--   DELETE FROM schema_migrations WHERE version = 178;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.plugin_server_pairs (
    plugin_id   BIGINT      NOT NULL PRIMARY KEY REFERENCES public.plugins (id) ON DELETE CASCADE,
    partner_id  BIGINT      NOT NULL REFERENCES public.plugins (id) ON DELETE CASCADE,
    linked_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT plugin_server_pairs_not_self CHECK (plugin_id <> partner_id),
    CONSTRAINT plugin_server_pairs_both UNIQUE (plugin_id, partner_id),
    CONSTRAINT plugin_server_pairs_mutual FOREIGN KEY (partner_id, plugin_id)
        REFERENCES public.plugin_server_pairs (plugin_id, partner_id) ON DELETE CASCADE
);

ALTER TABLE public.plugin_server_pairs ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.plugin_server_pairs FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS scope_all ON public.plugin_server_pairs;
CREATE POLICY scope_all ON public.plugin_server_pairs AS PERMISSIVE FOR ALL TO PUBLIC
    USING (current_setting('muster.scope', true) = 'all')
    WITH CHECK (current_setting('muster.scope', true) = 'all');

GRANT SELECT, INSERT, DELETE ON public.plugin_server_pairs TO muster_app;
