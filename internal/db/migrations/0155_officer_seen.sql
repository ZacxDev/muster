-- muster 0155 — WHEN EACH PERSON LAST LOOKED AT EACH OFFICER (internal/officers
-- builtin seen.go; docs/officers-builtin.md "An officer's page").
--
--   officer_seen   one row per (viewer, officer): seen_at, the last time the
--       viewer opened that officer's page. The sidebar's Officers group and the
--       Officers list count the officer's items that FINISHED after it (its
--       "What it did" entries the viewer has not seen) as "N new"; opening the
--       officer's page moves it to now. An officer the viewer has never opened
--       counts what finished in the last 7 days (the reader's baseline, not a
--       row here).
--
--   A person's looks go with them (ON DELETE CASCADE): they are nobody
--   else's to keep.
--
--   owner_id is the VIEWER (the person who looked), never the officer's owner:
--   a team's officer is looked at by each member on their own, and one
--   member's look clears nothing for another. officer_owner + officer name the
--   officer (a person, or a team for a team officer). No foreign key to the
--   officer: an officer is a def, a declared row or a MUSTER_OFFICERS entry,
--   and a look at one that is later deleted is harmless (it is read only
--   joined to that officer's items, which go with it).
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, PERSONAL share class (0147's
-- shape: one owner's rows are invisible to every other principal; no team
-- access). rollback/rls_off.sql turns it off with the rest, and --rls-on
-- re-applies this file (db.RLSOn), so every statement is idempotent.
--
-- Must apply after its predecessor, 0154_plugin_autostart (the contiguity
-- test refuses a gap). 🔴 RENUMBER IT (and db.officerSeenMigrationVersion) at merge
-- if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE public.officer_seen;
--   DELETE FROM schema_migrations WHERE version = 155;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_seen (
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id) ON DELETE CASCADE,
    officer_owner BIGINT      NOT NULL CHECK (officer_owner > 0),
    officer       TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    seen_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_id, officer_owner, officer)
);

ALTER TABLE public.officer_seen ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.officer_seen ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.officer_seen FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.officer_seen;
CREATE POLICY owner_isolation ON public.officer_seen
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.officer_seen TO muster_app;
