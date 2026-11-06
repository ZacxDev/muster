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

// keyLiterals hold a LITERAL string under a credential-like KEY: refused
// whatever the value looks like (short, letters only, a passphrase, a
// resource-style name), in YAML, JSON, an assignment and a flag.
var keyLiterals = []string{
	"password: hunter2",
	"  password: correct-horse-battery-staple",
	"password: SuperSecretPassword",
	"  secret: app-tls",
	"secret: ClusterSecretRef # see the store",
	"client_secret: abc",
	"api_key: 'k1'",
	"private_key: xyz",
	"credentials: admin:admin",
	"passphrase: open sesame",
	`{"password": "hunter2"}`,
	`  "token": "abc"`,
	"DB_PASSWORD=hunter2",
	"export ADMIN_TOKEN='abc'",
	"--password hunter2",
	"--token=abc",
	"password: |",
}

// keyNonLiterals are credential-like keys holding NO literal: a boolean, a
// number, an empty or null value, a reference, an encrypted value, a path, or
// a key that itself names a reference. Each passes.
var keyNonLiterals = []string{
	"automountServiceAccountToken: false",
	"existingSecret: x",
	"passwordSecret: x",
	"password: ${DB_PASSWORD}",
	"  token: 3600",
	"  token: 3600s",
	"password: \"\"",
	"secret: ~",
	"secretKeyRef: db-credentials",
	"existingSecretPasswordKey: admin-password",
	"tlsSecretName: app-tls",
	"credentialsSecretRef: cloud",
	"password: ENC[AES256_GCM,data:Zm9v,iv:YmFy,tag:YmF6,type:str]",
	"tokenFile: /var/run/secrets/token",
	"credentials:",
	"--password=$(cat /run/pw)",
}

// TestTheKeyRuleRefusesEveryLiteralUnderACredentialKey (Y1): each literal is
// refused, each non-literal passes.
func TestTheKeyRuleRefusesEveryLiteralUnderACredentialKey(t *testing.T) {
	for _, l := range keyLiterals {
		if !HasSecretValue(l) {
			t.Errorf("HasSecretValue(%q) = false: a literal under a credential key", l)
		}
	}
	for _, l := range keyNonLiterals {
		if HasSecretValue(l) {
			t.Errorf("HasSecretValue(%q) = true: no literal under the key", l)
		}
	}
}

// TestTheKeyRuleInSourceCodeNeedsAQuotedLiteral: in a source file an
// unquoted value is an expression (Password: cfg.Password), so only a quoted
// string literal under a credential key is refused there; the same unquoted
// line in a YAML file is a literal (control).
func TestTheKeyRuleInSourceCodeNeedsAQuotedLiteral(t *testing.T) {
	for _, c := range []struct {
		path, line string
		want       bool
	}{
		{"internal/db/config.go", "\tPassword: cfg.Password,", false},
		{"internal/db/config.go", "\tToken:    tok,", false},
		{"internal/db/config.go", "\tPassword: \"hunter2\",", true},
		{"web/src/api.ts", "  apiKey: process.env.API_KEY,", false},
		{"web/src/api.ts", "  apiKey: 'k1',", true},
		{"values.yaml", "Password: cfg.Password", true},
	} {
		if got := HasSecretValueIn(c.path, c.line); got != c.want {
			t.Errorf("HasSecretValueIn(%q, %q) = %v, want %v", c.path, c.line, got, c.want)
		}
	}
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
