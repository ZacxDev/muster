-- muster 0094 — NOTIFICATION PREFERENCES, PER USER (internal/notifypref).
--
-- One row per owner who has flipped a notification preference on the Account
-- page. Today it holds one: `tmux_address`, "Show tmux session in
-- notifications" — a notification that names a mirrored session by its human
-- name (internal/sessionname) adds the window's codename (its tmux address
-- when its host sent none) and host after it ("Billing migration · mango:1 @
-- atlas"); see docs/focus.md.
--
-- 🔴 OFF FOR EVERY USER, NEW AND EXISTING, AND NOTHING IS BACKFILLED. No row
-- reads as off (notifypref.PG.TmuxAddress), and the column's own default is
-- false, so a user who never touched the switch gets exactly the notification
-- text they had before this migration.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0060's chief_preferences
-- shape: owner_isolation keeps one user from reading or flipping another's
-- preference. rollback/rls_off.sql turns it off with the rest, and --rls-on
-- re-applies this file (db.RLSOn), so every statement is idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads the table, and names every notification as this preference off does):
--   DROP TABLE IF EXISTS public.notify_preferences;
--   DELETE FROM schema_migrations WHERE version = 94;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.notify_preferences (
    owner_id      BIGINT      PRIMARY KEY
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    tmux_address  BOOLEAN     NOT NULL DEFAULT false,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.notify_preferences ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.notify_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.notify_preferences FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.notify_preferences;
CREATE POLICY owner_isolation ON public.notify_preferences
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.notify_preferences TO muster_app;
