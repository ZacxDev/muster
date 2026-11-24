package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// PAIRING A PHONE WITH A STEP-UP KEY NEEDS THE ENROLMENT PROOF (operator
// decision; docs/auth.md, "Adding a passkey").
//
// A phone paired with a step-up key answers step-up cards, confirms the
// enrolment proof (and with it a passkey that may approve cards) and the
// removal proof. So if a password alone could pair one, a password alone would
// reach card approval through it. The rule is the passkey's:
//
//   - on an account that has a passkey or a phone with a step-up key, the
//     pairing code carries permission for a step-up key only when the session
//     that minted it held an enrolment ticket (a passkey you have, or your
//     phone's Confirm), which the mint TAKES; a phone redeeming a code without
//     it is refused if it brings a step-up key (it may still pair read-only);
//   - on an account that has NEVER had either (users.had_strong_factor_at,
//     migration 0188 — Sign out everywhere, which unpairs every phone, does
//     not clear it; only a completed password reset does), the first phone
//     pairs with its key on the
//     password, as before: it is the account's first factor, and it MAY decide
//     cards (a phone's step-up key is the hardware-bound proof cards were built
//     on). From then on, the rule above holds.
//
// Every pairing pushes every device already registered "A phone was paired
// to your account", outside the notification switches.
//
// The permission lives with the code, in memory, until it expires with the
// code (a spent code is refused by the redeem itself). A restart loses it: the person mints a
// new code.
// ---------------------------------------------------------------------------

// The pairing gate's words.
const (
	PairStepUpProofNeeded = "To pair a phone that confirms requests, first confirm on Account, Passkeys with a passkey you have or your paired phone, then show the pairing code again. With neither left (phones unpaired, passkeys gone, or the phone's key invalidated), the way back is a password reset by email."
	PhonePairedTitle      = "A phone was paired to your account"
	PhonePairedBody       = "If this was not you, remove it under Account, Devices, and change your password."
)

// pairingPermits is which pairing codes may pair a step-up key, by the
// sha256 of the code.
type pairingPermits struct {
	mu sync.Mutex
	m  map[string]time.Time // → expiry
}

func codeKey(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func (p *pairingPermits) allow(code string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]time.Time{}
	}
	for k, exp := range p.m {
		if time.Now().After(exp) {
			delete(p.m, k)
		}
	}
	p.m[codeKey(code)] = until
}

func (p *pairingPermits) allowed(code string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.m[codeKey(code)]
	return ok && time.Now().Before(exp)
}


// pairingStepUpAllowed decides, as a code is minted, whether it may pair a
// step-up key: the account has nothing to prove with, or the session holds a
// ticket (TAKEN here). Password mode has no passkeys and no gate. An error
// refuses (fail closed): the code then pairs read-only.
func (s *Server) pairingStepUpAllowed(r *http.Request) bool {
	if !s.auth.goTrueMode() || s.gt == nil {
		return true
	}
	creds, ok := gotrueCredsFrom(r.Context())
	if !ok {
		return false
	}
	need, err := s.passkeyEnrolNeed(r.Context(), creds, clientIP(r), true)
	if err != nil {
		s.logger.Printf("devices: the pairing gate could not read the account: %v", err)
		return false
	}
	return need.open || need.ticket != nil
}
