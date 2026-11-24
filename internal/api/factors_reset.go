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
	ResetFactors(ctx context.Context, by users.Actor, id int64, passkeysRemoved int) (phones int, err error)
}

// FactorsReset is what ResetAccountFactors removed.
type FactorsReset struct {
	UserID   int64
	Passkeys int
	Phones   int
	// PasswordSet: a new password was set (newPassword was given).
	PasswordSet bool
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
//  4. every GoTrue session is signed out (its refresh tokens die);
//  5. every passkey goes, through GoTrue's admin API, and muster's records of
//     them;
//  6. in one transaction: every paired phone is deleted, muster's session
//     cutoff moved and every API token revoked, the mark cleared and the
//     audit row written.
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
		if err := g.Client.AdminSignOutEverywhere(ctx, ip, u.GotrueSub); err != nil {
			return out, fmt.Errorf("signing out every session: %w", err)
		}
		n, err := g.Client.AdminDeleteAllPasskeys(ctx, ip, u.GotrueSub)
		out.Passkeys = n
		if err != nil {
			return out, fmt.Errorf("removing the passkeys (after %d): %w", n, err)
		}
		out.RecordsKept = g.Client.AdminSetAppMetadata(ctx, ip, u.GotrueSub, passkeyMetaKey, nil)
	}
	phones, err := st.ResetFactors(ctx, by, id, out.Passkeys)
	out.Phones = phones
	return out, err
}
