-- muster 0150 — A HOST TOKEN THAT HAS STARTED AGENTS STAYS MARKED
-- (internal/api host_stepup.go, "SO IS A MACHINE THAT STARTS AGENTS FOR THE
-- CHIEF"; docs/mirroring.md, "A machine that starts agents is step-up gated").
--
--   mirror_tokens.spawned_at   when this token's host first advertised
--       -agent-spawn in a hello (internal/mirror Service.markSpawned), or had
--       a host dispatch reserved on it (plugins.ReserveHostDispatch), NULL
--       before. 🔴 NOTHING CLEARS IT: a host-spawned agent runs as the
--       machine's user and can read the token, so the mark belongs to the
--       CREDENTIAL, and ends only with it — rotating the host's token (a new
--       token is unmarked; the old one, revoked, authenticates nothing). While
--       it is set, every owner-authority tool called with this token is a
--       confirmation card on the owner's phone.
--
-- The backfill marks every live token whose (owner, host) already has a host
-- dispatch row or an officer host spawn, so a host that started agents before
-- this migration is gated from the first call after it.
--
-- Idempotent; not re-applied by db.RLSOn (no new table or policy: 0008's
-- policy and grants cover the column).
--
-- Must apply after its predecessor, 0149_officer_chat_marks (the contiguity
-- test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never reads it):
--   ALTER TABLE public.mirror_tokens DROP COLUMN IF EXISTS spawned_at;
--   DELETE FROM schema_migrations WHERE version = 150;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.mirror_tokens ADD COLUMN IF NOT EXISTS spawned_at TIMESTAMPTZ;

UPDATE public.mirror_tokens t SET spawned_at = now()
WHERE t.spawned_at IS NULL AND t.revoked_at IS NULL
  AND (EXISTS (SELECT 1 FROM public.host_dispatches d WHERE d.owner_id = t.owner_id AND d.host = t.host)
    OR EXISTS (SELECT 1 FROM public.officer_host_spawns o WHERE o.owner_id = t.owner_id AND o.host = t.host));
