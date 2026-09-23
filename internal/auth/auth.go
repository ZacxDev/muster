// Package auth provides the cryptographic primitives that lock down this
// service's two distinct surfaces:
//
//   - The human web UI is gated by a signed session cookie. A cookie value is an
//     HMAC-SHA256-signed payload of the form "issuedAt|expiry"; the signature is
//     keyed by a server-held secret so a client cannot forge or tamper with it.
//   - The machine hook endpoint is gated by a bearer token compared in constant
//     time (see ConstantTimeEqual).
//
// Tokens and the session secret are random, URL-safe, and generated with
// crypto/rand (see RandomToken).
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Errors returned by VerifySession. They are intentionally coarse: a caller
// should treat any of them as "no valid session" and never surface the reason to
// an unauthenticated client.
var (
	// ErrMalformed indicates the cookie value is not a well-formed signed token
	// (wrong number of fields, non-numeric timestamps, bad base64, …).
	ErrMalformed = errors.New("auth: malformed session token")
	// ErrBadSignature indicates the HMAC did not verify: the payload was tampered
	// with or signed under a different secret.
	ErrBadSignature = errors.New("auth: invalid session signature")
	// ErrExpired indicates the session's expiry timestamp is in the past.
	ErrExpired = errors.New("auth: session expired")
)

// Session is the decoded, verified payload carried by a session cookie.
type Session struct {
	IssuedAt time.Time
	Expiry   time.Time
}

// SignSession returns the cookie value for a session that was issued at now and
// expires after ttl. The value is "<issuedAtUnix>|<expiryUnix>|<hexSignature>"
// where the signature is HMAC-SHA256 over "<issuedAtUnix>|<expiryUnix>".
func SignSession(secret []byte, now time.Time, ttl time.Duration) string {
	issued := now.Unix()
	expiry := now.Add(ttl).Unix()
	payload := strconv.FormatInt(issued, 10) + "|" + strconv.FormatInt(expiry, 10)
	sig := sign(secret, payload)
	return payload + "|" + hex.EncodeToString(sig)
}

// VerifySession parses and validates a cookie value produced by SignSession. It
// checks the HMAC (in constant time) before checking expiry, and returns the
// decoded Session on success. now is the reference time for the expiry check.
func VerifySession(secret []byte, value string, now time.Time) (Session, error) {
	parts := strings.Split(value, "|")
	if len(parts) != 3 {
		return Session{}, ErrMalformed
	}
	issuedUnix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Session{}, ErrMalformed
	}
	expiryUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Session{}, ErrMalformed
	}
	gotSig, err := hex.DecodeString(parts[2])
	if err != nil {
		return Session{}, ErrMalformed
	}

	payload := parts[0] + "|" + parts[1]
	wantSig := sign(secret, payload)
	if !hmac.Equal(gotSig, wantSig) {
		return Session{}, ErrBadSignature
	}

	expiry := time.Unix(expiryUnix, 0)
	if !now.Before(expiry) {
		return Session{}, ErrExpired
	}
	return Session{IssuedAt: time.Unix(issuedUnix, 0), Expiry: expiry}, nil
}

// sign computes HMAC-SHA256 of payload keyed by secret.
func sign(secret []byte, payload string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// ConstantTimeEqual reports whether a and b are equal using a constant-time
// comparison, so a timing side-channel cannot be used to recover a secret token
// byte by byte. Empty strings never match (a missing token must not authorize).
func ConstantTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RandomToken returns a cryptographically-random, URL-safe token with nBytes of
// entropy (base64-raw-url encoded, so the string is longer than nBytes). It is
// used to mint the human auth token, the hook token, and the session secret.
func RandomToken(nBytes int) (string, error) {
	if nBytes <= 0 {
		nBytes = 32
	}
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
