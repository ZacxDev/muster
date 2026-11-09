-- muster 0099 — SESSION SUCCESSIONS: a new session that continues one the owner
-- was viewing (docs/design/chief-in-agent-view.md, phase 2; the store is
-- internal/plugins/session_successions.go, the server internal/api/succession.go).
--
--   session_successions   one row per succession the chief PROPOSED from a line
--       the owner typed on a session's view (succession_propose): which session
--       it continues (pred_*; pred_key is chat_messages.about of that line, never
--       a session named in transcript text), how (mode), on what (target, model,
--       repo, base), and the kickoff COPIED BYTE FOR BYTE from the session's
--       transcript (kickoff, its sha256, the message ref and block it came from).
--       The owner's one tap on the card (ask_id) is the authorisation: only then
--       is the successor dispatched, and host_dispatch_id names it.
--
-- 🔴 NO STATUS OF THE SUCCESSOR'S OWN. Whether it started, runs or ended IS its
-- host_dispatches row's status (design 3.2): a second status here could drift.
-- decided records the owner's tap only: '' (open), started (the tap reserved a
-- dispatch, host_dispatch_id set — the CHECK ties them) or declined.
--
-- 🔴 WRITERS ARE A LEDGER (asserted over the Go source): the proposal insert
-- (CreateSuccession), the link (inside ReserveHostDispatch's reservation
-- transaction, single use: an undecided row only), DeclineSuccession,
-- MarkSuperseded (replace only, once the successor runs), SetSuccessionNotified
-- and SetSuccessorSeen.
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
-- sessionSuccessionsMigrationVersion in internal/db/rollback.go).
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE IF EXISTS public.session_successions;
--   DELETE FROM schema_migrations WHERE version = 99;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.session_successions (
    id                 BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id           BIGINT      NOT NULL
                       DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                       REFERENCES public.users (id),
    -- the chief that proposed it, and the owner's view line it answered
    chief_agent        BIGINT      NOT NULL,
    owner_message      BIGINT      NOT NULL,
    pred_key           TEXT        NOT NULL CHECK (char_length(pred_key) BETWEEN 1 AND 200),
    pred_runtime       TEXT        NOT NULL CHECK (char_length(pred_runtime) <= 40),
    pred_host          TEXT        NOT NULL CHECK (char_length(pred_host) <= 100),
    pred_tmux_session  TEXT        NOT NULL CHECK (char_length(pred_tmux_session) <= 200),
    pred_tmux_window   TEXT        NOT NULL CHECK (char_length(pred_tmux_window) <= 40),
    pred_title         TEXT        NOT NULL DEFAULT '' CHECK (char_length(pred_title) <= 300),
    pred_dispatch      BIGINT,
    -- NULL until the owner chose, when the chief passed succession "?" (D2)
    mode               TEXT        CHECK (mode IS NULL OR mode IN ('replace', 'spinoff')),
    target             TEXT        NOT NULL CHECK (target ~ '^host/[a-z0-9-]{1,63}/(claude|opencode)$'),
    model              TEXT        NOT NULL DEFAULT '' CHECK (char_length(model) <= 128),
    repo               TEXT        NOT NULL CHECK (char_length(repo) BETWEEN 1 AND 200),
    base               TEXT        NOT NULL DEFAULT '' CHECK (char_length(base) <= 200),
    kickoff            TEXT        NOT NULL CHECK (octet_length(kickoff) BETWEEN 1 AND 8192),
    kickoff_sha256     TEXT        NOT NULL CHECK (kickoff_sha256 ~ '^[0-9a-f]{64}$'),
    kickoff_ref        TEXT        NOT NULL CHECK (kickoff_ref ~ '^m:[0-9a-f]{12}$'),
    -- the fenced block's index; NULL for the whole message (an unfenced kickoff)
    kickoff_block      INTEGER     CHECK (kickoff_block IS NULL OR kickoff_block >= 0),
    ask_id             BIGINT,
    decided            TEXT        NOT NULL DEFAULT '' CHECK (decided IN ('', 'started', 'declined')),
    decided_at         TIMESTAMPTZ,
    host_dispatch_id   BIGINT      REFERENCES public.host_dispatches (id),
    -- the base the successor was started from, once it was ('' = the default)
    base_used          TEXT        NOT NULL DEFAULT '' CHECK (char_length(base_used) <= 200),
    pred_state         TEXT        NOT NULL DEFAULT 'active' CHECK (pred_state IN ('active', 'superseded')),
    successor_seen_at  TIMESTAMPTZ,
    notified_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT session_successions_started CHECK ((decided = 'started') = (host_dispatch_id IS NOT NULL)),
    CONSTRAINT session_successions_decided_at CHECK ((decided = '') = (decided_at IS NULL)),
    CONSTRAINT session_successions_started_mode CHECK (decided <> 'started' OR mode IS NOT NULL),
    CONSTRAINT session_successions_superseded CHECK (pred_state = 'active' OR (mode = 'replace' AND decided = 'started'))
);

ALTER TABLE public.session_successions ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS session_successions_owner_pred ON public.session_successions (owner_id, pred_key, id DESC);
CREATE INDEX IF NOT EXISTS session_successions_owner_ask ON public.session_successions (owner_id, ask_id) WHERE ask_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS session_successions_dispatch ON public.session_successions (owner_id, host_dispatch_id)
    WHERE host_dispatch_id IS NOT NULL;

ALTER TABLE public.session_successions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.session_successions FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.session_successions;
CREATE POLICY owner_isolation ON public.session_successions
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.session_successions TO muster_app;
