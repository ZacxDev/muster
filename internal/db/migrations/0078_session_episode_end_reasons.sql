-- muster 0078 — AN EPISODE ENDS WHEN ITS OWNER HANDLES IT, AND THE BACKLOG IS
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
--     'reset'   written by THIS migration only (below); muster never writes it.
--   Each withdraws the episode's push (the restart rule: clearEndedEpisodes on
--   the next pass) without waiting for EndAfter of sustained work ('worked'),
--   which stays the rule for a session that carries on by itself. 'owner',
--   'viewed' and 'reset' are HANDLED (sessionstate.Handled): the session is not
--   opened a new episode on the streak its handled one ended on.
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
-- Must apply after 0077 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default) — which matters: the reset
-- must run once. Applied again (after the rollback below), it widens the check
-- again and ends the episodes open THEN, which only withdraws their pushes.
--
-- ROLLBACK. An image from before 0078 never writes these reasons; it reads such
-- a row as an ended episode like any other. The reset is not undone (an ended
-- episode stays ended; a session still stopped opens a new one under the older
-- image). To narrow the check again (after rewriting the new reasons, e.g. to
-- 'worked'):
--   UPDATE session_episodes SET end_reason = 'worked' WHERE end_reason IN ('owner', 'viewed', 'reset');
--   ALTER TABLE session_episodes DROP CONSTRAINT IF EXISTS session_episodes_end_reason_check;
--   ALTER TABLE session_episodes ADD CONSTRAINT session_episodes_end_reason_check
--       CHECK (end_reason IN ('', 'worked', 'gone'));
--   DELETE FROM schema_migrations WHERE version = 78;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.session_episodes DROP CONSTRAINT IF EXISTS session_episodes_end_reason_check;
ALTER TABLE public.session_episodes ADD CONSTRAINT session_episodes_end_reason_check
    CHECK (end_reason IN ('', 'worked', 'gone', 'owner', 'viewed', 'reset'));

UPDATE public.session_episodes SET ended_at = now(), end_reason = 'reset', updated_at = now()
    WHERE ended_at IS NULL;
