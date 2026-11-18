-- muster 0079 — AN EPISODE ENDS WHEN ITS OWNER HANDLES IT, AND THE BACKLOG IS
-- ENDED ONCE (internal/sessionstate; docs/design/session-state.md, "Episodes").
--
--   session_episodes.end_reason   gains three reasons:
--     'owner'   the owner acted in the session after the episode opened — a
--               message of their own written after it opened, or, for a
--               waiting episode, the prompt it waited on answered
--               (sessionstate.OwnerActed). It ends on the next state pass.
--     'viewed'  the owner opened the session's own page in muster after the
--               episode opened — its rendered view, its terminal, an agent's
--               conversation (internal/api episodeViewed). It ends at once.
--     'reset'   written by THIS migration (below), and by muster only to end
--               again a stop an older image re-opened on the same streak
--               mid-rollout (sessionstate.PG.EndRepeat).
--   Each withdraws the episode's push (the restart rule: clearEndedEpisodes on
--   the next pass) without waiting for EndAfter of sustained work ('worked'),
--   which stays the rule for a session that carries on by itself. 'viewed'
--   and 'reset' are HANDLED (sessionstate.Handled): the session is not opened
--   a new episode on the streak its handled one ended on. 'owner' is not: the
--   owner answered, so the session moved, and its next stop is a new one.
--
--   THE BACKLOG RESET. Every episode open when this applies is ended 'reset',
--   once. Those stops opened while an episode ended only after sustained work,
--   so many are stops the owner already dealt with; the next state pass
--   withdraws each one's push and drops its Stalled / waiting row, and a
--   session still stopped on the same streak is not opened again (Handled).
--   A session that moves afterwards opens a new episode as usual.
--
-- The constraint is the column's own CHECK from 0073, named by Postgres
-- (session_episodes_end_reason_check); it is dropped and re-added wider.
-- Every existing row ('', 'worked', 'gone') passes the new check.
--
-- THE ROLLING UPDATE. The image before this one keeps running its passes until
-- the rollout finishes, and it has no reopen guard: it opens and pushes a
-- session still stopped on the streak its reset episode ended on (its waiting
-- hook, with no streak at all). The new image's pass ends such a repeat at
-- once, for the reason its predecessor ended (sessionstate.PG.EndRepeat), and
-- withdraws its push — so the reset needs no manual step at deploy: at worst a
-- push the older image sends is withdrawn within one pass.
--
-- Must apply after 0078 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default) — which matters: the reset
-- must run once. Applied again (after the rollback below), it widens the check
-- again and ends the episodes open THEN, which only withdraws their pushes.
--
-- ROLLBACK. An image from before 0079 never writes these reasons; it reads such
-- a row as an ended episode like any other. The reset is not undone (an ended
-- episode stays ended; a session still stopped opens a new one under the older
-- image). To narrow the check again (after rewriting the new reasons, e.g. to
-- 'worked'). It deletes THIS migration's row, 79, and no other (78 is
-- 0078_chief_nudge_notify's):
--   UPDATE session_episodes SET end_reason = 'worked' WHERE end_reason IN ('owner', 'viewed', 'reset');
--   ALTER TABLE session_episodes DROP CONSTRAINT IF EXISTS session_episodes_end_reason_check;
--   ALTER TABLE session_episodes ADD CONSTRAINT session_episodes_end_reason_check
--       CHECK (end_reason IN ('', 'worked', 'gone'));
--   DELETE FROM schema_migrations WHERE version = 79;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_episodes DROP CONSTRAINT IF EXISTS session_episodes_end_reason_check;
ALTER TABLE public.session_episodes ADD CONSTRAINT session_episodes_end_reason_check
    CHECK (end_reason IN ('', 'worked', 'gone', 'owner', 'viewed', 'reset'));

UPDATE public.session_episodes SET ended_at = now(), end_reason = 'reset', updated_at = now()
    WHERE ended_at IS NULL;
