package attemptcode

import (
	"encoding/hex"
	"regexp"
	"testing"
)

// TestACodeIsTypeableAndTypedFormsAgree: a fresh code is XXXX-XXXX-XXXX in
// Crockford base32; two fresh codes differ; the forms a person types (lower
// case, no dashes, spaces, the I/L/O look-alikes) hash as the code itself;
// anything else is malformed.
func TestACodeIsTypeableAndTypedFormsAgree(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)
	a, b := New(), New()
	if !shape.MatchString(a) || !shape.MatchString(b) || a == b {
		t.Fatalf("fresh codes %q, %q; want two different XXXX-XXXX-XXXX codes", a, b)
	}
	const code = "K1T0-ABCD-9XYZ"
	want, ok := Hash(code)
	if !ok {
		t.Fatal("a well-formed code did not hash")
	}
	for _, typed := range []string{"k1t0abcd9xyz", "K1T0 ABCD 9XYZ", "kIto-abcd-9xyz", "KLTO-ABCD-9XYZ"} {
		if got, ok := Hash(typed); !ok || got != want {
			t.Errorf("typed %q does not hash as %q", typed, code)
		}
	}
	for _, bad := range []string{"", "K1T0-ABCD-9XY", "K1T0-ABCD-9XYZ0", "K1T0-ABCD-9XYU", "K1T0-ABCD-9XY!", "Ｋ1T0-ABCD-9XYZ"} {
		if _, ok := Hash(bad); ok {
			t.Errorf("malformed %q hashed", bad)
		}
	}
	if other, _ := Hash("K1T0-ABCD-9XYY"); other == want {
		t.Fatal("two different codes hash alike")
	}
	if fp := Fingerprint(want); len(fp) != 16 {
		t.Fatalf("fingerprint %q; want 16 hex characters", fp)
	}
}

// TestTheFingerprintIsKeyed (#688 round 1, Z1): the fingerprint depends on
// the per-process key — another key gives another fingerprint for the same
// hash — and is not the unkeyed hash prefix a log reader could test guesses
// against offline. Within one key it is stable (a log line matches its
// audit row).
func TestTheFingerprintIsKeyed(t *testing.T) {
	h, _ := Hash("K1T0-ABCD-9XYZ")
	a, again := Fingerprint(h), Fingerprint(h)
	saved := fingerprintKey
	t.Cleanup(func() { fingerprintKey = saved })
	fingerprintKey = []byte("another key, not the process's own")
	b := Fingerprint(h)
	if a != again || a == b || a == hex.EncodeToString(h[:8]) || len(a) != 16 {
		t.Fatalf("fingerprints %q, %q (same key), %q (another key); want stable per key, different across keys, 16 hex, not the hash prefix", a, again, b)
	}
}
