-- muster 0121 — WHEN AN EPISODE IS 'gone', AND WHAT ITS RETURN KEEPS
-- (internal/sessionstate; docs/design/session-state.md, "gone").
--
--   session_episodes.missed_passes   how many CONSECUTIVE state passes found
--       the episode's session missing: its window absent from a host that is
--       online, reporting status and listing windows (or its agent absent
--       from an agent list that was read). The pass ends the episode 'gone'
--       only once it reaches episodeGoneMisses (internal/api/session_state.go),
--       so a listing that leaves a window out for one pass ends nothing. Any
--       pass that finds the session — listed with its key (Observe) or
--       without it (Present) — resets it to 0. A pass whose host is offline
--       or unread changes nothing. Persisted, not in memory, so a restart
--       mid-count neither loses it nor starts it over.
--
--   session_episodes.resumed_from    for an episode opened after the
--       session's latest one ended 'gone' on the SAME streak (the same stop
--       back: sessionstate.resumedReasons), that stop's FIRST episode — the
--       ended one's own resumed_from, else its id; NULL otherwise. The
--       chief's decision taken in that episode is this one's (streakDecision),
--       as its push is (pushed_at, carried in the same INSERT). Not a foreign
--       key: it is compared, never joined, and outlives nothing it needs.
--
-- Must apply after 0120 (the contiguity test refuses a gap); 🔴 RENUMBER IT
-- if another migration takes 0121 first. Not re-applied by db.RLSOn (it
-- changes no policy, grant or default); the columns inherit the table's
-- row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0121 never reads or writes the columns (it
-- ends an episode 'gone' on the first pass that misses it, as before) and can
-- run against this schema unchanged. To remove them:
--   ALTER TABLE session_episodes DROP COLUMN IF EXISTS missed_passes;
--   ALTER TABLE session_episodes DROP COLUMN IF EXISTS resumed_from;
--   DELETE FROM schema_migrations WHERE version = 121;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_episodes ADD COLUMN IF NOT EXISTS missed_passes INTEGER NOT NULL DEFAULT 0
    CONSTRAINT session_episodes_missed_passes CHECK (missed_passes >= 0);

ALTER TABLE public.session_episodes ADD COLUMN IF NOT EXISTS resumed_from BIGINT;
