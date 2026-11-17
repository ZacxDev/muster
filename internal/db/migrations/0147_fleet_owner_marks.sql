-- muster 0147 — THE FLEET DIGEST'S MARKS (internal/fleet; docs/chief.md
-- "Fleet digest", docs/schema.md).
--
--   fleet_owner_marks   one row per owner: the digest's "since you last
--       looked" baseline (seen_at: the look BEFORE the latest one; looked_at:
--       the latest), and the owner's "Fleet" side chat (fleet_chat_id), the
--       chief chat the digest's "Ask the chief about this" opens.
--
-- The fleet's action log (fleet_actions) is the plan's Phase 2, not this one:
-- nothing here records an action on a session.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0060's shape: one owner's
-- marks are invisible to every other principal. PERSONAL share class (no team
-- access): a team's sessions are not in an owner's fleet. rollback/rls_off.sql
-- turns it off with the rest, and --rls-on re-applies this file (db.RLSOn), so
-- every statement is idempotent.
--
-- Must apply after its predecessor, 0146_chief_usage_nudge_check (the
-- contiguity test refuses a gap). 🔴 RENUMBER IT (and
-- db.fleetOwnerMarksMigrationVersion) at merge if main moved.
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   DROP TABLE public.fleet_owner_marks;
--   DELETE FROM schema_migrations WHERE version = 147;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.fleet_owner_marks (
    owner_id      BIGINT      PRIMARY KEY
                  DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                  REFERENCES public.users (id),
    -- The digest's baseline: the look before the latest (NULL: none yet).
    seen_at       TIMESTAMPTZ,
    -- The latest look (the panel opened on the digest).
    looked_at     TIMESTAMPTZ,
    -- The owner's "Fleet" side chat. ON DELETE SET NULL: a deleted chat is
    -- opened again on demand.
    fleet_chat_id BIGINT      REFERENCES public.chat_sessions (id) ON DELETE SET NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE public.fleet_owner_marks ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.fleet_owner_marks ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.fleet_owner_marks FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.fleet_owner_marks;
CREATE POLICY owner_isolation ON public.fleet_owner_marks
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.fleet_owner_marks TO muster_app;
