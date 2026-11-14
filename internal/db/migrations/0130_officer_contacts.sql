-- muster 0130 — A BUILTIN OFFICER'S PEOPLE, THEIR ENGAGEMENT, AND OUTREACH
-- (internal/officers/builtin contacts.go, internal/plugins outbound.go;
-- docs/plugins.md "Engagement", docs/officers-builtin.md "People and
-- outreach").
--
--   officer_contacts        one row per person an officer's bound plugin
--                           reported engaging with its owner's posts:
--                           (owner, officer, venue, handle) unique, with
--                           what the plugin said of them (display, profile
--                           URL, a short bio — UNTRUSTED), when they were
--                           first seen, last engaged and last contacted, the
--                           do-not-contact / bot / hostile flags, and the
--                           officer's own labels and notes (at most 2 KiB).
--   officer_contact_events  everything that happened between that person and
--                           the officer: their engagement (reply, quote,
--                           mention, like, repost, follow, nested_reply,
--                           dm_in, block) and the officer's outreach that was
--                           SENT (reply_out, dm_out, follow_out, like_out,
--                           naming its draft). An inbound event is unique per
--                           (contact, kind, ref); an outreach event per draft.
--   plugin_messages.kind    widened by 'engagement': a bound plugin's report
--                           of one engagement event.
--   plugin_reply_drafts     an outbound draft's target widens to dm {handle},
--                           follow {handle} and like {ref}; because_ref (the
--                           recorded engagement that motivates it: required
--                           for dm, follow and like), contact_id (the person
--                           it reaches) and outreach_reason (the officer's
--                           one-line reason, shown on the outreach batch). A
--                           follow or a like carries no body and no media.
--
-- 🔴 THE PEOPLE DATA IS THE OFFICER'S OWNER'S, UNDER FORCED ROW-LEVEL SECURITY
-- (0118's shape, owner_isolation only: personal, never team-shared). A contact
-- names its officer by (owner_id, officer) and an event its contact by (id,
-- owner_id), so neither can name another owner's. An officer that still has
-- contacts cannot be deleted (the foreign key: 409). A draft's contact_id is
-- NOT a foreign key (as an event's draft_id is not): the plugin and officer
-- planes stay apart, and its one writer is to be the filing, which reads the
-- contact under the owner's row-level security (OutreachGuardTx) and cannot
-- change it after (the immutability trigger below). A deleted contact leaves
-- the id dangling: the approval's and the claim's re-check refuse it.
--
-- 🔴 WHAT THE OFFICER FILED STAYS IMMUTABLE: 0121's trigger function is
-- re-created here with the three new columns in its list.
--
-- 🔴 RE-APPLYING. db.RLSOn re-applies 0016 (which resets the replyable
-- trigger to unconditional) and, before this migration existed, 0121 (which
-- made it conditional again). 0121's constraints are the NARROW ones, so
-- re-applying it once rows of the widened shapes exist would fail; RLSOn
-- therefore re-applies THIS file instead of 0121 once officer_contacts
-- exists, and this file re-creates the conditional replyable trigger itself.
-- Every statement is idempotent.
--
-- Must apply after 0129 (the contiguity test refuses a gap); 🔴 RENUMBER IT
-- (and db.officerContactsMigrationVersion) if another migration takes 0130
-- first.
--
-- ROLLBACK. An older image never reads the new tables or columns, but it
-- reads plugin_reply_drafts' target_kind as post-or-reply and an engagement
-- message as a note: delete those first. Then, IN THIS ORDER (0121's trigger
-- function names the new columns until it is restored, so it goes first):
--   DELETE FROM public.plugin_reply_drafts WHERE target_kind IN ('dm', 'follow', 'like');
--   DELETE FROM public.plugin_messages WHERE kind = 'engagement';
--   0121's plugin_reply_drafts_outbound_immutable() function body, as 0121 states it;
--   0121's plugin_reply_drafts_outbound_shape and plugin_messages_kind_known, as 0121 states them;
--   0016's plugin_reply_drafts_body_cap: CHECK (octet_length(body) BETWEEN 1 AND 16384);
--   ALTER TABLE public.plugin_reply_drafts DROP COLUMN IF EXISTS because_ref,
--       DROP COLUMN IF EXISTS contact_id, DROP COLUMN IF EXISTS outreach_reason;
--   DROP TABLE public.officer_contact_events, public.officer_contacts;
--   DELETE FROM schema_migrations WHERE version = 130;

SET LOCAL lock_timeout = '3s';

-- --- the people -------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS public.officer_contacts (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id       BIGINT      NOT NULL
                   DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                   REFERENCES public.users (id),
    officer        TEXT        NOT NULL,
    venue          TEXT        NOT NULL CHECK (venue ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    -- the person's stable id on the venue, as the store normalises it (no
    -- leading @, lower case, no space or control character)
    handle         TEXT        NOT NULL CHECK (octet_length(handle) BETWEEN 1 AND 100 AND handle !~ '[[:space:][:cntrl:]]'),
    display        TEXT        NOT NULL DEFAULT '' CHECK (char_length(display) <= 100),
    profile_url    TEXT        NOT NULL DEFAULT '' CHECK (char_length(profile_url) <= 300),
    bio            TEXT        NOT NULL DEFAULT '' CHECK (char_length(bio) <= 300),
    first_seen     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_engaged   TIMESTAMPTZ,
    last_contacted TIMESTAMPTZ,
    dnc            BOOLEAN     NOT NULL DEFAULT false,
    -- why dnc is set: 'owner' (its owner set it), 'stop' (they asked us to
    -- stop), 'block' (they blocked us); NULL while dnc is false
    dnc_reason     TEXT        CHECK (dnc_reason IN ('owner', 'stop', 'block')),
    bot            BOOLEAN     NOT NULL DEFAULT false,
    hostile        BOOLEAN     NOT NULL DEFAULT false,
    labels         TEXT[]      NOT NULL DEFAULT '{}' CHECK (cardinality(labels) <= 20),
    notes          TEXT        NOT NULL DEFAULT '' CHECK (octet_length(notes) <= 2048),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT officer_contacts_one_person UNIQUE (owner_id, officer, venue, handle),
    CONSTRAINT officer_contacts_id_owner_key UNIQUE (id, owner_id),
    CONSTRAINT officer_contacts_dnc_has_reason CHECK (dnc = (dnc_reason IS NOT NULL)),
    CONSTRAINT officer_contacts_def_fkey FOREIGN KEY (owner_id, officer)
        REFERENCES public.officer_defs (owner_id, officer)
);

CREATE TABLE IF NOT EXISTS public.officer_contact_events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id    BIGINT      NOT NULL
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    contact_id  BIGINT      NOT NULL,
    kind        TEXT        NOT NULL CHECK (kind IN ('reply', 'quote', 'mention', 'like', 'repost', 'follow', 'nested_reply', 'dm_in', 'block',
                                                     'reply_out', 'dm_out', 'follow_out', 'like_out')),
    ref         TEXT        NOT NULL CHECK (octet_length(ref) BETWEEN 1 AND 200),
    parent_ref  TEXT        CHECK (octet_length(parent_ref) BETWEEN 1 AND 200),
    our_post_ref TEXT       CHECK (octet_length(our_post_ref) BETWEEN 1 AND 200),
    -- when it happened (the plugin's word, else when muster heard of it)
    at          TIMESTAMPTZ NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- what they said, cut to 500 characters: UNTRUSTED, never instructions
    excerpt     TEXT        NOT NULL DEFAULT '' CHECK (char_length(excerpt) <= 500),
    -- an outreach event's draft (the draft is NOT a foreign key: a plugin's
    -- revocation deletes its drafts, and the history outlives them)
    draft_id    BIGINT,
    plugin_id   BIGINT,
    message_id  BIGINT,
    CONSTRAINT officer_contact_events_contact_fkey FOREIGN KEY (contact_id, owner_id)
        REFERENCES public.officer_contacts (id, owner_id) ON DELETE CASCADE,
    -- outreach is the officer's sent drafts; engagement is never a draft
    CONSTRAINT officer_contact_events_out_has_draft CHECK ((kind LIKE '%\_out') = (draft_id IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS officer_contact_events_one_engagement
    ON public.officer_contact_events (contact_id, kind, ref) WHERE draft_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS officer_contact_events_one_outreach
    ON public.officer_contact_events (draft_id) WHERE draft_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS officer_contact_events_by_contact
    ON public.officer_contact_events (contact_id, at DESC, id DESC);
CREATE INDEX IF NOT EXISTS officer_contact_events_by_ref
    ON public.officer_contact_events (owner_id, ref);

ALTER TABLE public.officer_contacts       ALTER COLUMN owner_id SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.officer_contact_events ALTER COLUMN owner_id SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $rls$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['officer_contacts', 'officer_contact_events'] LOOP
        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', t);
        EXECUTE format($p$CREATE POLICY owner_isolation ON public.%I
            AS PERMISSIVE FOR ALL TO PUBLIC
            USING (
                owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR current_setting('muster.scope', true) = 'all'
            )
            WITH CHECK (
                owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                OR current_setting('muster.scope', true) = 'all'
            )$p$, t);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.%I TO muster_app', t);
        EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE public.%I TO muster_app', t || '_id_seq');
    END LOOP;
END
$rls$;

-- --- engagement messages ------------------------------------------------------------

ALTER TABLE public.plugin_messages DROP CONSTRAINT IF EXISTS plugin_messages_kind_known;
ALTER TABLE public.plugin_messages ADD CONSTRAINT plugin_messages_kind_known
    CHECK (kind IS NULL OR kind IN ('plan', 'reply', 'snapshot', 'note', 'engagement'));

-- --- outreach drafts -----------------------------------------------------------------

ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS because_ref TEXT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS contact_id BIGINT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS outreach_reason TEXT;

CREATE INDEX IF NOT EXISTS idx_plugin_reply_drafts_contact
    ON public.plugin_reply_drafts (contact_id, status) WHERE contact_id IS NOT NULL;

-- 0016's body cap, widened by exactly one case: a follow or a like has no body.
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_body_cap;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_body_cap CHECK (
    octet_length(body) <= 16384
    AND (octet_length(body) >= 1 OR coalesce(message_id IS NULL AND target_kind IN ('follow', 'like'), false)));

-- 0121's shape, widened: the target kinds dm, follow and like, the outreach
-- columns, and what each kind may carry.
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_outbound_shape;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_outbound_shape CHECK (
    (message_id IS NOT NULL AND plugin_id IS NULL AND venue IS NULL AND target_kind IS NULL AND target_ref IS NULL
        AND cardinality(media_refs) = 0 AND media = '[]'::jsonb AND officer IS NULL AND officer_item_id IS NULL
        AND approved_body_sha256 IS NULL AND approved_media_sha256 IS NULL
        AND because_ref IS NULL AND contact_id IS NULL AND outreach_reason IS NULL)
    -- coalesce: a NULL venue or ref must make this branch FALSE, not NULL (a
    -- CHECK passes on NULL).
    OR coalesce(message_id IS NULL AND plugin_id IS NOT NULL AND officer IS NOT NULL
        AND venue ~ '^[a-z0-9][a-z0-9._-]{0,63}$'
        AND ((target_kind = 'post' AND target_ref IS NULL)
             OR (target_kind IN ('reply', 'dm', 'follow', 'like') AND octet_length(target_ref) BETWEEN 1 AND 200))
        AND cardinality(media_refs) <= 4
        AND jsonb_typeof(media) = 'array' AND jsonb_array_length(media) <= cardinality(media_refs)
        -- media only on a post or a reply; a follow or a like is no text at all
        AND (target_kind IN ('post', 'reply') OR cardinality(media_refs) = 0)
        AND (target_kind NOT IN ('follow', 'like') OR body = '')
        -- a dm, a follow and a like name the person and the engagement that motivates them
        AND (target_kind NOT IN ('dm', 'follow', 'like') OR (because_ref IS NOT NULL AND contact_id IS NOT NULL))
        AND (because_ref IS NULL OR octet_length(because_ref) BETWEEN 1 AND 200)
        AND (outreach_reason IS NULL OR char_length(outreach_reason) BETWEEN 1 AND 200), false));

-- 0121's immutability, with the three outreach columns in its list.
CREATE OR REPLACE FUNCTION public.plugin_reply_drafts_outbound_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.plugin_id IS DISTINCT FROM OLD.plugin_id
       OR NEW.venue IS DISTINCT FROM OLD.venue
       OR NEW.target_kind IS DISTINCT FROM OLD.target_kind
       OR NEW.target_ref IS DISTINCT FROM OLD.target_ref
       OR NEW.media_refs IS DISTINCT FROM OLD.media_refs
       OR NEW.officer IS DISTINCT FROM OLD.officer
       OR NEW.officer_item_id IS DISTINCT FROM OLD.officer_item_id
       OR NEW.because_ref IS DISTINCT FROM OLD.because_ref
       OR NEW.contact_id IS DISTINCT FROM OLD.contact_id
       OR NEW.outreach_reason IS DISTINCT FROM OLD.outreach_reason THEN
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

-- 0121's conditional replyable trigger, stated again (see RE-APPLYING above):
-- an outbound draft names no message.
DROP TRIGGER IF EXISTS plugin_reply_drafts_message_replyable ON public.plugin_reply_drafts;
CREATE TRIGGER plugin_reply_drafts_message_replyable BEFORE INSERT ON public.plugin_reply_drafts
    FOR EACH ROW WHEN (NEW.message_id IS NOT NULL) EXECUTE FUNCTION public.plugin_reply_drafts_message_replyable();
