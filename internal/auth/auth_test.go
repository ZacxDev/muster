package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	secret := []byte("s3cr3t-key")
	now := time.Unix(1_700_000_000, 0)
	val := SignSession(secret, now, 30*24*time.Hour)

	sess, err := VerifySession(secret, val, now)
	if err != nil {
		t.Fatalf("VerifySession: unexpected error %v", err)
	}
	if !sess.IssuedAt.Equal(now) {
		t.Fatalf("IssuedAt = %v, want %v", sess.IssuedAt, now)
	}
	if got, want := sess.Expiry, now.Add(30*24*time.Hour); !got.Equal(want) {
		t.Fatalf("Expiry = %v, want %v", got, want)
	}
}

func TestVerifyTamperedPayloadRejected(t *testing.T) {
	secret := []byte("s3cr3t-key")
	now := time.Unix(1_700_000_000, 0)
	val := SignSession(secret, now, time.Hour)

	// Flip the issuedAt field but keep the original signature.
	parts := strings.SplitN(val, "|", 3)
	tampered := "9999999999|" + parts[1] + "|" + parts[2]
	if _, err := VerifySession(secret, tampered, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyWrongSecretRejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	val := SignSession([]byte("right-secret"), now, time.Hour)
	if _, err := VerifySession([]byte("wrong-secret"), val, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyExpiredRejected(t *testing.T) {
	secret := []byte("s3cr3t-key")
	now := time.Unix(1_700_000_000, 0)
	val := SignSession(secret, now, time.Hour)
	// Two hours later → expired.
	later := now.Add(2 * time.Hour)
	if _, err := VerifySession(secret, val, later); !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestVerifyExpiryBoundaryIsExclusive(t *testing.T) {
	secret := []byte("k")
	now := time.Unix(1_700_000_000, 0)
	val := SignSession(secret, now, time.Hour)
	// Exactly at expiry must be rejected (now.Before(expiry) is false).
	if _, err := VerifySession(secret, val, now.Add(time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("at-expiry err = %v, want ErrExpired", err)
	}
	// One second before expiry must pass.
	if _, err := VerifySession(secret, val, now.Add(time.Hour-time.Second)); err != nil {
		t.Fatalf("just-before-expiry err = %v, want nil", err)
	}
}

func TestVerifyMalformedRejected(t *testing.T) {
	secret := []byte("k")
	now := time.Unix(1_700_000_000, 0)
	cases := []string{
		"",
		"only-one-field",
		"a|b",             // too few fields
		"a|b|c|d",         // too many fields
		"notanint|123|ab", // bad issuedAt
		"123|notanint|ab", // bad expiry
		"123|456|zz",      // bad hex signature
	}
	for _, c := range cases {
		if _, err := VerifySession(secret, c, now); !errors.Is(err, ErrMalformed) {
			t.Fatalf("VerifySession(%q) err = %v, want ErrMalformed", c, err)
		}
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("token-abc", "token-abc") {
		t.Fatal("equal tokens should match")
	}
	if ConstantTimeEqual("token-abc", "token-xyz") {
		t.Fatal("different tokens should not match")
	}
	if ConstantTimeEqual("", "") {
		t.Fatal("empty tokens must never match")
	}
	if ConstantTimeEqual("x", "") || ConstantTimeEqual("", "x") {
		t.Fatal("empty token must never authorize")
	}
	// Differing lengths must not match (subtle.ConstantTimeCompare returns 0).
	if ConstantTimeEqual("short", "longer-token") {
		t.Fatal("different-length tokens should not match")
	}
}

func TestRandomTokenUniqueAndURLSafe(t *testing.T) {
	a, err := RandomToken(32)
	if err != nil {
		t.Fatalf("RandomToken: %v", err)
	}
	b, err := RandomToken(32)
	if err != nil {
		t.Fatalf("RandomToken: %v", err)
	}
	if a == b {
		t.Fatal("two random tokens collided")
	}
	if a == "" {
		t.Fatal("empty token")
	}
	if strings.ContainsAny(a, "+/=") {
		t.Fatalf("token %q is not URL-safe", a)
	}
	// nBytes <= 0 falls back to a sane default rather than producing an empty token.
	if z, err := RandomToken(0); err != nil || z == "" {
		t.Fatalf("RandomToken(0) = %q, %v; want non-empty", z, err)
	}
}
