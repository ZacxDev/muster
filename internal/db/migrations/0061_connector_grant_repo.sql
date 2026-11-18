-- muster 0061 — A CONNECTOR GRANT REMEMBERS WHICH REPOSITORY IT WAS APPROVED FOR.
--
-- An officer instance's repository is not on its agent row: it is the
-- officer's `repo` in MUSTER_OFFICERS, read per request (api.Server.agentRepo),
-- and it can change under an approved grant — or appear, for an officer
-- approved while it had none (when the approve card said "approving grants
-- NOTHING usable"). Without this column the approval silently followed the
-- config to the new repository.
--
-- `repo` is the agent's effective repository at the decision; NULL means "the
-- agent row's own repository", which is exactly what every approval before
-- this migration was made against (an officer row's is '', so an officer
-- approved before 0061 reads as approved for NO repository), and what an
-- approval from a standing grant is made against (never for an officer). A
-- grant whose recorded repository is not the agent's effective one now is put
-- back to `pending` (connectors.ReconcileGrant), as 0033 does for a changed
-- credential source, and needs approving again.
--
-- Additive and nullable: no backfill, no rewrite. Must apply after 0060 (the
-- contiguity test refuses a gap). Not re-applied by db.RLSOn (no policy, grant
-- or default change) and idempotent.

SET LOCAL lock_timeout = '3s';

ALTER TABLE connector_grants
    ADD COLUMN IF NOT EXISTS repo TEXT;
