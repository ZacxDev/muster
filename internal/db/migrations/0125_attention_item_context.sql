-- muster 0125 — WHAT AN OFFICER'S ASK IS ABOUT, BUILT BY MUSTER
-- (internal/attention ItemContext; internal/api officer_ask_context.go;
-- docs/officers-builtin.md "What the owner is shown").
--
--   attention_asks.item_context
--       the officer item a builtin officer's Attention ask (officer_ask_person)
--       or escalation notice is about, as MUSTER read it from the item when it
--       raised the ask — never the officer's words: the officer, the item's
--       id, kind and subject, and for an item a plugin message became, that
--       message (plugin, venue, thread, sender and whether the plugin verified
--       it, the body sanitised and bounded, an http(s) url when the plugin
--       sent one). The Attention card, the push and attention_list draw it
--       APART from the question, which stays the officer's note. NULL on every
--       other ask, and on an officer's ask about an item no plugin message
--       became (such an ask is unchanged).
--
-- The bounds are the store's (attention.Clean, item_context.go); the CHECK is
-- the backstop, so a writer that skipped them cannot store a non-object or an
-- unbounded one. Its 64 KiB is ~4x the largest context Clean can produce
-- (~16 KB: every field at its bound in 4-byte runes): a CHECK failure is not
-- ErrInvalid, so an escalation it refused would be retried on every sweep.
--
-- No row-level security change: the column rides on 0025's table and policy.
-- Every statement is idempotent.
--
-- Must apply after 0124 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if another migration took 0125.
--
-- ROLLBACK. An older image never reads the column (its asks show the question
-- alone, as before). To remove it (optionally):
--   ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_item_context_shape;
--   ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS item_context;
--   DELETE FROM schema_migrations WHERE version = 125;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS item_context JSONB;
ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_item_context_shape;
ALTER TABLE public.attention_asks ADD CONSTRAINT attention_asks_item_context_shape CHECK (
    item_context IS NULL OR (jsonb_typeof(item_context) = 'object' AND octet_length(item_context::text) <= 65536));
