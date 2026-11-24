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
//     when the acting admin's account has had a factor;
//   - minting a RESET LINK for, or REMOVING THE PASSKEYS of, an account that
//     has had a factor takes the acting admin's proof, AND the acting admin's
//     own account must have had one — an admin that never has is open-tier
//     and could mint itself a proof;
//   - the target is told when a reset link is minted for it, and again when
//     it is used.
//
// The recovery for a sole admin who lost every factor is on the server, not
// here: `muster-server account clear-strong-factor`.
// ---------------------------------------------------------------------------

// The admin pages' words for the proof.
const (
	AdminProofNeeded     = "This needs you to confirm it's you first: on Account, Passkeys, confirm with a passkey you have or your paired phone, then do this again within ten minutes."
	AdminProofOpenTier   = "Your own account has no passkey or phone that confirms requests, so it cannot do this for an account that has one. Add one to your account first."
	ResetLinkMintedTitle = "An admin created a password reset link for your account"
	ResetLinkUsedTitle   = "Your account's password was reset"
	ResetLinkBody        = "If you did not ask for this, tell another admin now."
)

// adminProof decides an admin page's sensitive change: nil when it may go
// ahead (the acting admin's ticket TAKEN when one is needed), else the
// refusal's words. targetMarked: the account acted on has had a factor
// (a promotion passes false: what matters there is the acting admin alone).
// requireActorMarked: refuse an acting admin whose own account has never had
// a factor (a reset link, a passkey strip).
func (s *Server) adminProof(r *http.Request, targetMarked, requireActorMarked bool) string {
	if !s.auth.goTrueMode() || s.gt == nil {
		return ""
	}
	actor := requestOwner(r.Context())
	actorMarked, err := s.hadStrongFactor(r.Context(), actor)
	if err != nil {
		s.logger.Printf("admin: read user %d's strong-factor mark: %v", actor, err)
		return PasskeyEnrolUnchecked
	}
	switch {
	case requireActorMarked && targetMarked && !actorMarked:
		return AdminProofOpenTier
	case !actorMarked && !targetMarked:
		// Nothing to protect, and nothing to prove with.
		return ""
	case requireActorMarked && !targetMarked:
		return ""
	}
	creds, ok := gotrueCredsFrom(r.Context())
	if !ok {
		return AdminProofNeeded
	}
	if _, ok := s.passkeyEnrol.take(enrolKey(actor, creds), s.stepUpNow()); !ok {
		return AdminProofNeeded
	}
	return ""
}

// targetMarked reads whether account u has had a factor (an error refuses:
// reported as marked).
func (s *Server) targetMarked(r *http.Request, u users.User) bool {
	had, err := s.hadStrongFactor(r.Context(), u.ID)
	if err != nil {
		s.logger.Printf("admin: read user %d's strong-factor mark: %v", u.ID, err)
		return true
	}
	return had
}
