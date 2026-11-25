-- muster 0188 — WHEN MUSTER STARTED RECORDING WHICH PASSKEYS MAY APPROVE A
-- STEP-UP CARD (internal/api passkey_enrol.go; docs/auth.md "Adding a
-- passkey").
--
-- A passkey can approve a step-up card in the browser, and muster records, for
-- every passkey added through it, whether that one may (its record's
-- `vouched`, in GoTrue's app_metadata). A passkey with no such record is one
-- of two things: added BEFORE muster recorded it (it keeps working, card
-- approval included), or added some other way since — GoTrue's own passkey
-- endpoints called directly, or an add whose record could not be written and
-- whose clean-up failed. Nothing about the passkey itself tells the two apart,
-- and the client shapes what it sends, so muster decides by GoTrue's own
-- created_at for the passkey against ONE instant it fixes here: when this
-- migration ran.
--
--   passkey_rules.vouching_since   set once, to the migration's now(); never
--       updated by muster. A passkey with no record counts as one added
--       before the rule only when GoTrue created it before this instant.
--
-- One row (singleton). Read at scope 'all' (authz.System), as
-- instance_branding is.
--
-- Must apply after its predecessor, 0187 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. Nothing to undo: an older image ignores the table, and with it
-- every rule that reads it (it also has no card approval in the browser).
-- 🔴 LEAVE THE TABLE AND ITS ROW IN PLACE. Dropping it and running this
-- version again would set a NEW instant, and every passkey added in between
-- with no record would then count as added before the rule — trusted to
-- approve cards. Leave schema_migrations' row too, so a later run of this
-- version does not re-apply it.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.passkey_rules (
    singleton       BOOLEAN     PRIMARY KEY DEFAULT true CHECK (singleton),
    vouching_since  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO public.passkey_rules (singleton) VALUES (true) ON CONFLICT DO NOTHING;

ALTER TABLE public.passkey_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.passkey_rules FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS scope_all ON public.passkey_rules;
CREATE POLICY scope_all ON public.passkey_rules AS PERMISSIVE FOR ALL TO PUBLIC
    USING (current_setting('muster.scope', true) = 'all')
    WITH CHECK (current_setting('muster.scope', true) = 'all');

GRANT SELECT ON public.passkey_rules TO muster_app;
