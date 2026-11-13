-- muster 0121 — A PLUGIN BOUND TO AN OFFICER, AND THE OUTBOUND DRAFTS IT SENDS
-- (internal/plugins outbound.go; docs/plugins.md "A plugin bound to an
-- officer").
--
--   plugins.deliver_to          'officer:<id>': the plugin's messages become
--                               items in that builtin officer's queue instead
--                               of chief turns.
--   plugin_messages.kind/venue  what a bound plugin's message is (plan,
--                               reply, snapshot, note) and where it happened.
--   plugin_messages.officer_item_id
--                               the item it became. routed_to = 'officer' on
--                               such a row: delivered from birth, never a
--                               chief event.
--   plugin_reply_drafts         an OUTBOUND draft: message_id NULL, its plugin
--                               named directly (plugin_id), with its venue,
--                               target, the media refs the officer named, the
--                               bytes the plugin uploaded for them (media),
--                               its author officer and item, and — set by the
--                               approval — the body and media SHA-256 values
--                               the owner approved.
--
-- 🔴 AN OUTBOUND DRAFT NAMES NO MESSAGE, SO 0016'S "replyable" TRIGGER IS RE-
-- CREATED HERE WITH WHEN (NEW.message_id IS NOT NULL): its function reads a
-- NULL message as "not replyable" and would refuse every outbound draft.
-- db.RLSOn re-applies 0016 (restoring the unconditional trigger) and THEN this
-- file, so the order holds after an --rls-on too; every statement here is
-- idempotent for that reason.
--
-- 🔴 WHAT THE OFFICER FILED IS IMMUTABLE (plugin_reply_drafts_outbound_
-- immutable), beside 0016's body rule; the uploaded media may only grow while
-- the draft is pending and unapproved, and never once approved; the approved
-- hashes may only be set by an approval and cleared by a withdrawal.
--
-- Must apply after 0120 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT (and db.outboundDraftsMigrationVersion) at merge if another
-- migration took 0121.
--
-- ROLLBACK. An older image never reads the new columns, but it would read an
-- outbound draft's NULL message_id into an int64 and fail: delete them first.
--   DELETE FROM public.plugin_reply_drafts WHERE message_id IS NULL;
--   UPDATE public.plugins SET deliver_to = NULL;
--   UPDATE public.plugin_messages SET routed_to = NULL, routed_done_at = NULL WHERE routed_to = 'officer';
--   then the reverse of each statement below, and
--   DELETE FROM schema_migrations WHERE version = 121;

SET LOCAL lock_timeout = '3s';

-- --- the binding ---------------------------------------------------------------

ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS deliver_to TEXT;
ALTER TABLE public.plugins DROP CONSTRAINT IF EXISTS plugins_deliver_to_shape;
ALTER TABLE public.plugins ADD CONSTRAINT plugins_deliver_to_shape CHECK (
    deliver_to IS NULL OR deliver_to ~ '^officer:[a-z0-9]+(-[a-z0-9]+)*:[a-z0-9]+(-[a-z0-9]+)*$');

-- --- inbound: a bound plugin's message -----------------------------------------

ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS kind TEXT;
ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS venue TEXT;
ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS officer_item_id BIGINT;
ALTER TABLE public.plugin_messages DROP CONSTRAINT IF EXISTS plugin_messages_kind_known;
ALTER TABLE public.plugin_messages ADD CONSTRAINT plugin_messages_kind_known
    CHECK (kind IS NULL OR kind IN ('plan', 'reply', 'snapshot', 'note'));
ALTER TABLE public.plugin_messages DROP CONSTRAINT IF EXISTS plugin_messages_venue_shape;
ALTER TABLE public.plugin_messages ADD CONSTRAINT plugin_messages_venue_shape
    CHECK (venue IS NULL OR venue ~ '^[a-z0-9][a-z0-9._-]{0,63}$');

-- 0028's rule, widened: 'officer' is a bound plugin's message, delivered from
-- birth to the officer's queue (never a task event, never the chief's).
ALTER TABLE public.plugin_messages DROP CONSTRAINT IF EXISTS plugin_messages_routed_to_check;
ALTER TABLE public.plugin_messages ADD CONSTRAINT plugin_messages_routed_to_check CHECK (
    (routed_to IS NULL AND routed_done_at IS NULL AND routed_claimed_at IS NULL)
    OR (coalesce(routed_to IN ('agent', 'attention'), false) AND task_id IS NOT NULL AND status = 'delivered')
    OR (coalesce(routed_to = 'officer', false) AND task_id IS NULL AND status = 'delivered' AND routed_done_at IS NOT NULL));

-- --- outbound drafts -------------------------------------------------------------

ALTER TABLE public.plugin_reply_drafts ALTER COLUMN message_id DROP NOT NULL;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS plugin_id BIGINT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS venue TEXT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS target_kind TEXT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS target_ref TEXT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS media_refs TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS media JSONB NOT NULL DEFAULT '[]';
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS officer TEXT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS officer_item_id BIGINT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS approved_body_sha256 TEXT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS approved_media_sha256 TEXT[];

ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_plugin_owner_fkey;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_plugin_owner_fkey
    FOREIGN KEY (plugin_id, owner_id) REFERENCES public.plugins (id, owner_id) ON DELETE CASCADE;

-- A draft answers a message (a reply draft, every column below NULL or empty)
-- or is outbound (no message; its plugin, venue, target and author named).
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_outbound_shape;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_outbound_shape CHECK (
    (message_id IS NOT NULL AND plugin_id IS NULL AND venue IS NULL AND target_kind IS NULL AND target_ref IS NULL
        AND cardinality(media_refs) = 0 AND media = '[]'::jsonb AND officer IS NULL AND officer_item_id IS NULL
        AND approved_body_sha256 IS NULL AND approved_media_sha256 IS NULL)
    -- coalesce: a NULL venue or ref must make this branch FALSE, not NULL (a
    -- CHECK passes on NULL).
    OR coalesce(message_id IS NULL AND plugin_id IS NOT NULL AND officer IS NOT NULL
        AND venue ~ '^[a-z0-9][a-z0-9._-]{0,63}$'
        AND ((target_kind = 'post' AND target_ref IS NULL)
             OR (target_kind = 'reply' AND octet_length(target_ref) BETWEEN 1 AND 200))
        AND cardinality(media_refs) <= 4
        AND jsonb_typeof(media) = 'array' AND jsonb_array_length(media) <= cardinality(media_refs), false));

-- The approval's binding: an approved-or-later outbound draft carries the body
-- and media hashes it was approved on; nothing else carries them.
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_outbound_binding;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_outbound_binding CHECK (
    message_id IS NOT NULL
    OR (approved_at IS NOT NULL) = (approved_body_sha256 IS NOT NULL AND approved_media_sha256 IS NOT NULL));

CREATE INDEX IF NOT EXISTS idx_plugin_reply_drafts_outbound_plugin
    ON public.plugin_reply_drafts (plugin_id, status) WHERE message_id IS NULL;

DROP TRIGGER IF EXISTS plugin_reply_drafts_message_replyable ON public.plugin_reply_drafts;
CREATE TRIGGER plugin_reply_drafts_message_replyable BEFORE INSERT ON public.plugin_reply_drafts
    FOR EACH ROW WHEN (NEW.message_id IS NOT NULL) EXECUTE FUNCTION public.plugin_reply_drafts_message_replyable();

CREATE OR REPLACE FUNCTION public.plugin_reply_drafts_outbound_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.plugin_id IS DISTINCT FROM OLD.plugin_id
       OR NEW.venue IS DISTINCT FROM OLD.venue
       OR NEW.target_kind IS DISTINCT FROM OLD.target_kind
       OR NEW.target_ref IS DISTINCT FROM OLD.target_ref
       OR NEW.media_refs IS DISTINCT FROM OLD.media_refs
       OR NEW.officer IS DISTINCT FROM OLD.officer
       OR NEW.officer_item_id IS DISTINCT FROM OLD.officer_item_id THEN
        RAISE EXCEPTION 'plugin_reply_drafts: an outbound draft''s target and media refs are immutable (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    -- uploaded media: only while pending and unapproved, and only by adding
    -- entries (an uploaded medium is never replaced or removed)
    IF NEW.media IS DISTINCT FROM OLD.media
       AND NOT (OLD.status = 'pending' AND OLD.approved_at IS NULL AND NEW.approved_at IS NULL
                AND NEW.media @> OLD.media AND jsonb_array_length(NEW.media) > jsonb_array_length(OLD.media)) THEN
        RAISE EXCEPTION 'plugin_reply_drafts: an outbound draft''s uploaded media may only be added to while it is pending (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    -- the binding: set by the approval (with approved_at), cleared by a withdrawal
    IF (NEW.approved_body_sha256 IS DISTINCT FROM OLD.approved_body_sha256
        OR NEW.approved_media_sha256 IS DISTINCT FROM OLD.approved_media_sha256)
       AND NOT ((OLD.approved_at IS NULL AND NEW.approved_at IS NOT NULL)
                OR (OLD.approved_at IS NOT NULL AND NEW.approved_at IS NULL
                    AND NEW.approved_body_sha256 IS NULL AND NEW.approved_media_sha256 IS NULL)) THEN
        RAISE EXCEPTION 'plugin_reply_drafts: an approval''s body and media hashes change only with the approval (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_reply_drafts_outbound_immutable ON public.plugin_reply_drafts;
CREATE TRIGGER plugin_reply_drafts_outbound_immutable BEFORE UPDATE ON public.plugin_reply_drafts
    FOR EACH ROW EXECUTE FUNCTION public.plugin_reply_drafts_outbound_immutable();
