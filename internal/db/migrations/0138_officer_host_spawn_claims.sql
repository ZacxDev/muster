-- muster 0138 — A HOST OFFICER INSTANCE THAT NEVER CLAIMS ITS ITEM
-- (officers redesign O-B; internal/officers hostspawns.go, internal/api
-- officer_unclaimed.go).
--
-- A host officer's window can start and then sit at Claude Code's first-run,
-- login or folder-trust screen: the spawn succeeded, the window is listed, and
-- nothing ever claims the item it was woken for. This records, per spawn, what
-- tells that apart, so muster raises ONE Attention notice for it and clears it:
--
--   claimed_at          when the instance this spawn started first claimed an
--                       item (NULL: not yet). Set once, by the claim itself.
--   unclaimed_notice_id the Attention notice raised because it had not claimed
--                       within the window (NULL: none raised). Set once, by a
--                       compare-and-swap from NULL: two replicas never both
--                       raise one, and one spawn never raises two.
--   unclaimed_notice_settled  the notice was withdrawn (the instance claimed,
--                       or the window is gone, or a later spawn replaced it):
--                       never touched again.
--
-- No backfill: every row before this one reads "not claimed, no notice", and
-- the sweep only looks at a spawn younger than a day, so old rows never raise.
--
-- Must apply after 0137 (the contiguity test refuses a gap); 🔴 RENUMBER IT if
-- another migration takes 0138 first.
--
-- ROLLBACK. An older image never reads the columns; roll the image back, then
-- (optionally):
--   ALTER TABLE public.officer_host_spawns DROP COLUMN IF EXISTS claimed_at,
--     DROP COLUMN IF EXISTS unclaimed_notice_id, DROP COLUMN IF EXISTS unclaimed_notice_settled;
--   DELETE FROM schema_migrations WHERE version = 138;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.officer_host_spawns
    ADD COLUMN IF NOT EXISTS claimed_at               TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS unclaimed_notice_id      BIGINT,
    ADD COLUMN IF NOT EXISTS unclaimed_notice_settled BOOLEAN NOT NULL DEFAULT false;

-- The sweep reads each owner's notices still open.
CREATE INDEX IF NOT EXISTS officer_host_spawns_unclaimed_open
    ON public.officer_host_spawns (owner_id) WHERE unclaimed_notice_id IS NOT NULL AND NOT unclaimed_notice_settled;
