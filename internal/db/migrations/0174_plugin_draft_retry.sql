-- muster 0174 — AN APPROVED OUTBOUND DRAFT RETRIES ON ITS OWN (internal/plugins
-- drafts.go "RETRY CHAINS"; docs/plugins.md "Retries").
--
-- The owner approves an outbound draft ONCE. When its plugin reports the send
-- failed AND asserts nothing reached the venue (nothing_sent) AND says the
-- cause should clear by itself (transient), muster files a SUCCESSOR row: the
-- same bytes, already approved, claimable from not_before. The failed row is
-- settled `failed` as before and its claim is never cleared: claimed_at IS
-- NULL stays the at-most-once rule, and no row is ever re-offered.
--
--   plugin_reply_drafts.nothing_sent   the plugin's result asserted nothing
--       reached the venue (refused before any submit, or provably absent).
--       Only on a `failed` row. (The result's transient flag is not stored:
--       it decides the retry in the result's transaction, and the result's
--       audit row records it.)
--   plugin_reply_drafts.retry_of       a successor's chain ROOT (the draft
--       the owner approved); NULL on the root and on every other draft.
--   plugin_reply_drafts.attempt        1 on a root, n+1 on the successor of
--       attempt n.
--   plugin_reply_drafts.not_before     a successor is not claimed before it
--       (the backoff); NULL on every other draft.
--   plugin_reply_drafts.retry_last_detail   the detail of the attempt a
--       successor follows: what the owner reads as "last: …".
--   plugin_reply_drafts.auto_from      the attempt the chain's current
--       AUTOMATIC retry budget began at: 1, or the attempt an owner's manual
--       retry filed. Automatic retries stop after plugins.MaxAutoAttempts
--       attempts counted from it.
--
-- 🔴 A SUCCESSOR IS CHECKED HERE TOO (plugin_reply_drafts_retry_successor,
-- BEFORE INSERT when retry_of is set), whatever the application did: it must
-- be born approved and unclaimed; its root must be a root of the same owner
-- carrying the same plugin, body, target, media, officer, item and approved
-- hashes, the expiry no later than the root's; and the attempt before it in
-- the chain must be `failed` with nothing_sent. So no row can re-send a post
-- that may have gone out. Its decided_by_user and requested_by_user are the
-- ROOT's (0127's and 0130's actor triggers stamp the inserting principal; this
-- trigger runs after them, by name, and restores the owner's approval of
-- record): the owner approved these bytes once, and a successor is that
-- approval, not a new one.
--
-- One successor per attempt (plugin_reply_drafts_retry_attempt) and at most
-- one live (pending or approved) successor per chain
-- (plugin_reply_drafts_retry_one_live).
--
-- No row-level security change: the columns ride on 0016's table and 0127's
-- team policies. Every statement is idempotent.
--
-- Must apply after its predecessor, 0173_instance_theme (the contiguity
-- test refuses a gap). 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. An older image never reads the columns, and its claim ignores
-- not_before — so FIRST settle the live successors (an older image would hand
-- one out before its backoff, which is harmless: its bytes were approved and
-- the attempt before it sent nothing), or leave them. To remove it:
--   DROP TRIGGER IF EXISTS plugin_reply_drafts_retry_successor ON public.plugin_reply_drafts;
--   DROP FUNCTION IF EXISTS public.plugin_reply_drafts_retry_successor();
--   DROP INDEX IF EXISTS public.plugin_reply_drafts_retry_attempt;
--   DROP INDEX IF EXISTS public.plugin_reply_drafts_retry_one_live;
--   ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_retry_of_fkey;
--   ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_retry_shape;
--   ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_result_flags;
--   ALTER TABLE public.plugin_reply_drafts DROP COLUMN IF EXISTS retry_of, DROP COLUMN IF EXISTS attempt,
--       DROP COLUMN IF EXISTS not_before, DROP COLUMN IF EXISTS nothing_sent, DROP COLUMN IF EXISTS auto_from,
--       DROP COLUMN IF EXISTS retry_last_detail;
--   DELETE FROM schema_migrations WHERE version = 174;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS nothing_sent BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS retry_of BIGINT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 1;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS not_before TIMESTAMPTZ;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS retry_last_detail TEXT NOT NULL DEFAULT '';
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS auto_from INTEGER NOT NULL DEFAULT 1;

ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_retry_of_fkey;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_retry_of_fkey
    FOREIGN KEY (retry_of) REFERENCES public.plugin_reply_drafts (id) ON DELETE CASCADE;

-- A successor is an outbound draft's, numbered after its root, and only it
-- carries a not_before.
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_retry_shape;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_retry_shape CHECK (
    attempt BETWEEN 1 AND 1000
    AND (retry_of IS NULL) = (attempt = 1)
    AND (retry_of IS NULL OR (message_id IS NULL AND retry_of <> id))
    AND (not_before IS NULL OR retry_of IS NOT NULL)
    AND auto_from BETWEEN 1 AND attempt
    AND octet_length(retry_last_detail) <= 4096);

-- The result's assertion: nothing_sent only on a failed send.
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_result_flags;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_result_flags CHECK (
    NOT nothing_sent OR status = 'failed');

CREATE UNIQUE INDEX IF NOT EXISTS plugin_reply_drafts_retry_attempt
    ON public.plugin_reply_drafts (retry_of, attempt) WHERE retry_of IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS plugin_reply_drafts_retry_one_live
    ON public.plugin_reply_drafts (retry_of) WHERE retry_of IS NOT NULL AND status IN ('pending', 'approved');

CREATE OR REPLACE FUNCTION public.plugin_reply_drafts_retry_successor() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    root public.plugin_reply_drafts%ROWTYPE;
BEGIN
    SELECT * INTO root FROM public.plugin_reply_drafts WHERE id = NEW.retry_of;
    IF NOT FOUND OR root.retry_of IS NOT NULL OR root.message_id IS NOT NULL THEN
        RAISE EXCEPTION 'plugin_reply_drafts: a retry names its chain''s root outbound draft (retry_of %)', NEW.retry_of
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.status IS DISTINCT FROM 'approved' OR NEW.claimed_at IS NOT NULL OR NEW.finished_at IS NOT NULL
       OR NEW.owner_id IS DISTINCT FROM root.owner_id
       OR NEW.plugin_id IS DISTINCT FROM root.plugin_id
       OR NEW.body IS DISTINCT FROM root.body
       OR NEW.venue IS DISTINCT FROM root.venue
       OR NEW.target_kind IS DISTINCT FROM root.target_kind
       OR NEW.target_ref IS DISTINCT FROM root.target_ref
       OR NEW.media_refs IS DISTINCT FROM root.media_refs
       OR NEW.media IS DISTINCT FROM root.media
       OR NEW.officer IS DISTINCT FROM root.officer
       OR NEW.officer_item_id IS DISTINCT FROM root.officer_item_id
       OR NEW.contact_id IS DISTINCT FROM root.contact_id
       OR root.approved_body_sha256 IS NULL
       OR NEW.approved_body_sha256 IS DISTINCT FROM root.approved_body_sha256
       OR NEW.approved_media_sha256 IS DISTINCT FROM root.approved_media_sha256
       OR NEW.expires_at > root.expires_at THEN
        RAISE EXCEPTION 'plugin_reply_drafts: a retry is its root''s approved bytes, approved and unclaimed (retry_of %)', NEW.retry_of
            USING ERRCODE = 'check_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.plugin_reply_drafts p
                    WHERE (p.id = NEW.retry_of OR p.retry_of = NEW.retry_of) AND p.attempt = NEW.attempt - 1
                      AND p.status = 'failed' AND p.nothing_sent) THEN
        RAISE EXCEPTION 'plugin_reply_drafts: attempt % of draft % follows only a failed attempt that sent nothing', NEW.attempt, NEW.retry_of
            USING ERRCODE = 'check_violation';
    END IF;
    NEW.decided_by_user := root.decided_by_user;
    NEW.requested_by_user := root.requested_by_user;
    RETURN NEW;
END
$$;

-- Named after 0127's actor_decided_by_user and 0130's actor_requested_by_user:
-- BEFORE triggers fire in name order, so this one has the last word on both.
DROP TRIGGER IF EXISTS plugin_reply_drafts_retry_successor ON public.plugin_reply_drafts;
CREATE TRIGGER plugin_reply_drafts_retry_successor BEFORE INSERT ON public.plugin_reply_drafts
    FOR EACH ROW WHEN (NEW.retry_of IS NOT NULL) EXECUTE FUNCTION public.plugin_reply_drafts_retry_successor();
