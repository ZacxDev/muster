-- muster 0160 — THE CONFIRM CARDS AN OFFICER ASKS FOR IN ITS OPERATOR CHAT
-- (internal/officers/builtin chat_cards.go; internal/api officer_own_state.go,
-- officer_operator_chat.go; docs/officers-builtin.md "Chat with it").
--
--   officer_chat_cards   one row per confirm card: an officer, chatting with a
--       person on its page, called officer_confirm_card for one of its
--       PENDING outbound drafts (kind 'draft') or its open outreach batch
--       (kind 'batch'), or officer_refusal_refile / officer_refusal_drop for
--       one of its refusals waiting on its owner (kinds 'refile', 'drop':
--       refusal_item, and refile_at for a re-file). muster draws the card in
--       that person's chat — the exact text and media, or the refusal and
--       when — and only that person's own signed-in click decides it, never
--       the officer: a tool call from the chat only PROPOSES.
--
--   🔴 THE CARD IS BOUND TO WHAT IT SHOWED WHEN IT WAS DRAWN. A draft card
--   keeps the draft's body_sha256 and every medium's sha256 (in order); a
--   batch card the batch's own batch_sha256 and its entry ids. An approval
--   passes THESE to the draft's (or batch's) one decision, which refuses
--   anything else under the row lock — so text or media changed after the
--   card was drawn is refused, however the card's page was reached.
--   card_sha256 is the SHA-256 of all of it (builtin.CardSHA256), posted back
--   by the card's form: a form for another card is refused too.
--
--   decided_at / decision: the card's own outcome, written once (a decided
--   card draws its outcome, never its buttons again).
--
--   owner_id is the PERSON WHOSE CHAT IT IS (the officer's owner, or a
--   manager of its team), never the officer's owner: one manager's cards are
--   never another's to read or approve. officer_owner + officer name the
--   officer. No foreign key to the officer or the draft: an officer is a
--   def, a declared row or a MUSTER_OFFICERS entry, and a card whose draft is
--   gone reads "no longer waiting" (the decision refuses it anyway). A
--   person's cards go with them (ON DELETE CASCADE).
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, PERSONAL share class (0156's
-- shape: one owner's rows are invisible to every other principal; no team
-- access). rollback/rls_off.sql turns it off with the rest, and --rls-on
-- re-applies this file (db.RLSOn), so every statement is idempotent.
--
-- Must apply after its predecessor, 0157_officer_cap_raises (the contiguity test
-- refuses a gap). 🔴 RENUMBER IT (and db.officerChatCardsMigrationVersion) at
-- merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE public.officer_chat_cards;
--   DELETE FROM schema_migrations WHERE version = 160;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_chat_cards (
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id) ON DELETE CASCADE,
    officer_owner BIGINT      NOT NULL CHECK (officer_owner > 0),
    officer       TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    kind          TEXT        NOT NULL CHECK (kind IN ('draft', 'batch', 'refile', 'drop')),
    draft_id      BIGINT      CHECK (draft_id > 0),
    body_sha256   TEXT        CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
    media_sha256  TEXT[],
    batch_id      TEXT        CHECK (length(batch_id) BETWEEN 1 AND 200),
    batch_sha256  TEXT        CHECK (batch_sha256 ~ '^[0-9a-f]{64}$'),
    batch_entries BIGINT[],
    refusal_item  BIGINT      CHECK (refusal_item > 0),
    refile_at     TIMESTAMPTZ,
    card_sha256   TEXT        NOT NULL CHECK (card_sha256 ~ '^[0-9a-f]{64}$'),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at    TIMESTAMPTZ,
    decision      TEXT        CHECK (decision IN ('approved', 'rejected')),
    CHECK ((decided_at IS NULL) = (decision IS NULL)),
    CHECK (
        (kind = 'draft' AND draft_id IS NOT NULL AND body_sha256 IS NOT NULL AND media_sha256 IS NOT NULL
            AND batch_id IS NULL AND batch_sha256 IS NULL AND batch_entries IS NULL)
        OR
        (kind = 'batch' AND batch_id IS NOT NULL AND batch_sha256 IS NOT NULL AND batch_entries IS NOT NULL
            AND draft_id IS NULL AND body_sha256 IS NULL AND media_sha256 IS NULL)
        OR
        (kind IN ('refile', 'drop') AND refusal_item IS NOT NULL AND (refile_at IS NOT NULL) = (kind = 'refile')
            AND draft_id IS NULL AND body_sha256 IS NULL AND media_sha256 IS NULL
            AND batch_id IS NULL AND batch_sha256 IS NULL AND batch_entries IS NULL)
    ),
    CHECK (kind IN ('refile', 'drop') OR (refusal_item IS NULL AND refile_at IS NULL))
);

CREATE INDEX IF NOT EXISTS officer_chat_cards_chat
    ON public.officer_chat_cards (owner_id, officer_owner, officer, created_at);

ALTER TABLE public.officer_chat_cards ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.officer_chat_cards ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.officer_chat_cards FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.officer_chat_cards;
CREATE POLICY owner_isolation ON public.officer_chat_cards
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.officer_chat_cards TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.officer_chat_cards_id_seq TO muster_app;
