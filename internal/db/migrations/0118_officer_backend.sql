-- muster 0118 — THE BUILT-IN OFFICER BACKEND'S STORE
-- (internal/officers/builtin; docs/design/officers-first-class.md "The
-- built-in backend").
--
--   officer_types      an officer type: its charter (versioned) and policy.
--   officer_defs       an officer the built-in backend serves: its type, its
--                      switch, its mode and who its escalations reach. "defs"
--                      because the declared officer list stays MUSTER_OFFICERS.
--   officer_items      the queue: one row per item, its lease, outcome and
--                      the routine pool's hand-off summary. At most ONE live
--                      item (open, claimed or waiting) per (officer, kind,
--                      subject_key): officer_items_live_subject.
--   officer_schedules  the officer's cron schedules (the table only; firing
--                      is a later slice).
--
-- 🔴 NOTHING READS OR WRITES THESE YET. The store exists and is tested; no
-- officer runs on it until the wiring lands (MUSTER_OFFICERS refuses
-- "backend": "builtin"). Applying this migration changes no behaviour.
--
-- 🔴 CLAIM-NEXT IS ONE STATEMENT: UPDATE … WHERE id = (SELECT … FOR UPDATE
-- SKIP LOCKED LIMIT 1), so two replicas never hand out one item; every lease
-- comparison takes muster's clock as a parameter, never now().
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0082's shape: owner_id
-- defaults to the principal and owner_isolation keeps one user's officers,
-- items and schedules from another. Children name their officer (and an
-- officer its type) by (owner_id, …) composite keys, so a row cannot name
-- another owner's parent. rollback/rls_off.sql turns RLS off with the rest,
-- and --rls-on re-applies this file (db.RLSOn), so every statement is
-- idempotent.
--
-- Must apply after 0117 (the contiguity test refuses a gap).
--
-- ROLLBACK. Nothing reads the tables, so an older image runs unchanged
-- beside them. To remove them (optionally):
--   DROP TABLE public.officer_schedules, public.officer_items,
--              public.officer_defs, public.officer_types;
--   DELETE FROM schema_migrations WHERE version = 118;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.officer_types (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    key             TEXT        NOT NULL CHECK (key ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(key) <= 54),
    name            TEXT        NOT NULL DEFAULT '' CHECK (length(name) <= 200),
    charter         TEXT        NOT NULL DEFAULT '' CHECK (length(charter) <= 32768),
    charter_version INTEGER     NOT NULL DEFAULT 1 CHECK (charter_version >= 1),
    policy          JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(policy) = 'object'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT officer_types_one_key UNIQUE (owner_id, key)
);

CREATE TABLE IF NOT EXISTS public.officer_defs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id    BIGINT      NOT NULL
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    officer     TEXT        NOT NULL CHECK (officer ~ '^[a-z0-9]+(-[a-z0-9]+)*:[a-z0-9]+(-[a-z0-9]+)*$'),
    type_key    TEXT        NOT NULL,
    enabled     BOOLEAN     NOT NULL DEFAULT true,
    mode        TEXT        NOT NULL DEFAULT 'live' CHECK (mode IN ('live', 'shadow')),
    escalate_to BIGINT[]    NOT NULL DEFAULT '{}' CHECK (cardinality(escalate_to) <= 20),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT officer_defs_one_officer UNIQUE (owner_id, officer),
    -- an officer's type is the part of its id before the colon
    CONSTRAINT officer_defs_type_is_the_id CHECK (type_key = split_part(officer, ':', 1)),
    CONSTRAINT officer_defs_type_fkey FOREIGN KEY (owner_id, type_key)
        REFERENCES public.officer_types (owner_id, key)
);

CREATE TABLE IF NOT EXISTS public.officer_items (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id         BIGINT      NOT NULL
                     DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                     REFERENCES public.users (id),
    officer          TEXT        NOT NULL,
    kind             TEXT        NOT NULL CHECK (length(kind) BETWEEN 1 AND 200),
    subject_key      TEXT        NOT NULL CHECK (length(subject_key) BETWEEN 1 AND 500),
    payload          JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(payload) = 'object'),
    status           TEXT        NOT NULL DEFAULT 'open'
                     CHECK (status IN ('open', 'claimed', 'waiting', 'done', 'escalated')),
    tier             TEXT        NOT NULL DEFAULT 'judgement' CHECK (tier IN ('routine', 'judgement')),
    priority         TEXT        NOT NULL DEFAULT 'normal' CHECK (priority IN ('urgent', 'high', 'normal', 'low')),
    not_before       TIMESTAMPTZ,
    blocked          BOOLEAN     NOT NULL DEFAULT false,
    claimed_by       TEXT,
    claimed_at       TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    attempts         INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    outcome          TEXT        CHECK (outcome IN ('done', 'no_action', 'escalated')),
    summary          TEXT        CHECK (char_length(summary) <= 24576),
    links            TEXT[]      NOT NULL DEFAULT '{}' CHECK (cardinality(links) <= 10),
    routine_summary  TEXT        CHECK (char_length(routine_summary) <= 24576),
    investigation    JSONB       CHECK (investigation IS NULL OR jsonb_typeof(investigation) = 'object'),
    cost_usd         NUMERIC(12, 6) CHECK (cost_usd >= 0 AND cost_usd <= 10000),
    cost_from        TEXT        CHECK (length(cost_from) <= 100),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT officer_items_def_fkey FOREIGN KEY (owner_id, officer)
        REFERENCES public.officer_defs (owner_id, officer),
    -- a claimed item has a holder and a lease
    CONSTRAINT officer_items_claimed_has_holder CHECK (status <> 'claimed' OR (claimed_by IS NOT NULL AND lease_expires_at IS NOT NULL)),
    -- a finished item has its outcome, and only a finished item has one
    CONSTRAINT officer_items_outcome_iff_finished CHECK ((status IN ('done', 'escalated')) = (outcome IS NOT NULL)),
    CONSTRAINT officer_items_escalated_outcome CHECK (status <> 'escalated' OR outcome = 'escalated'),
    CONSTRAINT officer_items_done_outcome CHECK (status <> 'done' OR outcome IN ('done', 'no_action')),
    CONSTRAINT officer_items_cost_whole CHECK ((cost_usd IS NULL) = (cost_from IS NULL))
);

-- At most one LIVE item per (officer, kind, subject_key): the dedupe an
-- enqueue relies on (ON CONFLICT … DO NOTHING, then the live row).
CREATE UNIQUE INDEX IF NOT EXISTS officer_items_live_subject
    ON public.officer_items (owner_id, officer, kind, subject_key)
    WHERE status IN ('open', 'claimed', 'waiting');
-- The queue: an officer's items by status, oldest first.
CREATE INDEX IF NOT EXISTS officer_items_queue
    ON public.officer_items (owner_id, officer, status, id);

CREATE TABLE IF NOT EXISTS public.officer_schedules (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id      BIGINT      NOT NULL
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    officer       TEXT        NOT NULL,
    name          TEXT        NOT NULL CHECK (name ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(name) <= 63),
    cron          TEXT        NOT NULL CHECK (cron ~ '^\S+( \S+){4}$' AND length(cron) <= 100),
    kind          TEXT        NOT NULL CHECK (length(kind) BETWEEN 1 AND 200),
    subject_key   TEXT        NOT NULL CHECK (length(subject_key) BETWEEN 1 AND 500),
    payload       JSONB       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(payload) = 'object'),
    tier          TEXT        NOT NULL DEFAULT 'judgement' CHECK (tier IN ('routine', 'judgement')),
    priority      TEXT        NOT NULL DEFAULT 'normal' CHECK (priority IN ('urgent', 'high', 'normal', 'low')),
    skip_if_live  BOOLEAN     NOT NULL DEFAULT true,
    enabled       BOOLEAN     NOT NULL DEFAULT true,
    next_fire_at  TIMESTAMPTZ,
    last_fired_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT officer_schedules_one_name UNIQUE (owner_id, officer, name),
    CONSTRAINT officer_schedules_def_fkey FOREIGN KEY (owner_id, officer)
        REFERENCES public.officer_defs (owner_id, officer)
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.officer_types     ALTER COLUMN owner_id SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.officer_defs      ALTER COLUMN owner_id SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.officer_items     ALTER COLUMN owner_id SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.officer_schedules ALTER COLUMN owner_id SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $rls$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['officer_types', 'officer_defs', 'officer_items', 'officer_schedules'] LOOP
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
