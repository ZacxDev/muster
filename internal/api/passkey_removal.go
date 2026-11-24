package api

import (
	"net/http"

	"github.com/ZacxDev/muster/internal/authz"
)

// ---------------------------------------------------------------------------
// REMOVING A PHONE OR A NOTIFICATION TARGET (docs/auth.md, "Adding a passkey").
//
// 🔴 THE "PASSKEY ADDED" NOTICE MUST NOT BE SILENCED FIRST. Removing a paired
// phone (POST /account/devices/{id}/remove) or a notification target (POST
// /account/push/{id}/remove) needed only the session, so a person holding the
// password could remove every device and then add a passkey nobody hears
// about. So on an account that can prove more (a passkey, or a paired phone
// with a step-up key), removing one takes the same ticket as adding a passkey
// (gateCredentialRemoval: a passkey you have, or your phone's Confirm, on
// Account → Passkeys). An account with neither removes as before, and every
// device registered for push is told first (the one being removed included).
//
// What stays open: a phone removing ITSELF (the app's own unpair, on the
// device tier), a browser unsubscribing its own notifications, and a password
// reset, which revokes every paired phone — the recovery for a phone lost on
// an account with no passkey. An account with a passkey proves with it, so a
// lost phone is removed with the passkey.
// ---------------------------------------------------------------------------

// The removal gate's words.
const (
	RemovalProofNeeded = "Removing a phone or a notification target needs a confirmation first: open Account, Passkeys, confirm with a passkey you have or on your paired phone, then remove it here within ten minutes."
	PhoneRemovedTitle  = "A phone was removed from your account"
	TargetRemovedTitle = "A notification target was removed from your account"
	RemovedBody        = "If this was not you, change your password and check Account, Passkeys."
)

// gateCredentialRemoval answers (false) when the session may not remove a
// device or a notification target now; true when it may — having TAKEN its
// ticket, or, on an account with nothing to prove with, having first told
// every push device (title). Password mode has no passkeys and no gate.
func (s *Server) gateCredentialRemoval(w http.ResponseWriter, r *http.Request, title string) bool {
	if !s.auth.goTrueMode() || s.gt == nil {
		return true
	}
	creds, ok := gotrueCredsFrom(r.Context())
	if !ok {
		s.renderNotice(w, r, http.StatusForbidden, "Confirm it's you", RemovalProofNeeded)
		return false
	}
	need, err := s.passkeyEnrolNeed(r.Context(), creds, clientIP(r), true)
	if err != nil {
		s.logger.Printf("account: the removal gate could not read the account: %v", err)
		s.renderNotice(w, r, http.StatusServiceUnavailable, "Not removed", PasskeyEnrolUnchecked)
		return false
	}
	switch {
	case need.ticket != nil:
		return true
	case need.open:
		s.notifySecurity(r.Context(), authz.MustFrom(r.Context()).UserID, title, RemovedBody, "/account")
		return true
	}
	s.renderNotice(w, r, http.StatusForbidden, "Confirm it's you", RemovalProofNeeded)
	return false
}

// handlePasskeyProofStatus is GET /account/passkeys/proof/status: whether this
// session holds a ticket now (the passkeys page waits on the phone with it).
func (s *Server) handlePasskeyProofStatus(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.passkeyAccountCreds(w, r)
	if !ok {
		return
	}
	_, held := s.passkeyEnrol.held(enrolKey(authz.MustFrom(r.Context()).UserID, creds), s.stepUpNow())
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"ticket": held})
}
