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
--   ALTER TABLE public.plugin_pair_requests DROP COLUMN IF EXISTS push_withdrawn_at;
--   DELETE FROM schema_migrations WHERE version = 192;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.fleet_owner_marks ADD COLUMN IF NOT EXISTS proposals_push_ids BIGINT[];

ALTER TABLE public.plugin_pair_requests ADD COLUMN IF NOT EXISTS push_withdrawn_at TIMESTAMPTZ;
-- The sweep's read: pending requests whose notification is still out.
CREATE INDEX IF NOT EXISTS idx_plugin_pair_requests_push_out ON public.plugin_pair_requests (expires_at)
    WHERE status = 'pending' AND push_withdrawn_at IS NULL;
