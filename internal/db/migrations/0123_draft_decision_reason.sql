-- muster 0123 — THE OWNER'S REASON FOR REJECTING A DRAFT (internal/plugins
-- drafts.go DraftDecision.Reason; docs/plugins.md "Rejecting with a reason").
--
--   plugin_reply_drafts.decision_reason
--       optional plain text the owner gave with a rejection (the Asks card's
--       Reject, or plugin_draft_decide {decision: "reject", reason}). Shown on
--       the card's history; for an officer's OUTBOUND draft it is also the
--       detail of the draft.refused item its officer gets. NULL when none was
--       given, and on every draft that was not rejected by its owner.
--
-- The bound is the store's (plugins.MaxDecisionReasonRunes); the CHECK is the
-- backstop, so a writer that skipped the screen cannot store an unbounded or
-- blank reason.
--
-- No row-level security change: the column rides on 0016's table and policy.
-- Every statement is idempotent.
--
-- Must apply after 0122 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if another migration took 0123.
--
-- ROLLBACK. An older image never reads the column. To remove it (optionally):
--   ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_decision_reason_bound;
--   ALTER TABLE public.plugin_reply_drafts DROP COLUMN IF EXISTS decision_reason;
--   DELETE FROM schema_migrations WHERE version = 123;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS decision_reason TEXT;
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_decision_reason_bound;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_decision_reason_bound CHECK (
    decision_reason IS NULL OR (char_length(decision_reason) BETWEEN 1 AND 1000 AND btrim(decision_reason) <> ''));
