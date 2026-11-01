-- muster 0068 — READ-ONLY personal API tokens (internal/apitokens).
--
--   * api_tokens.read_only (default false): the token may only read. It is
--     chosen when the token is minted (/account/tokens) and never changes.
--     Every existing row is false, so every token minted before this one keeps
--     the full access it was minted with.
--
-- What "read" admits is decided at the token door (internal/api
-- admitAPIToken): a read-only token is refused 403 on any method but GET and
-- HEAD, on every route a personal token reaches.
--
-- The column inherits the table's ownership, grants and row-level security
-- (0009's user_isolation); no policy changes, nothing for db.RLSOn to re-apply.
--
-- 🔴 Rollback: an image from before 0068 never reads read_only, so it would
-- admit a read-only token with FULL access. Revoke every live read-only token
-- first, then drop the column:
--   UPDATE public.api_tokens SET revoked_at = now() WHERE read_only AND revoked_at IS NULL;
--   ALTER TABLE public.api_tokens DROP COLUMN read_only;
--   DELETE FROM schema_migrations WHERE version = 68;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.api_tokens ADD COLUMN IF NOT EXISTS read_only BOOLEAN NOT NULL DEFAULT false;
