package api

import (
	"net/http"

	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// THE ADMIN PAGES CANNOT LAUNDER A PASSWORD INTO A FACTOR (operator decision
// "Proof + server recovery"; docs/auth.md, "Adding a passkey").
//
// With only an admin's password, the sticky tier (migration 0188) could be
// walked around through a second account: invite B, promote B to admin, have
// B mint a reset link for the first admin, complete it (which clears the
// first admin's mark), and pair a phone on the password. So, from the admin
// pages:
//
//   - PROMOTING an account to admin takes the ACTING admin's proof (the
//     enrolment ticket: a passkey assertion or the paired phone's Confirm),
//     when the acting admin's account is in the gated tier
//     (accountHasStrongFactor: it has, or has had, a passkey or a step-up
//     phone);
//   - minting a RESET LINK for, or REMOVING THE PASSKEYS of, an account that
//     has had a factor takes the acting admin's proof, AND the acting admin's
//     own account must have had one — an admin that never has is open-tier
//     and could mint itself a proof;
//   - the proof must be VOUCHED: the paired phone's Confirm, or a passkey that
//     may approve cards — never a passkey added with the password alone;
//   - the target is told when a reset link is minted for it, and again when
//     it is used.
//
// The recovery for a sole admin who lost every factor is on the server, not
// here: `muster-server user reset-factors` (factors_reset.go).
// ---------------------------------------------------------------------------

// The admin pages' words for the proof.
const (
	AdminProofNeeded     = "This needs you to confirm it's you first: on Account, Passkeys, confirm with a passkey you have or your paired phone, then do this again within ten minutes."
	AdminProofOpenTier   = "Your own account has no passkey or phone that confirms requests, so it cannot do this for an account that has one. Add one to your account first."
	AdminProofUnvouched  = "That confirmation was made with a passkey added with your password alone, which cannot vouch for this. Confirm on your paired phone, or with a passkey your phone confirmed."
	ResetLinkMintedTitle = "An admin created a password reset link for your account"
	ResetLinkUsedTitle   = "Your account's password was reset"
	ResetLinkBody        = "If you did not ask for this, tell another admin now."
)

// adminProof decides an admin page's sensitive change: "" when it may go
// ahead — having TAKEN the acting admin's ticket when one is needed — else the
// refusal's words. giveBack returns a taken ticket, for an action that then
// failed (nil when none was taken). targetMarked: the account acted on is in
// the gated tier (a promotion passes false: what matters there is the acting
// admin alone). requireActorMarked: refuse an acting admin whose own account
// is not (a reset link, a passkey strip).
//
// 🔴 THE TICKET MUST BE VOUCHED: proven by the paired phone or by a passkey
// that may approve cards — never by a passkey a password added, which a
// password-only admin can always make.
func (s *Server) adminProof(r *http.Request, targetMarked, requireActorMarked bool) (string, func()) {
	if !s.auth.goTrueMode() || s.gt == nil {
		return "", nil
	}
	actor := requestOwner(r.Context())
	actorMarked, err := s.accountHasStrongFactor(r.Context(), actor)
	if err != nil {
		s.logger.Printf("admin: read user %d's factors: %v", actor, err)
		return PasskeyEnrolUnchecked, nil
	}
	switch {
	case requireActorMarked && targetMarked && !actorMarked:
		return AdminProofOpenTier, nil
	case !actorMarked && !targetMarked:
		// Nothing to protect, and nothing to prove with.
		return "", nil
	case requireActorMarked && !targetMarked:
		return "", nil
	}
	creds, ok := gotrueCredsFrom(r.Context())
	if !ok {
		return AdminProofNeeded, nil
	}
	key := enrolKey(actor, creds)
	t, ok := s.passkeyEnrol.take(key, s.stepUpNow())
	if !ok {
		return AdminProofNeeded, nil
	}
	// A ticket outliving a recovery from the CLI is spent, not honoured
	// (sessionFresh; TestATicketDoesNotOutliveARecovery).
	if err := s.sessionFresh(r.Context()); err != nil {
		s.logger.Printf("admin: user %d's proof is from a session cut off since: %v", actor, err)
		return AdminProofNeeded, nil
	}
	if !t.vouched {
		s.passkeyEnrol.giveBack(key, t)
		return AdminProofUnvouched, nil
	}
	return "", func() { s.passkeyEnrol.giveBack(key, t) }
}

// targetMarked reports whether account u is in the gated tier
// (accountHasStrongFactor). An error refuses: reported as gated.
func (s *Server) targetMarked(r *http.Request, u users.User) bool {
	had, err := s.accountHasStrongFactor(r.Context(), u.ID)
	if err != nil {
		s.logger.Printf("admin: read user %d's factors: %v", u.ID, err)
		return true
	}
	return had
}
