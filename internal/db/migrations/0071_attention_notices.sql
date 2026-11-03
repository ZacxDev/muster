-- muster 0071 — A NOTICE ON THE ATTENTION QUEUE (internal/attention; the
-- chief's stall nudges, docs/chief-nudges.md#live-mode; docs/attention.md).
--
--   attention_asks.kind   'ask' (every row before 0071, and every question an
--       agent, a session or muster puts to its owner) or 'notice': a POINTER
--       muster raised itself — today only a live chief nudge — that names a
--       session the owner may want to look at. A notice has NO options and NO
--       answer: it is never answered, only dismissed, withdrawn when its
--       session moves on, or expired. It does not count toward the per-owner
--       open-ask cap (attention.MaxOpenPerOwner), so notices never crowd out a
--       real ask.
--
-- 🔴 THE CHECK IS THE GUARD THAT A NOTICE IS NEVER ANSWERED. Code refuses an
-- answer to a notice (attention.ErrNotice, the answer UPDATE's `kind = 'ask'`);
-- this CHECK refuses it again, so no code path — present or future — can write
-- an answer onto a notice and have it relayed or delivered. A notice is also
-- always muster's own (system_asker), never a question to the chief, and never
-- carries options.
--
-- The session a notice points at is NOT copied here: it is the decision that
-- raised it (chief_nudges.ask_id), joined when drawn — the rule
-- internal/api nudgeSessionPath already follows.
--
-- Must apply after 0070 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default); the column inherits the
-- table's row-level security. Idempotent.
--
-- ROLLBACK. An image from before 0071 never reads or writes the column, but it
-- WOULD draw an open notice as an answerable ask (no options, an answer box),
-- so withdraw the open notices FIRST, then roll the image back, then (only if
-- the schema must go too):
--   UPDATE attention_asks SET status = 'cancelled', resolved_at = now()
--     WHERE kind = 'notice' AND status = 'open';
--   -- roll the image back here
--   ALTER TABLE attention_asks DROP CONSTRAINT IF EXISTS attention_asks_kind_check;
--   ALTER TABLE attention_asks DROP COLUMN IF EXISTS kind;
--   DELETE FROM schema_migrations WHERE version = 71;
-- Dropping the column keeps every notice row as an ordinary settled system ask
-- (cancelled or expired), which an older image reads as such.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'ask';

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_kind_check;
ALTER TABLE public.attention_asks ADD CONSTRAINT attention_asks_kind_check CHECK (
    kind = 'ask'
    OR (kind = 'notice'
        AND system_asker AND NOT to_chief
        AND options = '[]'::jsonb
        AND answer IS NULL
        AND status IN ('open', 'cancelled', 'expired')));
