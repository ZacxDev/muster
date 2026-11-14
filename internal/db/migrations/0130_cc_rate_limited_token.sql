-- muster 0130 — WHICH TOKEN A RATE-LIMIT MARK IS ABOUT (internal/ccpool).
--
--   cc_account_marks.rate_limited_token
--       the ccpool.Fingerprint (12 hex characters of a sha256) of the token
--       that hit the rate limit, exactly what auth_failed_token holds for an
--       auth failure. The pool ignores a rate_limited mark whose fingerprint
--       is not the token configured now, so replacing an account's token ends
--       its cooldown the way it already clears an auth_failed mark. '' on
--       every row written before this column: such a mark is honoured
--       whatever the token, as before, until the next mark rewrites it.
--
-- 🔴 A FINGERPRINT, NEVER THE TOKEN (0003's rule for auth_failed_token).
--
-- No row-level security change: the column rides on 0011's table and policy.
-- Every statement is idempotent.
--
-- Must apply after 0129 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if another migration took 0130.
--
-- ROLLBACK. An older image never reads the column (it honours every
-- rate_limited mark whatever the token, as before). To remove it (optionally):
--   ALTER TABLE public.cc_account_marks DROP COLUMN IF EXISTS rate_limited_token;
--   DELETE FROM schema_migrations WHERE version = 130;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.cc_account_marks ADD COLUMN IF NOT EXISTS rate_limited_token TEXT NOT NULL DEFAULT '';
