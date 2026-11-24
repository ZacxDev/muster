-- muster 0188 — AN ACCOUNT THAT HAS EVER HAD A PASSKEY OR A PHONE WITH A
-- STEP-UP KEY STAYS GATED (internal/api passkey_enrol.go, pairing_gate.go;
-- docs/auth.md "Adding a passkey").
--
-- Adding a passkey, pairing a phone that confirms requests, and removing
-- either take a proof once an account has a passkey or such a phone; an
-- account with neither adds its first factor on the password. "Has neither"
-- was read from the LIVE rows, and the live rows can be emptied by the
-- password alone: Sign out everywhere unpairs every phone (users.
-- RecordSignOutEverywhere). So a password-only session could sign out
-- everywhere, sign back in, and pair its own phone as a "first" factor.
--
--   users.had_strong_factor_at   set the first time muster knows the account
--       has a passkey or a phone with a step-up key (a passkey added, such a
--       phone paired, or either seen by the gate), and never by deleting rows:
--       signing out everywhere, removing a phone or a passkey, an admin
--       removing passkeys leave it set. Only a completed password reset (its
--       link proves more than the password) clears it. NULL: the account has
--       never had one, and its first factor is added on the password.
--
-- Backfilled here for every account with a paired phone that has a step-up
-- key. Passkeys live in GoTrue, where SQL cannot see them: muster marks an
-- account the first time its gate reads one, and before any removal of one.
--
-- Must apply after its predecessor, 0187 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. An older image ignores the column. To remove it before an older
-- image starts:
--   ALTER TABLE public.users DROP COLUMN IF EXISTS had_strong_factor_at;
--   DELETE FROM schema_migrations WHERE version = 188;
-- 🔴 Re-applying it later backfills only from the phones paired THEN: an
-- account whose phones were unpaired in between reopens to a password-only
-- first factor. Leave the column on a server that has run this version.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.users ADD COLUMN IF NOT EXISTS had_strong_factor_at TIMESTAMPTZ;

UPDATE public.users u SET had_strong_factor_at = now()
    WHERE u.had_strong_factor_at IS NULL
      AND EXISTS (SELECT 1 FROM public.devices d WHERE d.owner_id = u.id AND d.stepup_pubkey IS NOT NULL);
