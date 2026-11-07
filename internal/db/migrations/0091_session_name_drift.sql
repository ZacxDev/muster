-- muster 0091 — A DISMISSED RENAME OFFER (docs/focus.md#when-the-work-moves-on).
--
-- When a session's focus has stopped being about its name for long enough,
-- muster OFFERS a rename to the current focus title (focus.DriftOffer); it
-- never renames on its own. The offer is computed from the focus history at
-- render time and stores nothing — except the owner's "not now":
--
--   agent_focus.drift_dismissed   the focus title the owner dismissed an
--       offer at ('' for none). The offer stays hidden while the session's
--       current focus title is exactly that title; a new title releases it.
--       Written by focus.PGStore.DismissDrift only.
--
-- 🔴 PER USER, BY CONSTRUCTION: agent_focus is owned and under FORCE
-- row-level security (0020), so a dismissal is its owner's alone. A new
-- column inherits both, so nothing here touches a policy, grant or default
-- and db.RLSOn re-applies nothing. Idempotent.
--
-- Must apply after its predecessor (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   ALTER TABLE agent_focus DROP CONSTRAINT IF EXISTS agent_focus_drift_dismissed_cap;
--   ALTER TABLE agent_focus DROP COLUMN IF EXISTS drift_dismissed;
--   DELETE FROM schema_migrations WHERE version = 91;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agent_focus ADD COLUMN IF NOT EXISTS drift_dismissed TEXT NOT NULL DEFAULT '';

-- Past any title an offer can carry (a nameable one: focus.TitleCapRunes).
ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_drift_dismissed_cap;
ALTER TABLE public.agent_focus ADD CONSTRAINT agent_focus_drift_dismissed_cap CHECK (char_length(drift_dismissed) <= 80);
