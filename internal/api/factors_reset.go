package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZacxDev/muster/internal/users"
)

// FactorsResetter is the database half of ResetAccountFactors
// (users.PGStore).
type FactorsResetter interface {
	MarkStrongFactor(ctx context.Context, id int64) error
	ResetFactors(ctx context.Context, by users.Actor, id int64, detail map[string]any) (phones int, err error)
}

// FactorsReset is what ResetAccountFactors removed.
type FactorsReset struct {
	UserID   int64
	Passkeys int
	Phones   int
	// PasswordSet: a new password was set (newPassword was given) — reported
	// on a failure too, since it is already in effect then.
	PasswordSet bool
	// GoTrueSignOutSkipped: the account is disabled, so GoTrue (which refuses
	// /logout for a banned user) was not asked; the disable already signed
	// it out there.
	GoTrueSignOutSkipped bool
	// RecordsKept: the passkey records muster keeps in GoTrue (app_metadata)
	// could not be cleared. Harmless — each names a passkey that is gone — and
	// reported so the operator knows.
	RecordsKept error
}

// ResetAccountFactors is THE SERVER RECOVERY for an account that lost every
// passkey and phone (muster-server user reset-factors; docs/auth.md): the
// account goes back to its first-factor tier with nothing left that could
// confirm for it and no session left open. The order is the fail-closed one —
// any failure stops with the account still gated:
//
//  1. the account is looked up (an unknown one changes nothing, audits
//     nothing: users.ErrNotFound);
//  2. it is MARKED, so that "still gated" is true of every later stop;
//  3. a new password is set, when one is given (newPassword): whoever else
//     knew the old one could otherwise sign in first and pair their phone on
//     the reopened account;
//  4. every GoTrue session is signed out (its refresh tokens die) — skipped
//     for a disabled account, which the disable already signed out and
//     whose /logout GoTrue refuses;
//  5. every passkey goes, through GoTrue's admin API, and muster's records of
//     them;
//  6. in one transaction: every paired phone is deleted, muster's session
//     cutoff moved and every personal API and flow-runner token revoked, the
//     mark cleared and the audit row written. A running server ends the
//     account's open streams within its sweep interval (sweepDisabled reads
//     the cutoff), and grants no old session the reopened tier at once
//     (openTier's fresh read). Mirror host tokens are not touched: they
//     authenticate a host, and decide no factor.
//
// There is no HTTP route to it: a shell with the server's database and GoTrue
// secret is the proof.
func ResetAccountFactors(ctx context.Context, g GoTrueAuth, st FactorsResetter, by users.Actor, id int64, newPassword string) (FactorsReset, error) {
	out := FactorsReset{UserID: id}
	if g.Users == nil || st == nil {
		return out, errors.New("no account store")
	}
	u, err := g.Users.ByID(ctx, id)
	if err != nil {
		return out, err
	}
	if err := st.MarkStrongFactor(ctx, id); err != nil {
		return out, fmt.Errorf("marking the account before the reset: %w; nothing was changed", err)
	}
	if u.GotrueSub == "" {
		if newPassword != "" {
			return out, errors.New("the account has no GoTrue login, so it has no password to set; nothing was changed")
		}
	} else {
		if g.Client == nil || g.Client.BaseURL == "" {
			return out, errors.New("the account has a GoTrue login but GoTrue is not configured: its passkeys cannot be removed")
		}
		const ip = "muster-server"
		if newPassword != "" {
			if err := g.Client.AdminSetPassword(ctx, ip, u.GotrueSub, newPassword); err != nil {
				return out, fmt.Errorf("setting the new password: %w", err)
			}
			out.PasswordSet = true
		}
		if u.Disabled() {
			out.GoTrueSignOutSkipped = true
		} else if err := g.Client.AdminSignOutEverywhere(ctx, ip, u.GotrueSub); err != nil {
			return out, fmt.Errorf("signing out every GoTrue session: %w", err)
		}
		n, err := g.Client.AdminDeleteAllPasskeys(ctx, ip, u.GotrueSub)
		out.Passkeys = n
		if err != nil {
			return out, fmt.Errorf("removing the passkeys (after %d): %w", n, err)
		}
		out.RecordsKept = g.Client.AdminSetAppMetadata(ctx, ip, u.GotrueSub, passkeyMetaKey, nil)
	}
	phones, err := st.ResetFactors(ctx, by, id, map[string]any{"passkeys_removed": out.Passkeys, "password_set": out.PasswordSet})
	out.Phones = phones
	if errors.Is(err, users.ErrNotFound) {
		// Looked up above, gone now: not "no such user, nothing changed".
		return out, errors.New("the account was deleted while it was being reset")
	}
	return out, err
}
