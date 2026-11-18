-- muster 0117 — "START NOTIFICATION TITLES WITH THE TMUX NAME", PER USER
-- (internal/notifypref; docs/focus.md#tmux-name-first-in-push-titles).
--
--   notify_preferences.codename_first   the user's choice that a PUSH title
--       naming a mirrored session starts with the window's tmux codename
--       ("mango:1 · Billing migration needs you"), so a phone that cuts the
--       END of a title still shows which window it is. It is independent of
--       tmux_address (0094): either, both or neither may be on.
--
-- 🔴 OFF FOR EVERY USER, NEW AND EXISTING, AND NOTHING IS BACKFILLED. The
-- column is NOT NULL DEFAULT false, so every existing row (a user who turned
-- "Show tmux sessions" on) reads off here, and a user with no row still reads
-- off (notifypref.PG.CodenameFirst): every push title is exactly what it was.
--
-- A new column on 0094's owned table: its forced owner_isolation policy
-- covers it, and rollback/rls_off.sql + --rls-on re-apply 0094 unchanged.
-- Must apply after 0116 (the contiguity test refuses a gap); the number was
-- the next free one on main when written and may need renumbering at merge.
-- Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads the column, and titles every push as this preference off does):
--   ALTER TABLE public.notify_preferences DROP COLUMN IF EXISTS codename_first;
--   DELETE FROM schema_migrations WHERE version = 117;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.notify_preferences
    ADD COLUMN IF NOT EXISTS codename_first BOOLEAN NOT NULL DEFAULT false;
