-- muster 0078 — THE CHIEF'S NOTIFY, AND ONE DECISION PER EPISODE
-- (internal/chiefnudge; docs/design/session-state.md, "The Chief: nudge,
-- leave or notify", step 3a).
--
--   chief_nudges.decision   gains 'notify': with session state on
--       (MUSTER_SESSION_STATE) the chief answers nudge, leave or notify, and
--       writes no question, summary or options — the session's episode carries
--       the one text-free push.
--   chief_nudges.outcome    gains 'notified': a notify, recorded. Nothing is
--       sent and no notice raised, in either mode (the episode pushes).
--   chief_nudges.episode_id the session_episodes row the decision was taken in
--       (NULL with session state off, and on every older row). Not a foreign
--       key: an episode row is the owner's like the decision, and nothing
--       deletes either.
--
-- 🔴 THE PARTIAL UNIQUE INDEX IS THE CLAIM, as chief_nudges_one_per_streak is
-- for a streak: an episode is put to the chief AT MOST ONCE, across passes,
-- restarts and replicas. Insert is ON CONFLICT DO NOTHING over both.
--
-- Must apply after 0077 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the column inherits the
-- table's row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0078 never writes 'notify' or 'notified' and
-- never reads episode_id; it runs against this schema, except that it cannot
-- read a 'notify' row any differently from an unknown decision (the tab shows
-- it as unreadable). To remove it (after deleting or rewriting any row that
-- holds the new values):
--   DROP INDEX IF EXISTS chief_nudges_one_per_episode;
--   ALTER TABLE chief_nudges DROP COLUMN IF EXISTS episode_id;
--   ALTER TABLE chief_nudges DROP CONSTRAINT IF EXISTS chief_nudges_decision_check;
--   ALTER TABLE chief_nudges ADD CONSTRAINT chief_nudges_decision_check
--       CHECK (decision IN ('nudge', 'ask', 'leave', 'invalid'));
--   ALTER TABLE chief_nudges DROP CONSTRAINT IF EXISTS chief_nudges_outcome_check;
--   ALTER TABLE chief_nudges ADD CONSTRAINT chief_nudges_outcome_check CHECK (
--       outcome IN ('would_send', 'would_ask', 'left', 'queued', 'sent', 'asked',
--                   'relay_queued', 'relayed', 'failed', 'ask_closed')
--       OR outcome ~ '^suppressed:[a-z_]{1,40}$');
--   DELETE FROM schema_migrations WHERE version = 78;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chief_nudges DROP CONSTRAINT IF EXISTS chief_nudges_decision_check;
ALTER TABLE public.chief_nudges ADD CONSTRAINT chief_nudges_decision_check
    CHECK (decision IN ('nudge', 'ask', 'leave', 'notify', 'invalid'));

ALTER TABLE public.chief_nudges DROP CONSTRAINT IF EXISTS chief_nudges_outcome_check;
ALTER TABLE public.chief_nudges ADD CONSTRAINT chief_nudges_outcome_check CHECK (
    outcome IN ('would_send', 'would_ask', 'left', 'queued', 'sent', 'asked',
                'relay_queued', 'relayed', 'failed', 'ask_closed', 'notified')
    OR outcome ~ '^suppressed:[a-z_]{1,40}$');

ALTER TABLE public.chief_nudges ADD COLUMN IF NOT EXISTS episode_id BIGINT;

CREATE UNIQUE INDEX IF NOT EXISTS chief_nudges_one_per_episode
    ON public.chief_nudges (episode_id) WHERE episode_id IS NOT NULL;
