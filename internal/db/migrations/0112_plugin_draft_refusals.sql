-- muster 0112 — A REPLY THE PLUGIN REFUSED (docs/plugins.md#when-the-plugin-refuses-a-reply).
--
--   plugins.capabilities 'draft_checks'      the plugin CHECKS a pending reply
--       draft before the owner may approve it (GET /plugin/v1/drafts/checks,
--       POST /plugin/v1/drafts/{id}/check). Chosen when the token is minted,
--       like every capability (0102). A token without it behaves as before.
--   plugin_reply_drafts.check_state           NULL: no check is asked for (a
--       plugin without draft_checks, every draft before this migration).
--       'awaiting': filed for a plugin with draft_checks, and not approvable
--       until the plugin says. 'sendable': the plugin said it can send it.
--       'refused': the plugin said it cannot; the draft is then `rejected`, and
--       detail holds the plugin's reason. 'lapsed': no verdict came within
--       plugins.DraftCheckWait, so it became an ordinary pending draft the
--       owner may approve (and was asked to, told the plugin did not check it).
--   plugin_reply_drafts.checked_at            when the plugin's verdict came, or
--       the check lapsed.
--   plugin_reply_drafts.check_due_at          an awaiting draft's deadline for
--       the verdict (filing + plugins.DraftCheckWait).
--   session_routes.state 'send_failed'        the route's reply draft was
--       refused by its plugin — reported failed after it was claimed, or
--       refused by the check before approval. Terminal; reached from relayed
--       or fallback only. The route then owes its chief a hand-back.
--   session_routes.redraft                    on a send_failed route: the
--       hand-back goes back to the SESSION it was routed to (a new route, with
--       the plugin's reason), not to the chief. Decided once, when the route
--       became send_failed, by the message's count of such routes
--       (plugins.MaxRouteRedrafts), so a reply that keeps being refused ends
--       with the chief.
--
-- 🔴 AN UNCHECKED DRAFT CANNOT BE APPROVED, AND THE SCHEMA HOLDS IT: an
-- 'awaiting' draft is pending or left that state without being approved
-- (rejected, superseded, expired), and a 'refused' one is rejected. The store
-- refuses the approval first, with a sentence (plugins.ErrDraftUnchecked).
--
-- Must apply after 0111 (the contiguity test refuses a gap). New columns and
-- widened CHECKs on owned tables: their row-level security covers them; not
-- re-applied by db.RLSOn (0016's and 0021's bodies create their tables only IF
-- NOT EXISTS, and 0102 is not re-applied). Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image reads neither the
-- capability nor the states):
--   UPDATE plugin_reply_drafts SET status = 'expired' WHERE status = 'pending' AND check_state = 'awaiting';
--   UPDATE session_routes SET state = 'failed', handback = NULL, redraft = false WHERE state = 'send_failed';
--   UPDATE plugins SET capabilities = array_remove(capabilities, 'draft_checks')
--    WHERE 'draft_checks' = ANY (capabilities) AND cardinality(capabilities) > 1;
--   UPDATE plugins SET revoked_at = now() WHERE capabilities = '{draft_checks}' AND revoked_at IS NULL;
--   ALTER TABLE plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
--   ALTER TABLE plugins ADD CONSTRAINT plugins_capabilities_known
--     CHECK (capabilities <@ ARRAY['messages', 'viewer']::text[]);
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_redraft_check;
--   ALTER TABLE session_routes DROP COLUMN IF EXISTS redraft;
--   ALTER TABLE session_routes DROP CONSTRAINT IF EXISTS session_routes_state_check;
--   ALTER TABLE session_routes ADD CONSTRAINT session_routes_state_check
--     CHECK (state IN ('routed', 'answered', 'relayed', 'timed_out', 'fallback', 'failed', 'owner_answered'));
--   ALTER TABLE plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_check_state_known;
--   ALTER TABLE plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_check_has_time;
--   ALTER TABLE plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_unchecked_not_approved;
--   ALTER TABLE plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_refused_is_rejected;
--   DROP INDEX IF EXISTS idx_plugin_reply_drafts_awaiting_check;
--   ALTER TABLE plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_check_due;
--   ALTER TABLE plugin_reply_drafts DROP COLUMN IF EXISTS check_due_at;
--   ALTER TABLE plugin_reply_drafts DROP COLUMN IF EXISTS checked_at;
--   ALTER TABLE plugin_reply_drafts DROP COLUMN IF EXISTS check_state;
--   DELETE FROM schema_migrations WHERE version = 112;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_capabilities_known;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_capabilities_known
    CHECK (capabilities <@ ARRAY['messages', 'viewer', 'draft_checks']::text[]);

ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS check_state TEXT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS checked_at TIMESTAMPTZ;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS check_due_at TIMESTAMPTZ;

ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_check_state_known;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_check_state_known
    CHECK (check_state IS NULL OR check_state IN ('awaiting', 'sendable', 'refused', 'lapsed'));

-- A verdict and its time together.
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_check_has_time;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_check_has_time
    CHECK (coalesce(check_state IN ('sendable', 'refused', 'lapsed'), false) = (checked_at IS NOT NULL));

ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_unchecked_not_approved;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_unchecked_not_approved
    CHECK (check_state IS DISTINCT FROM 'awaiting' OR status IN ('pending', 'rejected', 'superseded', 'expired'));

ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_refused_is_rejected;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_refused_is_rejected
    CHECK (check_state IS DISTINCT FROM 'refused' OR status = 'rejected');

-- An awaiting draft always has its check's deadline (plugins.DraftCheckWait
-- after filing), past which it lapses to an ordinary pending draft.
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_check_due;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_check_due
    CHECK (check_state IS DISTINCT FROM 'awaiting' OR check_due_at IS NOT NULL);

-- GET /plugin/v1/drafts/checks: the drafts awaiting a verdict.
CREATE INDEX IF NOT EXISTS idx_plugin_reply_drafts_awaiting_check
    ON public.plugin_reply_drafts (id) WHERE status = 'pending' AND check_state = 'awaiting';

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_state_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_state_check
    CHECK (state IN ('routed', 'answered', 'relayed', 'timed_out', 'fallback', 'failed', 'owner_answered', 'send_failed'));

ALTER TABLE public.session_routes ADD COLUMN IF NOT EXISTS redraft BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_redraft_check;
ALTER TABLE public.session_routes ADD CONSTRAINT session_routes_redraft_check
    CHECK (NOT redraft OR state = 'send_failed');
