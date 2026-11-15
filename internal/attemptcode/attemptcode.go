// Package attemptcode is a team host's ATTEMPT CODE (team sharing 2b.2
// follow-up; internal/mirror's teamhold.go): drawn by muster-mirror, checked
// by the hub, shown by `muster doctor`. 🔴 A LEAF: it imports only the
// standard library, so the CLI links it without the mirror package.
package attemptcode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ---------------------------------------------------------------------------
// WHY A CODE.
//
// An address tells no machines apart behind a proxy or a Kubernetes NodePort
// (#685 round 1, Y1), so a held connection is told apart by a code instead:
// muster-mirror draws a random code once per PROCESS (New), sends
// it on every /mirror/connect as Header — a header, so the tunnel
// protocol is unchanged — and, when it is held, prints it in its log and
// leaves it where `muster doctor` reads it (mirrorpath). The manager types the
// code shown on the machine they moved; a kept copy of the token, on another
// machine, draws its own code, which the manager never sees.
//
// 🔴 THE HUB KEEPS ONLY THE CODE'S HASH (Hash). The code is hashed
// the moment it is read off the request or the manager's form, and only the
// hash is kept, compared (in constant time), logged and audited (as
// Fingerprint).
// ---------------------------------------------------------------------------

// Header carries a muster-mirror process's attempt code on /mirror/connect,
// and on `muster doctor`'s GET /mirror/host.
const Header = "X-Muster-Attempt-Code"

// attemptAlphabet is Crockford's base32: no I, L, O or U, so a code read off
// a terminal and typed into the page is not misread (Normalize folds
// the look-alikes back).
const attemptAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// attemptGroups × attemptGroupLen characters: 12 × 5 bits = 60 bits, shown as
// XXXX-XXXX-XXXX.
const (
	attemptGroups   = 3
	attemptGroupLen = 4
	attemptLen      = attemptGroups * attemptGroupLen
)

// New is a fresh random attempt code in its typeable form, XXXX-XXXX-XXXX
// (60 bits from crypto/rand).
func New() string {
	var raw [attemptLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("attemptcode: no randomness for an attempt code: " + err.Error())
	}
	var b strings.Builder
	for i, c := range raw {
		if i > 0 && i%attemptGroupLen == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(attemptAlphabet[c&31]) // 256 is a multiple of 32: unbiased
	}
	return b.String()
}

// Normalize is code's canonical form — upper case, without the
// dashes and spaces a person types — and whether it is a well-formed code at
// all. The look-alikes Crockford folds (I and L to 1, O to 0) are folded.
func Normalize(code string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if r > 127 || !strings.ContainsRune(attemptAlphabet, r) {
			return "", false
		}
		b.WriteRune(r)
		if b.Len() > attemptLen {
			return "", false
		}
	}
	if b.Len() != attemptLen {
		return "", false
	}
	return b.String(), true
}

// Hash is the hash the hub keeps of a code (canonical or as
// typed), and false for a malformed one.
func Hash(code string) ([32]byte, bool) {
	n, ok := Normalize(code)
	if !ok {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte("muster attempt code v1\x00" + n)), true
}

// Fingerprint is the short hex of a code's hash that logs and audit rows
// carry: enough to match a log line to an audit row, never the code.
func Fingerprint(h [32]byte) string { return hex.EncodeToString(h[:8]) }
