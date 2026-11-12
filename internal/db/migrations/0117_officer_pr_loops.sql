-- muster 0117 — A DRAFT OFFICER'S REVIEW LOOPS, AS muster TOLD HQ
-- (internal/officers prloops.go; docs/officers.md "The review loop through HQ").
--
--   officer_pr_loops   one row per (owner, officer, repository, pull request)
--       muster announced to HQ (pr-opened): its state — 'pending' (no 200
--       from HQ yet: the draft sweep re-announces it), 'in_loop' (HQ's 200
--       named a loop item: the authoring officer then writes to the pull
--       request only under a held *.pr_fix item), 'no_taker' (HQ's 200 named
--       none) or 'closed' (never re-announced: HQ answered officer_shadow, or
--       the pull request closed first).
--
-- 🔴 WHY muster KEEPS IT. HQ's pr-opened is idempotent only while the pull
-- request's loop item is live; a repeat after that item finished enqueues a
-- new one and restarts the loop. So a re-announcement is safe only for an
-- announcement that NEVER got a 200, which only muster knows — and it must
-- survive a restart and be shared by replicas.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0082's shape: the row's
-- owner is the officer's owner. rollback/rls_off.sql turns it off with the
-- rest, and --rls-on re-applies this file (db.RLSOn), so every statement is
-- idempotent.
--
-- Must apply after 0116 (the contiguity test refuses a gap).
--
-- ROLLBACK. An older image never reads the table; roll the image back, then
-- (optionally):
--   DROP TABLE public.officer_pr_loops;
--   DELETE FROM schema_migrations WHERE version = 117;
-- A pull request announced while rolled back is not recorded, so the newer
-- image's loop rule does not apply to it.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_pr_loops (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id   BIGINT      NOT NULL
               DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
               REFERENCES public.users (id),
    officer    TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9-]+:[a-z0-9-]+$'),
    repo       TEXT        NOT NULL CHECK (repo ~ '^[a-z0-9._-]+/[a-z0-9._-]+$'),
    pr         INTEGER     NOT NULL CHECK (pr > 0),
    head_sha   TEXT        NOT NULL DEFAULT '' CHECK (head_sha ~ '^([0-9a-f]{40})?$'),
    state      TEXT        NOT NULL CHECK (state IN ('pending', 'in_loop', 'no_taker', 'closed')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT officer_pr_loops_one_row UNIQUE (owner_id, officer, repo, pr)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.officer_pr_loops ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.officer_pr_loops ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.officer_pr_loops FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.officer_pr_loops;
CREATE POLICY owner_isolation ON public.officer_pr_loops
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.officer_pr_loops TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.officer_pr_loops_id_seq TO muster_app;
