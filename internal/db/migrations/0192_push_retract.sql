-- muster 0192 — WITHDRAWING A NOTIFICATION WHEN ITS THING STOPS WAITING
-- (docs/push.md "When a notification is withdrawn"; docs/chief.md "Fleet
-- proposals").
--
--   fleet_owner_marks.proposals_push_ids   the waiting proposals the owner's
--       "N proposals waiting" notification shows, as of the last time muster
--       pushed or replaced it; NULL when no such notification is showing
--       (none sent yet, or it was withdrawn). The session-state pass compares
--       it with the proposals waiting now: once none waits, the notification is
--       withdrawn; when the set changed, it is replaced quietly, in place. Only
--       a change is acted on, so a pass with nothing new sends nothing.
--   fleet_actions.expired_why   why an expired proposal expired: 'ttl' (its
--       12 hours passed) or 'moved_on' (its session moved on), so the page a
--       stale link opens can say which without reading the outcome's prose.
--       '' for every other state and for a proposal expired before this
--       migration. The append-only trigger (0158) lets it change only while
--       the action is unfinished, as it does the rest of an action's progress.
--   plugin_pair_requests.push_withdrawn_at   when muster withdrew an expired
--       pairing request's notification (the Attention sweep), so it is
--       withdrawn once. A pairing request's expiry is never stored (its status
--       stays 'pending'): this is the only record that it was noticed.
--
-- Must apply after its predecessor, 0191_page_feedback (the contiguity test
-- refuses a gap). 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image reads none of them):
--   ALTER TABLE public.fleet_owner_marks DROP COLUMN IF EXISTS proposals_push_ids;
--   ALTER TABLE public.fleet_actions DROP COLUMN IF EXISTS expired_why;
--   ALTER TABLE public.plugin_pair_requests DROP COLUMN IF EXISTS push_withdrawn_at;
--   DELETE FROM schema_migrations WHERE version = 192;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.fleet_owner_marks ADD COLUMN IF NOT EXISTS proposals_push_ids BIGINT[];

ALTER TABLE public.fleet_actions ADD COLUMN IF NOT EXISTS expired_why TEXT NOT NULL DEFAULT ''
    CONSTRAINT fleet_actions_expired_why_check CHECK (expired_why IN ('', 'ttl', 'moved_on'));
-- A reason only on an expired action.
ALTER TABLE public.fleet_actions DROP CONSTRAINT IF EXISTS fleet_actions_expired_why_state;
ALTER TABLE public.fleet_actions ADD CONSTRAINT fleet_actions_expired_why_state CHECK (expired_why = '' OR state = 'expired');

ALTER TABLE public.plugin_pair_requests ADD COLUMN IF NOT EXISTS push_withdrawn_at TIMESTAMPTZ;
-- The sweep's read: pending requests whose notification is still out.
CREATE INDEX IF NOT EXISTS idx_plugin_pair_requests_push_out ON public.plugin_pair_requests (expires_at)
    WHERE status = 'pending' AND push_withdrawn_at IS NULL;
