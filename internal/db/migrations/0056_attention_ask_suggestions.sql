-- muster 0056 — SUGGESTED REPLIES FOR AN ASK (internal/api/attention_suggest.go;
-- docs/attention.md#suggested-replies-to-an-ask).
--
--   suggestions  the suggested replies generated for an OPEN ask, a JSON array
--                of at most 3 strings: what its notification (payload v2's
--                ask.suggestions) and the ask page show as chips. Written once,
--                after the ask is filed, by one conditional UPDATE that moves
--                nothing on an ask that is no longer open
--                (attention.PGStore.SetSuggestions), so a settled ask is never
--                given chips. '[]' for an ask with none (no OpenRouter key,
--                suggestions off, nothing grounded, or every ask before this
--                migration).
--
-- ⚠ THIS STORES SUGGESTION TEXT, WHICH internal/suggest OTHERWISE NEVER DOES
-- (its sets live in memory for an hour). An ask's chips have to outlive the
-- process that made them — the notification is re-pushed carrying them, and
-- the ask page (served by any replica) draws them — so they are kept with the
-- ask, beside the question they were made from, and go with its row.
--
-- Must apply after 0055 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default) and idempotent: ADD COLUMN
-- IF NOT EXISTS, DROP/ADD CONSTRAINT.
--
-- ROLLBACK. An image from before 0056 runs over this schema unchanged (it never
-- reads or writes the column). To remove the schema itself, after rolling the
-- image back:
--   ALTER TABLE attention_asks DROP COLUMN IF EXISTS suggestions;
--   DELETE FROM schema_migrations WHERE version = 56;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS suggestions JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_suggestions_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_suggestions_check CHECK (
        jsonb_typeof(suggestions) = 'array' AND jsonb_array_length(suggestions) <= 3);
