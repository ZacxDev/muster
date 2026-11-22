-- muster 0175 — MULTI-SELECT ASKS (internal/attention, internal/api).
--
--   attention_asks.multi   true: the asker's options are CHECKBOXES — the
--       owner may pick any number — and the recorded answer is the picked
--       labels joined by "; " (attention.OptionSep), with an optional note of
--       the owner's appended (" — note: …", attention.NotePrefix). false (every
--       row before this one, and every ordinary ask): one option, one button,
--       exactly as 0025 drew it.
--
-- 🔴 IT IS PAGE-ONLY. A multi-select ask is answered on the ask's own page or
-- its Asks card; the phone's notification stays label-only chips (a chip sends
-- ONE label, internal/push MaxNativeChips), so multi is never expressed as
-- checkboxes in a lock-screen action. The flag changes the form the page draws
-- and how the answer string is composed, nothing about delivery.
--
-- New column on a table under FORCE row-level security (0025): its
-- owner_isolation policy covers it and nothing re-applies it here. Must apply
-- after 0174 (the contiguity test refuses a gap). Idempotent.
--
-- ROLLBACK (after rolling the image back; an older image reads no multi column
-- and treats every ask as single-select — a multi answer is then just text):
--   ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS multi;
--   DELETE FROM schema_migrations WHERE version = 175;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS multi BOOLEAN NOT NULL DEFAULT false;
