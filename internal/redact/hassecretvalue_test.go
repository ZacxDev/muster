package redact

import "testing"

// ---------------------------------------------------------------------------
// HasSecretValue: THE secret-shape test for an officer's SHIP content (its
// pull request diff, title, body and comments; internal/officership's draft
// gate). The transcript redactor (Secrets) refuses a credential-NAMED key
// whatever its value, which is right for a transcript and wrong for a
// manifest: `automountServiceAccountToken: false` names a token and holds a
// boolean. HasSecretValue needs the VALUE to be secret-shaped.
//
// Every value below is synthetic. A long literal is assembled at run time so
// no single source line is one (tests/leakscan.py scans lines).
// ---------------------------------------------------------------------------

var (
	fakeLong40 = "FAKE7qL2mN5vR7tY1wK4" + "pB9xZ3cH6jD8sG0fA2eQ" // 40 characters, mixed case and digits
	fakeHex40  = "a94a8fe5ccb19ba61c4c" + "0873d391e987982fbbd3" // a lowercase hex token
	fakeB64    = "RkFLRWZha2VGQUtFZmFr" + "ZTAwMDBleGFtcGxl"     // base64 of a fake
	fakeUUID   = "3f2a9c1e-8b4d-4e2f-" + "9a1c-7d6e5f4a3b2c"
)

// manifestLines are ordinary lines of Kubernetes manifests, Helm values,
// kustomizations and shell that NAME a credential without holding one. Every
// one must pass HasSecretValue.
var manifestLines = []string{
	"automountServiceAccountToken: false",
	"  automountServiceAccountToken: true",
	"existingSecret: x",
	"existingSecret: my-release-postgresql",
	"passwordSecret: db-credentials",
	"  passwordSecret: x",
	"  secret: app-tls",
	"    token: \"\"",
	"apiKey: ${API_KEY}",
	"password: '${DB_PASSWORD}'",
	"  token: \"{{ .Values.token }}\"",
	"password: {valueFrom: {secretKeyRef: {name: db, key: password}}}",
	"secret: null",
	"  passwordKey: DB_PASSWORD",
	"tokenSecret: ADMIN_TOKEN_REF",
	"  token: 3600s",
	"secret: /var/run/secrets/kubernetes.io/serviceaccount/token",
	"  - --api-key=$(API_KEY)",
	"export GITHUB_TOKEN=\"${GITHUB_TOKEN:-}\"",
	"DB_PASSWORD=$(cat /run/secrets/db)",
	`{"password": "${PASSWORD}", "user": "app"}`,
	`  "token": ""`,
	"url: postgres://app:${DB_PASSWORD}@db:5432/app",
	"the password is required",
	"secret: ClusterSecretRef # see the store",
	"    secretName: tls-2024-renewal",
	"--token \"$TOKEN\"",
}

// secretLines hold a real-shaped secret VALUE. Every one must be refused.
var secretLines = []string{
	"password: " + fakeLong40,
	"  token: " + fakeHex40,
	"  password: " + fakeB64,
	"apiKey: \"" + fakeLong40 + "\"",
	"client_secret: '" + fakeUUID + "'",
	`{"api_key": "` + fakeLong40 + `"}`,
	"export DB_PASSWORD=" + fakeLong40,
	"--token " + fakeHex40,
	"url: postgres://app:" + "FAKEpass0000" + "@db:5432/app",
	"stripe: sk_live_" + "FAKE0000example",
	"key: " + pemBegin + "\nFAKE\n" + pemEnd,
	"aws_secret_access_key: " + awsFake,
	"value: eyJFAKEhdr0.eyJFAKEbody0.FAKEsignature0",
	"x: ghp_" + "FAKE0000example0000",
}

// TestHasSecretValueAdmitsManifestKeysAndRefusesSecretValues: every
// manifest line passes, every secret line is refused.
func TestHasSecretValueAdmitsManifestKeysAndRefusesSecretValues(t *testing.T) {
	for _, l := range manifestLines {
		if HasSecretValue(l) {
			t.Errorf("HasSecretValue(%q) = true: an ordinary manifest line, no secret value", l)
		}
	}
	for _, l := range secretLines {
		if !HasSecretValue(l) {
			t.Errorf("HasSecretValue(%q) = false: a secret-shaped value", l)
		}
	}
}

// TestTheTranscriptRedactorIsUnchanged (an INVARIANT guard: the transcript
// path never had the value rule): Secrets still redacts a credential-named
// key whatever its value, so a transcript line naming one is still masked.
func TestTheTranscriptRedactorIsUnchanged(t *testing.T) {
	for in, want := range map[string]string{
		"automountServiceAccountToken: false": "automountServiceAccountToken: " + Mark,
		"existingSecret: x":                   "existingSecret: " + Mark,
		"passwordSecret: x":                   "passwordSecret: " + Mark,
		"apiKey: ${API_KEY}":                  "apiKey: " + Mark,
	} {
		if got := Secrets(in); got != want {
			t.Errorf("Secrets(%q) = %q, want %q", in, got, want)
		}
	}
}
