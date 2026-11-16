-- muster 0139 — WHICH CHARTER EACH BUILTIN INSTANCE WAS BRIEFED WITH
-- (internal/officers/builtin backend.go BriefFor / CharterCurrent;
-- docs/officers-builtin.md "Charter changes").
--
--   officer_defs.charter_briefed   {instance name: charter_version} — the
--       officer_types.charter_version each of the officer's instances was
--       last answered by officer_brief. Written by the brief itself, in the
--       statement that reads the charter it answers, so the version recorded
--       is the version the instance was handed.
--
-- 🔴 AN INSTANCE BRIEFED WITH AN OLDER CHARTER CANNOT CLAIM OR COMPLETE: an
-- instance is ONE long Claude Code conversation, resumed across scale-to-zero,
-- and the charter reaches it only through officer_brief. Before this column
-- nothing told a running (or woken) instance its type's charter was re-put,
-- so it worked on the charter it read days earlier. officer_claim (by id and
-- claim-next) and officer_complete now refuse with 409 charter_changed until
-- the instance calls officer_brief again; an instance with no entry (never
-- briefed, or briefed before this migration) is refused the same way.
--
-- WHY officer_defs AND NOT A NEW TABLE: the record is per officer and bounded
-- by its instances, its owner, share class and row-level security are the
-- def's already (0118, 0127), and the claim gate reads it beside the def's
-- type in one join. A table would be a second owner_isolation and share-class
-- entry for a map of a few integers.
--
-- Inert without its code: defaulted, and an older image never reads it.
-- Idempotent (db.RLSOn re-applies 0118, which leaves this column alone).
--
-- Must apply after its predecessor, 0138 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT if another migration takes 0139 first.
--
-- ROLLBACK (after rolling the image back; an older image ignores it):
--   ALTER TABLE public.officer_defs DROP COLUMN IF EXISTS charter_briefed;
--   DELETE FROM schema_migrations WHERE version = 139;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.officer_defs
    ADD COLUMN IF NOT EXISTS charter_briefed JSONB NOT NULL DEFAULT '{}'
        CONSTRAINT officer_defs_charter_briefed_object CHECK (jsonb_typeof(charter_briefed) = 'object');
