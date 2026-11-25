-- muster 0190 — A PHONE WITH A STEP-UP KEY MARKS ITS ACCOUNT IN THE SAME
-- TRANSACTION AS IT IS PAIRED, AND AGAIN AS IT IS DELETED (internal/api
-- passkey_enrol.go accountHasStrongFactor; docs/auth.md "Adding a passkey").
--
-- 0189 made the gated tier sticky (users.had_strong_factor_at), but the mark
-- was written by the application, next to — never inside — the statements
-- that change the phones: the pairing marked best-effort after its insert, and
-- Sign out everywhere, a disable and a password reset delete every phone
-- (users.deleteDevicesTx) without marking. So an account whose step-up phone
-- was never marked (a failed mark write, or a phone paired by an image older
-- than 0189 after its backfill ran) lost its only record of the phone to
-- "Sign out everywhere" — the password alone — and then paired the attacker's
-- phone as a first factor.
--
--   muster_devices_mark_strong_factor()   the trigger function: for every row
--       of public.devices INSERTED with a step-up key, DELETED with one, or
--       UPDATED in its step-up key or its OWNER so that either side has one,
--       it sets had_strong_factor_at (kept when already set) of the old row's
--       owner when the old row had a key, and of the new row's owner when the
--       new row has one — so a keyed phone moved between accounts marks BOTH.
--       It runs in the statement's own transaction, so the phone row and the
--       mark commit or roll back together: a mark that cannot be written
--       refuses the pairing, the unpairing and the delete-everything paths
--       alike, and no row-level INSERT, UPDATE or DELETE of devices — from any
--       package or a hand-run psql — skips it.
--
--   ⚠ WHAT A ROW TRIGGER DOES NOT SEE: TRUNCATE (of devices, or of users
--   CASCADE) fires no row trigger, and a session with
--   session_replication_role = replica fires no ordinary trigger at all. No Go
--   code in the module does either (internal/api TestTheDevicesWritersAreALedger
--   pins every SQL that writes devices, and refuses a TRUNCATE of devices or
--   users); a hand-run one is outside this guarantee.
--
-- SECURITY DEFINER with the 0007 shape (search_path pinned to pg_catalog,
-- every relation schema-qualified, muster.scope = 'all' for its own duration):
-- a phone is removed under its OWNER's principal (internal/devices Remove),
-- and under row-level security a person cannot write their own users row.
-- EXECUTE is revoked from PUBLIC: a trigger function is never called directly,
-- and the trigger fires whoever runs the statement.
--
-- 🔴 THE ONE WRITER THAT CLEARS THE MARK MUST DELETE THE PHONES FIRST. The
-- server recovery (users.PGStore.ResetFactors) deletes them and THEN clears
-- the mark in the same transaction; the other order would have this trigger
-- write it back. A completed password reset deletes them in
-- ResetCredentials' transaction, and clears the mark after.
--
-- Must apply after its predecessor, 0189 (the contiguity test refuses a gap).
-- 🔴 RENUMBER IT at merge if main moved.
--
-- ROLLBACK. An older image is unaffected (it writes the same rows; the
-- trigger only adds the mark). 🔴 DROPPING THE TRIGGER UNDER THIS IMAGE REOPENS
-- THE GAP ABOVE for the delete-everything paths (Sign out everywhere, a
-- disable, a reset): this image marks a pairing best-effort in Go too
-- (internal/api devices.go), but nothing in Go marks before those deletes. Do
-- it only with an image that predates this migration. To remove it:
--   DROP TRIGGER IF EXISTS devices_mark_strong_factor ON public.devices;
--   DROP FUNCTION IF EXISTS public.muster_devices_mark_strong_factor();
--   DELETE FROM schema_migrations WHERE version = 190;

SET LOCAL lock_timeout = '3s';

CREATE OR REPLACE FUNCTION public.muster_devices_mark_strong_factor()
    RETURNS trigger
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog
    SET muster.scope = 'all'
AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'UPDATE') AND OLD.stepup_pubkey IS NOT NULL THEN
        UPDATE public.users SET had_strong_factor_at = coalesce(had_strong_factor_at, now())
            WHERE id = OLD.owner_id;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') AND NEW.stepup_pubkey IS NOT NULL THEN
        UPDATE public.users SET had_strong_factor_at = coalesce(had_strong_factor_at, now())
            WHERE id = NEW.owner_id;
    END IF;
    RETURN NULL;
END
$$;

REVOKE ALL ON FUNCTION public.muster_devices_mark_strong_factor() FROM PUBLIC;

DROP TRIGGER IF EXISTS devices_mark_strong_factor ON public.devices;
CREATE TRIGGER devices_mark_strong_factor
    AFTER INSERT OR DELETE OR UPDATE OF stepup_pubkey, owner_id ON public.devices
    FOR EACH ROW EXECUTE FUNCTION public.muster_devices_mark_strong_factor();

-- Accounts whose step-up phone was paired after 0189's backfill without a
-- mark (the gap above) are marked now.
UPDATE public.users u SET had_strong_factor_at = now()
    WHERE u.had_strong_factor_at IS NULL
      AND EXISTS (SELECT 1 FROM public.devices d WHERE d.owner_id = u.id AND d.stepup_pubkey IS NOT NULL);
