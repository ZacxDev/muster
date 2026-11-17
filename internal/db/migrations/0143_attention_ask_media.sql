-- muster 0143 — WHAT AN AGENT'S ASK CARRIES BESIDE ITS QUESTION
-- (internal/attention Attachment, AskWhy; internal/api ask_media.go;
-- docs/attention.md "Attachments and context").
--
--   attention_asks.attachments
--       the files an agent attached to its attention_ask: a JSON array of
--       {"id", "name", "type", "size"} — id the upload's public id in
--       muster's upload store (migration 0024's uploads.public_id, so the
--       object is 'uploads/' || id), name the cleaned filename, type the type
--       muster SNIFFED from the bytes when it stored them, size in bytes.
--       Written only by muster, from upload rows of the ask's own owner it
--       read when the ask was filed (attention.CleanAttachments). NULL for an
--       ask with none, and on every ask filed before this migration.
--   attention_asks.why
--       why the ask is here: {"text"} — the agent's own `context` argument,
--       sanitised and bounded — or, when the agent gave none, the fallback
--       muster read when the ask was filed: {"focus", "last"} — the asking
--       session's focus title and its last assistant message, secrets
--       redacted. NULL when there is neither.
--
-- Both are the asker's words or files: UNTRUSTED. Every surface renders them
-- escaped (internal/ui ask_media.go). The bounds are the store's
-- (attention.Clean); the CHECKs are the backstop, so a writer that skipped them
-- cannot store a non-array, more than 4 files, or an unbounded why. A CHECK
-- failure is not ErrInvalid, so the cap is kept above anything Clean admits.
--
-- No row-level security change: the columns ride on 0025's table and 0127's
-- team policies, so an attachment is readable by exactly who can read its ask.
-- Every statement is idempotent.
--
-- Must apply after 0142 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if main moved: 0141 and 0142 were held by open pull
-- requests when this was written.
--
-- ROLLBACK. An older image never reads the columns (its asks show the question
-- alone, as before). To remove them (optionally):
--   ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_attachments_shape;
--   ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_why_shape;
--   ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS attachments;
--   ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS why;
--   DELETE FROM schema_migrations WHERE version = 143;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS attachments JSONB;
ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_attachments_shape;
ALTER TABLE public.attention_asks ADD CONSTRAINT attention_asks_attachments_shape CHECK (
    attachments IS NULL OR (jsonb_typeof(attachments) = 'array' AND jsonb_array_length(attachments) BETWEEN 1 AND 4
        AND octet_length(attachments::text) <= 8192));

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS why JSONB;
ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_why_shape;
ALTER TABLE public.attention_asks ADD CONSTRAINT attention_asks_why_shape CHECK (
    why IS NULL OR (jsonb_typeof(why) = 'object' AND octet_length(why::text) <= 16384));
