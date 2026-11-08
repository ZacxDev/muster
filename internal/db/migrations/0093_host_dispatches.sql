-- muster 0093 — HOST DISPATCHES: an agent the chief started in a tmux window on
-- one of its owner's hosts (docs/design/chief-dispatch.md 3.7; the store is
-- internal/plugins/host_dispatches.go, the reconciler
-- internal/api/host_dispatch_reconcile.go).
--
--   host_dispatches   one row per host spawn the chief reserved: where it was
--       sent (host, runtime, repo, base), what the host made (window, session
--       id, worktree, branch), and where it stands (status). The row's id is
--       the `dispatch` on the wire, the window's @muster-dispatch and the
--       worktree directory's name, so a path is never reused.
--
-- 🔴 status IS THE ONLY THING THAT DECIDES A LIVE SLOT AND A FORWARD (design
-- 2.6, source C of the chief's live count): pending (asked for, not yet in a
-- listing) and running (its window listed, carrying its dispatch) hold a slot;
-- ended holds none. The CHECK ties ended to ended_at, so no row is ended
-- without saying when.
--
-- 🔴 WRITERS ARE A LEDGER (asserted over the Go source): the pending insert
-- (the reservation's own transaction), UpdateHostDispatch (never on an ended
-- row), EndHostDispatch (the only writer of ended), and
-- SetHostDispatchOrphanNotice (orphan_notice_id only, on an ended row).
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in asks' (0089) shape; no
-- DELETE grant (a row is ended, never removed).
--
-- send_queue gains chief_forward_dispatch: a row the chief's task_forward
-- queued for a host-spawned window (design 3.10), its gate re-reading this
-- table at delivery. 0064's one-origin CHECK is widened to it: a row carries
-- at most ONE of route_id, chief_nudge_id and chief_forward_dispatch.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT.
--
-- Must apply after its predecessor (the contiguity test refuses a gap). The
-- number was the next free one on main when written; it may need renumbering
-- at merge.
--
-- ROLLBACK (after rolling the image back; an older image never reads either):
--   ALTER TABLE public.send_queue DROP COLUMN IF EXISTS chief_forward_dispatch;
--   ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_one_origin;
--   ALTER TABLE public.send_queue ADD CONSTRAINT send_queue_one_origin CHECK (route_id IS NULL OR chief_nudge_id IS NULL);
--   DROP TABLE IF EXISTS public.host_dispatches;
--   DELETE FROM schema_migrations WHERE version = 93;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.host_dispatches (
    id               BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id         BIGINT      NOT NULL
                     DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                     REFERENCES public.users (id),
    host             TEXT        NOT NULL CHECK (host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    runtime          TEXT        NOT NULL CHECK (runtime IN ('claude', 'opencode')),
    model            TEXT        CHECK (model IS NULL OR char_length(model) <= 200),
    -- the chief.dispatch_held hold this dispatch completed (its id, "held:<hex>"),
    -- when the owner's tap chose a blank
    held_id          TEXT        CHECK (held_id IS NULL OR held_id ~ '^held:[0-9a-f]{32}$'),
    repo             TEXT        NOT NULL CHECK (char_length(repo) <= 200),
    base             TEXT        NOT NULL DEFAULT '' CHECK (char_length(base) <= 200),
    branch           TEXT        NOT NULL DEFAULT '' CHECK (char_length(branch) <= 200),
    worktree         TEXT        NOT NULL DEFAULT '' CHECK (char_length(worktree) <= 500),
    task_id          BIGINT      REFERENCES public.notes (id) ON DELETE SET NULL,
    -- the task.chief_dispatch audit row that reserved it: ending it releases
    -- the task's start BY THIS id
    reservation_id   BIGINT      NOT NULL,
    agent_session    TEXT        NOT NULL DEFAULT '' CHECK (char_length(agent_session) <= 200),
    tmux_window      TEXT        NOT NULL DEFAULT '' CHECK (tmux_window = '' OR tmux_window ~ '^@[0-9]+$'),
    status           TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'ended')),
    missing_listings SMALLINT    NOT NULL DEFAULT 0 CHECK (missing_listings >= 0),
    host_seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- the host's refusal (code: detail), when it refused; why it ended otherwise
    refusal          TEXT        NOT NULL DEFAULT '' CHECK (char_length(refusal) <= 500),
    ended_why        TEXT        NOT NULL DEFAULT '' CHECK (char_length(ended_why) <= 200),
    warning          TEXT        NOT NULL DEFAULT '' CHECK (char_length(warning) <= 200),
    orphan_notice_id BIGINT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at         TIMESTAMPTZ,
    CONSTRAINT host_dispatches_ended_at CHECK ((status = 'ended') = (ended_at IS NOT NULL)),
    -- the orphan notice is raised only for an ended dispatch
    CONSTRAINT host_dispatches_orphan_ended CHECK (orphan_notice_id IS NULL OR status = 'ended')
);

ALTER TABLE public.host_dispatches ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

CREATE INDEX IF NOT EXISTS host_dispatches_owner_open ON public.host_dispatches (owner_id, id) WHERE status <> 'ended';
CREATE INDEX IF NOT EXISTS host_dispatches_owner_task ON public.host_dispatches (owner_id, task_id) WHERE task_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS host_dispatches_reservation ON public.host_dispatches (owner_id, reservation_id);

ALTER TABLE public.host_dispatches ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.host_dispatches FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.host_dispatches;
CREATE POLICY owner_isolation ON public.host_dispatches
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.host_dispatches TO muster_app;

ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS chief_forward_dispatch BIGINT;

-- 0064's one-origin rule, widened to the chief forward.
ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_one_origin;
ALTER TABLE public.send_queue ADD CONSTRAINT send_queue_one_origin
    CHECK (num_nonnulls(route_id, chief_nudge_id, chief_forward_dispatch) <= 1);
