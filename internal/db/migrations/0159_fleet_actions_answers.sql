-- muster 0159 — THE FLEET'S ANSWER AND CI PROPOSALS (internal/fleet
-- actions.go; docs/chief.md "Fleet proposals").
--
-- fleet_actions.class widens to the classes phase 2b proposes:
--
--   answer_rule      session_answer continue: the fixed "Yes, continue."
--                    answering a session's held plain-text question;
--   answer_composed  session_answer composed: the chief's own answer to a
--                    session's held plain-text question;
--   answer_pick      session_answer pick: one of a held picker's OWN options,
--                    typed into the picker as the owner's tap would type it;
--   ci_result        muster's own proposal: the CI that was running when the
--                    session stopped has finished (facts muster read).
--
-- (route_to_session / route_many outside an owner turn file 0158's `custom`;
-- end_session_propose's card there is 0158's `close`, which A8's fallback
-- already writes.)
--
-- No other change: the log stays append-only by trigger (0158), its other
-- CHECKs are as they were.
--
-- Must apply after 0158_fleet_actions. 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK (after rolling the image back, and only once no row holds one of
-- the new classes — the log is append-only, so such rows stay):
--   ALTER TABLE public.fleet_actions DROP CONSTRAINT fleet_actions_class_check;
--   ALTER TABLE public.fleet_actions ADD CONSTRAINT fleet_actions_class_check
--       CHECK (class IN ('continue', 'handoff', 'stop_note', 'custom', 'wrap', 'reassign', 'close'));
--   DELETE FROM schema_migrations WHERE version = 159;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.fleet_actions DROP CONSTRAINT IF EXISTS fleet_actions_class_check;
ALTER TABLE public.fleet_actions ADD CONSTRAINT fleet_actions_class_check
    CHECK (class IN ('continue', 'handoff', 'stop_note', 'custom', 'wrap', 'reassign', 'close',
                     'answer_rule', 'answer_composed', 'answer_pick', 'ci_result'));
