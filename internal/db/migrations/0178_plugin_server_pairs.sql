-- muster 0178 — LINKED SERVER APPS (docs/design/contacts.md, "The core change:
-- linked server apps"; docs/plugins.md#linked-server-apps).
--
--   plugin_server_pairs   one row per LINKED PAIR of server apps: the two
--       apps know each person by ONE subject (internal/plugins subjectFor:
--       when either app of a pair needs a subject for a person, it takes the
--       one that person already has under the partner, and draws a new random
--       one only when there is none). An admin links two apps with
--       `muster-server app link <source> <adopter>` and ends the pair with
--       `muster-server app unlink <id>`; an uninstall of either ends it too.
--
--     low_id, high_id   the two apps, the lower id first, so a pair has one
--                       spelling (plugin_server_pairs_order).
--     source_id         the app whose subjects were kept when the pair was
--                       made (one of the two: plugin_server_pairs_source).
--                       BOOKKEEPING ONLY, shown by `app list`: no gate reads it.
--     linked_at         when.
--
-- 🔴 EACH APP IS IN AT MOST ONE PAIR. plugin_server_pairs_low and
-- plugin_server_pairs_high refuse an id twice in one column;
-- plugin_server_pairs_one_pair (a trigger) refuses an id that already appears
-- in the OTHER column of another row. Links are serialised by the server-app
-- advisory lock (internal/plugins LinkServerApps), so the trigger's read cannot
-- race another link.
--
-- 🔴 NOTHING HERE REWRITES A SUBJECT. plugin_server_users is not changed: its
-- UNIQUE (plugin_id, subject) still holds, a shared subject appearing once
-- under each of the two apps. The store refuses a link while the adopter has
-- any plugin_server_users row, so no person's data is ever moved under another
-- subject.
--
-- 🔴 NOT OWNED: no owner_id, like a server plugins row (0172). Row-level
-- security is enabled and forced with one policy, scope_all (the system scope
-- only, 0174's shape), so a person's principal reads and writes nothing; every
-- statement on it in internal/plugins is an explicit authz.System one. Neither
-- --rls-off nor db.RLSOn touches it.
--
-- The foreign keys cascade only on a DELETE of a plugins row, which nothing
-- does: an uninstall is an UPDATE (revoked_at), so UninstallServerApp deletes
-- the pair row itself, in its own transaction.
--
-- Must apply after its predecessor, 0177_session_wakes (the contiguity test
-- refuses a gap). 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads it). It
-- leaves every subject as it is: subjects already shared stay shared, and
-- people who first open either app afterwards get independent ones.
--   DROP TABLE IF EXISTS public.plugin_server_pairs;
--   DROP FUNCTION IF EXISTS public.plugin_server_pairs_one_pair();
--   DELETE FROM schema_migrations WHERE version = 178;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.plugin_server_pairs (
    low_id     BIGINT      NOT NULL REFERENCES public.plugins (id) ON DELETE CASCADE,
    high_id    BIGINT      NOT NULL REFERENCES public.plugins (id) ON DELETE CASCADE,
    source_id  BIGINT      NOT NULL,
    linked_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (low_id, high_id),
    CONSTRAINT plugin_server_pairs_order  CHECK (low_id < high_id),
    CONSTRAINT plugin_server_pairs_source CHECK (source_id IN (low_id, high_id))
);
CREATE UNIQUE INDEX IF NOT EXISTS plugin_server_pairs_low  ON public.plugin_server_pairs (low_id);
CREATE UNIQUE INDEX IF NOT EXISTS plugin_server_pairs_high ON public.plugin_server_pairs (high_id);

-- An id already in the OTHER column of another row: the same 23505 a unique
-- index raises, naming plugin_server_pairs_one_pair.
CREATE OR REPLACE FUNCTION public.plugin_server_pairs_one_pair() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.plugin_server_pairs p
                WHERE (p.low_id, p.high_id) <> (NEW.low_id, NEW.high_id)
                  AND (p.high_id = NEW.low_id OR p.low_id = NEW.high_id)) THEN
        RAISE EXCEPTION 'plugin_server_pairs_one_pair: an app is in at most one linked pair'
            USING ERRCODE = '23505', CONSTRAINT = 'plugin_server_pairs_one_pair';
    END IF;
    RETURN NEW;
END
$$;
DROP TRIGGER IF EXISTS plugin_server_pairs_one_pair ON public.plugin_server_pairs;
CREATE TRIGGER plugin_server_pairs_one_pair BEFORE INSERT OR UPDATE ON public.plugin_server_pairs
    FOR EACH ROW EXECUTE FUNCTION public.plugin_server_pairs_one_pair();

ALTER TABLE public.plugin_server_pairs ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.plugin_server_pairs FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS scope_all ON public.plugin_server_pairs;
CREATE POLICY scope_all ON public.plugin_server_pairs AS PERMISSIVE FOR ALL TO PUBLIC
    USING (current_setting('muster.scope', true) = 'all')
    WITH CHECK (current_setting('muster.scope', true) = 'all');

GRANT SELECT, INSERT, DELETE ON public.plugin_server_pairs TO muster_app;
