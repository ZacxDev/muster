-- muster 0099 — CHIEF PROPOSALS: an action the chief PROPOSES about one of the
-- owner's sessions, which only the owner's ONE TAP on its card starts
-- (docs/design/chief-in-agent-view.md, phase 2; the store is
-- internal/plugins/chief_proposals.go, the server internal/api/chief_proposal.go
-- and succession.go).
--
--   chief_proposals   one row per proposal: its kind, the chief that proposed
--       it, the owner's line it answers (owner_message) and the SESSION it is
--       about (subject_*; subject_key is chat_messages.about of that line,
--       never a session named in transcript text), its card (ask_id), and the
--       owner's decision (decided: '' open, accepted, declined).
--
--   kind 'succession' (phase 2): a new session that continues the subject —
--       how (mode), on what (target, model, repo, base), the kickoff COPIED
--       BYTE FOR BYTE from the subject's transcript (kickoff, its sha256, the
--       message ref and block it came from); on the tap, its successor
--       (host_dispatch_id) and the base it started from (base_used); once the
--       successor runs, a replace's subject is superseded (subject_state), and
--       the push and the successor's first view are recorded.
--   kind 'end_session' is reserved for ending the subject (the next phase,
--       which adds its own columns).
--
-- 🔴 NO STATUS OF THE SUCCESSOR'S OWN. Whether it started, runs or ended IS its
-- host_dispatches row's status (design 3.2): a second status could drift.
--
-- 🔴 WRITERS ARE A LEDGER (asserted over the Go source): the proposal insert
-- (CreateProposal), a succession's link (inside ReserveHostDispatch's
-- reservation transaction, single use: an undecided row only),
-- DeclineProposal, MarkSuperseded, SetSuccessionNotified and SetSuccessorSeen.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in host_dispatches' (0093)
-- shape; no DELETE grant.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). The
-- number was the next free one on main when written; another open branch may
-- take the same number, so it may need renumbering at merge (with
-- chiefProposalsMigrationVersion in internal/db/rollback.go).
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE IF EXISTS public.chief_proposals;
--   DELETE FROM schema_migrations WHERE version = 99;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.chief_proposals (
    id                   BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id             BIGINT      NOT NULL
                         DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                         REFERENCES public.users (id),
    kind                 TEXT        NOT NULL CHECK (kind IN ('succession', 'end_session')),
    -- the chief that proposed it, and the owner's line it answered
    chief_agent          BIGINT      NOT NULL,
    owner_message        BIGINT      NOT NULL,
    subject_key          TEXT        NOT NULL CHECK (char_length(subject_key) BETWEEN 1 AND 200),
    subject_runtime      TEXT        NOT NULL CHECK (char_length(subject_runtime) <= 40),
    subject_host         TEXT        NOT NULL CHECK (char_length(subject_host) <= 100),
    subject_tmux_session TEXT        NOT NULL CHECK (char_length(subject_tmux_session) <= 200),
    subject_tmux_window  TEXT        NOT NULL CHECK (char_length(subject_tmux_window) <= 40),
    subject_title        TEXT        NOT NULL DEFAULT '' CHECK (char_length(subject_title) <= 300),
    subject_dispatch     BIGINT,
    ask_id               BIGINT,
    decided              TEXT        NOT NULL DEFAULT '' CHECK (decided IN ('', 'accepted', 'declined')),
    decided_at           TIMESTAMPTZ,
    -- kind 'succession': NULL until the owner chose, when the chief passed
    -- succession "?" (D2)
    mode                 TEXT        CHECK (mode IS NULL OR mode IN ('replace', 'spinoff')),
    target               TEXT        NOT NULL DEFAULT '' CHECK (target = '' OR target ~ '^host/[a-z0-9-]{1,63}/(claude|opencode)$'),
    model                TEXT        NOT NULL DEFAULT '' CHECK (char_length(model) <= 128),
    repo                 TEXT        NOT NULL DEFAULT '' CHECK (char_length(repo) <= 200),
    base                 TEXT        NOT NULL DEFAULT '' CHECK (char_length(base) <= 200),
    kickoff              TEXT        NOT NULL DEFAULT '' CHECK (octet_length(kickoff) <= 8192),
    kickoff_sha256       TEXT        NOT NULL DEFAULT '' CHECK (kickoff_sha256 = '' OR kickoff_sha256 ~ '^[0-9a-f]{64}$'),
    kickoff_ref          TEXT        NOT NULL DEFAULT '' CHECK (kickoff_ref = '' OR kickoff_ref ~ '^m:[0-9a-f]{12}$'),
    -- the fenced block's index; NULL for the whole message (an unfenced kickoff)
    kickoff_block        INTEGER     CHECK (kickoff_block IS NULL OR kickoff_block >= 0),
    host_dispatch_id     BIGINT      REFERENCES public.host_dispatches (id),
    -- the base the successor was started from, once it was ('' = the default)
    base_used            TEXT        NOT NULL DEFAULT '' CHECK (char_length(base_used) <= 200),
    subject_state        TEXT        NOT NULL DEFAULT 'active' CHECK (subject_state IN ('active', 'superseded')),
    successor_seen_at    TIMESTAMPTZ,
    notified_at          TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chief_proposals_decided_at CHECK ((decided = '') = (decided_at IS NULL)),
    -- a succession carries what it starts: a target, a repository and a kickoff
    CONSTRAINT chief_proposals_succession CHECK (kind <> 'succession' OR
        (target <> '' AND repo <> '' AND octet_length(kickoff) >= 1 AND kickoff_sha256 <> '' AND kickoff_ref <> '')),
    -- a succession is accepted exactly when its successor was reserved, with a mode
    CONSTRAINT chief_proposals_succession_started CHECK (kind <> 'succession' OR
        ((decided = 'accepted') = (host_dispatch_id IS NOT NULL) AND (decided <> 'accepted' OR mode IS NOT NULL))),
    CONSTRAINT chief_proposals_succession_only CHECK (kind = 'succession' OR
        (host_dispatch_id IS NULL AND mode IS NULL AND subject_state = 'active')),
    CONSTRAINT chief_proposals_superseded CHECK (subject_state = 'active' OR (mode = 'replace' AND decided = 'accepted'))
);

ALTER TABLE public.chief_proposals ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS chief_proposals_owner_subject ON public.chief_proposals (owner_id, kind, subject_key, id DESC);
CREATE INDEX IF NOT EXISTS chief_proposals_owner_ask ON public.chief_proposals (owner_id, ask_id) WHERE ask_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS chief_proposals_dispatch ON public.chief_proposals (owner_id, host_dispatch_id)
    WHERE host_dispatch_id IS NOT NULL;

ALTER TABLE public.chief_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.chief_proposals FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.chief_proposals;
CREATE POLICY owner_isolation ON public.chief_proposals
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.chief_proposals TO muster_app;
