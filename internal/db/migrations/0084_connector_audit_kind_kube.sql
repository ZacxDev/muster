-- muster 0084 — officer_kube_read's AUDIT ROW (internal/api officer_kube.go;
-- docs/officers.md "Reading a cluster: officer_kube_read").
--
--   connector_audit.kind gains 'kube': one row per officer_kube_read call that
--       reached a cluster or was refused by its caps — the officer and item
--       (detail), the instance (agent_name), the cluster (host), the verb
--       (method), the GET path built, the API server's status, and in detail
--       the group/version/resource, namespace, name, selectors, the minted
--       token's jti, the object count, bytes and duration. Never the answer:
--       the record type that fills detail has no field for one.
--
-- The kind CHECK is the only change. db.RLSOn re-applies 0075, whose own
-- statements re-set this CHECK to the kinds before 'kube'; RLSOn therefore
-- skips those two statements of 0075 (db.withoutAuditKindCheck), and this
-- migration is the CHECK's one definition from here on. Not re-applied by
-- db.RLSOn (it changes no policy, grant or default). Idempotent. Must apply
-- after 0083 (the contiguity test refuses a gap).
--
-- ROLLBACK (after rolling the image back; an older image never writes 'kube',
-- and its RLSOn re-sets the CHECK without it, so the rows must go first):
--   DELETE FROM public.connector_audit WHERE kind = 'kube';
--   ALTER TABLE public.connector_audit DROP CONSTRAINT connector_audit_kind_check;
--   ALTER TABLE public.connector_audit ADD CONSTRAINT connector_audit_kind_check
--     CHECK (kind IN ('proxy', 'grant', 'query', 'llm'));
--   DELETE FROM schema_migrations WHERE version = 84;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.connector_audit DROP CONSTRAINT IF EXISTS connector_audit_kind_check;
ALTER TABLE public.connector_audit ADD CONSTRAINT connector_audit_kind_check
    CHECK (kind IN ('proxy', 'grant', 'query', 'llm', 'kube'));
