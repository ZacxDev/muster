package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZacxDev/muster/internal/users"
)

// FactorsResetter is the database half of ResetAccountFactors
// (users.PGStore.ResetFactors).
type FactorsResetter interface {
	ResetFactors(ctx context.Context, by users.Actor, id int64, passkeysRemoved int) (phones int, err error)
}

// FactorsReset is what ResetAccountFactors removed.
type FactorsReset struct {
	UserID   int64
	Passkeys int
	Phones   int
	// RecordsKept: the passkey records muster keeps in GoTrue (app_metadata)
	// could not be cleared. Harmless — each names a passkey that is gone — and
	// reported so the operator knows.
	RecordsKept error
}

// ResetAccountFactors is THE SERVER RECOVERY for an account that lost every
// passkey and phone (muster-server user reset-factors; docs/auth.md): the
// account goes back to its first-factor tier, with nothing left that could
// confirm for it. The order is the fail-closed one:
//
//  1. the account is looked up (an unknown one changes nothing, audits
//     nothing: users.ErrNotFound);
//  2. every passkey goes, through GoTrue's admin API — a failure stops here
//     with the account still gated (its mark untouched);
//  3. in one transaction: every paired phone is deleted, the mark
//     (users.had_strong_factor_at) cleared and the audit row written.
//
// There is no HTTP route to it: a shell with the server's database and GoTrue
// secret is the proof.
func ResetAccountFactors(ctx context.Context, g GoTrueAuth, st FactorsResetter, by users.Actor, id int64) (FactorsReset, error) {
	out := FactorsReset{UserID: id}
	if g.Users == nil || st == nil {
		return out, errors.New("no account store")
	}
	u, err := g.Users.ByID(ctx, id)
	if err != nil {
		return out, err
	}
	if u.GotrueSub != "" {
		if g.Client == nil || g.Client.BaseURL == "" {
			return out, errors.New("the account has a GoTrue login but GoTrue is not configured: its passkeys cannot be removed")
		}
		n, err := g.Client.AdminDeleteAllPasskeys(ctx, "muster-server", u.GotrueSub)
		out.Passkeys = n
		if err != nil {
			return out, fmt.Errorf("removing the passkeys (after %d): %w; nothing else was changed", n, err)
		}
		out.RecordsKept = g.Client.AdminSetAppMetadata(ctx, "muster-server", u.GotrueSub, passkeyMetaKey, nil)
	}
	phones, err := st.ResetFactors(ctx, by, id, out.Passkeys)
	out.Phones = phones
	return out, err
}
