-- muster 0136 — OFFICERS DECLARED IN THE DATABASE (officers redesign O-A;
-- internal/officers declared_db.go, internal/api officer_declared.go,
-- docs/officers-builtin.md "Declared in the database").
--
-- An officer used to be declared only in MUSTER_OFFICERS, read once at boot.
-- This adds its RUNTIME declaration to its officer_defs row, so the admin API
-- creates, changes, pauses and deletes it with no restart:
--
--   runtime           'pod' | 'host' | 'auto'; NULL = the row declares no
--                     runtime (a def an env officer runs on, as before 0136).
--   host              the mirror host of a 'host' officer; '' otherwise.
--   repo              the officer's own repository (owner/name), or NULL.
--   connectors        the connectors besides github its instances may request.
--   read_repos        public repositories read with no approval (standing).
--   max_instances, idle_after_seconds, poll_seconds   NULL = the default.
--   model             the pod's model; '' = the account's default.
--   instance_slug     the SERVER-ALLOCATED agent slug its instance names are
--                     built on (`officer-<slug>-<n>`): GLOBALLY unique
--                     (officer_defs_instance_slug), because agents.name is
--                     globally unique and two owners may each declare
--                     `marketing:main`. Set iff runtime is.
--
-- 🔴 A DECLARED OFFICER'S TYPE IS ITS OWN (round 0's F5). Its type row holds
-- its charter, and 0118 tied every def to the type named by its id's part
-- before the colon — so creating or changing DB officer `scout:main` rewrote
-- type `scout`, the charter an env officer `scout:ops` of the same owner runs
-- on. officer_defs_type_is_the_id now also admits a declared def whose type
-- key IS its instance slug: a type no env officer's id can name by accident
-- (the slug carries a tag of the owner and id), written and deleted with the
-- officer. An env officer's def is held to the old rule, unchanged.
--
-- PAUSED IS THE EXISTING SWITCH, enabled (0118): a paused officer is one whose
-- switch is off, which the wake loop already honours (it stops the instances
-- and starts none). One switch, not two.
--
-- WHY officer_defs AND NOT A NEW TABLE: the declaration is 1:1 with the def
-- (one officer is one record), its owner, share class (managed: 0127) and
-- row-level security are the def's already, and its children (items,
-- schedules, contacts) already key on it. A second table would be a second
-- owner_isolation, a second share-class entry and a join on every load, for
-- no row the def does not already have.
--
-- 🔴 THE UNIQUE INDEX IS GLOBAL, ACROSS OWNERS, UNDER ROW-LEVEL SECURITY: an
-- owner cannot SEE another's slugs, but an insert of one is still refused
-- (23505), and the allocator (internal/api) moves on to its next candidate.
--
-- Inert without its code: every column is nullable or defaulted, and an older
-- image never reads them. Idempotent (db.RLSOn re-applies 0118, which leaves
-- these columns alone).
--
-- Must apply after its predecessor, 0135 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT if another migration takes 0136 first.
--
-- Before rolling the image back: nothing is needed — an older image ignores
-- these columns, so a DB-declared officer simply stops running (no env entry
-- declares it), and its instance agents are left as ordinary stopped agents
-- (`muster agent rm officer-<slug>-<n>` removes one). Then, optionally:
--
-- ROLLBACK (after rolling the image back):
--   ALTER TABLE public.officer_defs DROP CONSTRAINT IF EXISTS officer_defs_type_is_the_id;
--   ALTER TABLE public.officer_defs ADD CONSTRAINT officer_defs_type_is_the_id
--     CHECK (type_key = split_part(officer, ':', 1)) NOT VALID;
--   DROP INDEX IF EXISTS public.officer_defs_instance_slug;
--   ALTER TABLE public.officer_defs DROP CONSTRAINT IF EXISTS officer_defs_declared_check,
--     DROP COLUMN IF EXISTS runtime, DROP COLUMN IF EXISTS host, DROP COLUMN IF EXISTS repo,
--     DROP COLUMN IF EXISTS connectors, DROP COLUMN IF EXISTS read_repos,
--     DROP COLUMN IF EXISTS max_instances, DROP COLUMN IF EXISTS idle_after_seconds,
--     DROP COLUMN IF EXISTS poll_seconds, DROP COLUMN IF EXISTS model,
--     DROP COLUMN IF EXISTS instance_slug;
--   DELETE FROM schema_migrations WHERE version = 136;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.officer_defs
    ADD COLUMN IF NOT EXISTS runtime            TEXT,
    ADD COLUMN IF NOT EXISTS host               TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS repo               TEXT,
    ADD COLUMN IF NOT EXISTS connectors         TEXT[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS read_repos         TEXT[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS max_instances      INTEGER,
    ADD COLUMN IF NOT EXISTS idle_after_seconds INTEGER,
    ADD COLUMN IF NOT EXISTS poll_seconds       INTEGER,
    ADD COLUMN IF NOT EXISTS model              TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS instance_slug      TEXT;

-- The shapes. The full rules (a host officer's refusals, the repo grammar,
-- the namespace length) are internal/officers' declared.officer, which every
-- write goes through; these keep a hand-written row inside the same box.
ALTER TABLE public.officer_defs DROP CONSTRAINT IF EXISTS officer_defs_declared_check;
ALTER TABLE public.officer_defs ADD CONSTRAINT officer_defs_declared_check CHECK (
    (runtime IS NULL) = (instance_slug IS NULL)
    AND (runtime IS NULL OR runtime IN ('pod', 'host', 'auto'))
    AND (instance_slug IS NULL OR (instance_slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(instance_slug) <= 54))
    AND (host = '' OR host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$')
    AND (host = '' OR runtime = 'host')
    AND (repo IS NULL OR (length(repo) BETWEEN 3 AND 200 AND repo ~ '^[^/[:space:]]+/[^/[:space:]]+$'))
    AND cardinality(connectors) <= 20
    AND cardinality(read_repos) <= 5
    AND (max_instances IS NULL OR max_instances BETWEEN 1 AND 8)
    AND (idle_after_seconds IS NULL OR idle_after_seconds > 0)
    AND (poll_seconds IS NULL OR poll_seconds >= 10)
    AND length(model) <= 200
);

CREATE UNIQUE INDEX IF NOT EXISTS officer_defs_instance_slug
    ON public.officer_defs (instance_slug) WHERE instance_slug IS NOT NULL;

-- A declared officer's type is its own: its key is its instance slug (F5).
ALTER TABLE public.officer_defs DROP CONSTRAINT IF EXISTS officer_defs_type_is_the_id;
ALTER TABLE public.officer_defs ADD CONSTRAINT officer_defs_type_is_the_id CHECK (
    type_key = split_part(officer, ':', 1)
    OR (instance_slug IS NOT NULL AND type_key = instance_slug)
);
