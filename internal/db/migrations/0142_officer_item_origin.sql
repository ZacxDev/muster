-- muster 0142 — WHO PUT A BUILTIN OFFICER'S ITEM IN ITS QUEUE
-- (internal/officers budget.go; docs/officers-builtin.md "Daily budget").
--
--   officer_items.origin   'owner' for an item the officer's owner enqueued
--       (the admin API behind `muster officer item enqueue`, "Run now", the owner's rejection
--       of an outbound draft); '' for everything else (plugins, schedules,
--       muster's own follow-ups).
--
-- 🔴 IT DECIDES WHAT A SPENT DAILY BUDGET STILL LETS THROUGH. With
-- daily_budget_usd spent, the wake loop wakes the officer, and the claim
-- hands it an item, only when origin = 'owner' and the item's kind is not one
-- of its routine_kinds (Officer.BypassesBudget). A payload field could be
-- written by anyone who can enqueue; this column is written only by the
-- enqueue paths that act for the owner.
--
-- Inert without its code: defaulted to '', and an older image never reads it.
-- Idempotent (IF NOT EXISTS; db.RLSOn re-applies 0118, which leaves this
-- column alone).
--
-- Must apply after its predecessor, 0141 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT if another migration takes 0142 first.
--
-- ROLLBACK (after rolling the image back; an older image ignores it):
--   ALTER TABLE public.officer_items DROP COLUMN IF EXISTS origin;
--   DELETE FROM schema_migrations WHERE version = 142;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.officer_items
    ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT ''
        CONSTRAINT officer_items_origin_known CHECK (origin IN ('', 'owner'));
