-- muster 0171 — A STEP-UP CONFIRMATION CARD'S STRUCTURED PARTS
-- (internal/attention step_up.go; internal/api host_stepup_card.go;
-- docs/mirroring.md "What a card shows").
--
--   attention_asks.step_up
--       a step-up card's parts, as muster built them when it filed the card:
--       {"tool", "title", "why", "asker", "about", "preview", "venue", "draft_id",
--       "media": [{"n", "ref", "type", "sha256"}], "changed", "details"} —
--       the short action title (the push title), the one-line why, who asked,
--       the one line naming who or where the call is about,
--       the preview (a post's exact text, or a plain summary of the call),
--       for a draft decision the draft it is bound to and its media, whether
--       that draft no longer matched the call when the card was filed, and
--       the boilerplate on the record (the host, the session, the call and
--       its arguments, why the phone is needed, the call's hash). The card's
--       question is built from these parts (attention.StepUpCard.Text) and
--       is what its push, its page and the phone's prompt read.
--       Written only by muster (attention.CleanStepUp), on a system ask.
--       NULL on every other ask, and on every card filed before this
--       migration (which renders its question as before).
--
-- Part of it is untrusted (a post's body, a window's label): every surface
-- renders it escaped. The bounds are the store's (attention.CleanStepUp); the
-- CHECK is the backstop (a JSON object of at most 32 KiB).
--
-- No row-level security change: the column rides on 0025's table and 0127's
-- team policies. Every statement is idempotent.
--
-- Must apply after its predecessor, 0170_officer_auto_restarts (the
-- contiguity test refuses a gap). 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. An older image never reads the column (a card shows its question
-- alone, which is the new short text; its details stay in the column). To
-- remove it (optionally):
--   ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_step_up_shape;
--   ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS step_up;
--   DELETE FROM schema_migrations WHERE version = 171;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS step_up JSONB;
ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_step_up_shape;
ALTER TABLE public.attention_asks ADD CONSTRAINT attention_asks_step_up_shape CHECK (
    step_up IS NULL OR (jsonb_typeof(step_up) = 'object' AND octet_length(step_up::text) <= 32768));
