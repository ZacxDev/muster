package redact

import "testing"

// TestHasSecretAgreesWithSecrets: HasSecret is true exactly where Secrets
// would rewrite — every shape of the second batch (a secret) and its
// look-alikes (none) — so a refusal and a redaction cannot disagree.
func TestHasSecretAgreesWithSecrets(t *testing.T) {
	secrets, clean := 0, 0
	for _, c := range batch2Cases() {
		want := c.in != c.want
		if got := HasSecret(c.in); got != want {
			t.Errorf("%s: HasSecret(%q) = %v, want %v", c.name, c.in, got, want)
		}
		if want {
			secrets++
		} else {
			clean++
		}
	}
	// The table must hold both kinds, or the agreement above is half-checked.
	if secrets == 0 || clean == 0 {
		t.Fatalf("batch2Cases has %d secret and %d clean cases; want both", secrets, clean)
	}
}
