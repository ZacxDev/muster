-- muster 0182 — A REPLY DRAFT SHOWS WHAT IT ANSWERS (internal/plugins
-- reply_context.go; internal/api draft_reply_context.go; docs/plugins.md
-- "What a reply answers").
--
--   plugin_reply_drafts.reply_context   the post or message an officer's
--       OUTBOUND reply answers, as the officer read it upstream and passed it
--       with officer_post_draft: {"text", "author"?, "written_at"?, "url"?}.
--       The owner's cards draw it above the draft. NULL for every other
--       draft, and for a reply filed without one (its card says the context
--       was not provided: muster never fetches it).
--
-- 🔴 IT IS AN OUTSIDE PARTY'S WORDS, UNTRUSTED. The application screens it as
-- an inbound plugin message (16 KiB of text, a 200-character author, an
-- absolute http(s) URL) and every face escapes it; this CHECK only holds the
-- shape: a JSON object with a string `text`, on an outbound reply (no
-- message, target_kind 'reply'), bounded in size (128 KiB of jsonb text: 16
-- KiB of text at its worst-case escaping, plus the rest). Each comparison is
-- coalesced: a CHECK passes on NULL, so a missing `text` must read as false.
--
-- No row-level security change: the column rides on 0016's table and 0127's
-- team policies. A retry chain's successor carries its root's (the
-- application's insert copies it; 0175's trigger does not compare it, since
-- nothing is sent from it). Every statement is idempotent.
--
-- Must apply after its predecessor, 0181 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. An older image never reads the column (its draft columns are
-- named, never `*`). To remove it:
--   ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_reply_context_shape;
--   ALTER TABLE public.plugin_reply_drafts DROP COLUMN IF EXISTS reply_context;

ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS reply_context JSONB;

ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_reply_context_shape;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_reply_context_shape CHECK (
    reply_context IS NULL OR (
        jsonb_typeof(reply_context) = 'object'
        AND coalesce(jsonb_typeof(reply_context -> 'text'), '') = 'string'
        AND message_id IS NULL
        AND coalesce(target_kind, '') = 'reply'
        AND octet_length(reply_context::text) <= 131072));
