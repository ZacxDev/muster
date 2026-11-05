package redact

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The Kubernetes env shape (round 2, Y1): a container's env list carries a
// credential as a `name:` / `value:` pair, so the credential NAME and the
// LITERAL sit on different lines and the key rule (which judges one line's
// key) never sees them together. HasSecretValueIn judges the pair; the
// literal rules are the key rule's, with the env list's own spelling of a
// boolean or a number ("true", "5432": an env value must be a string) still
// not a literal. Every value is synthetic, a long one assembled at run time.
// ---------------------------------------------------------------------------

var fakeEnvPass = "S0me" + "Passw0rd" + "Example"

// envDoc is a realistic Deployment with env entries in place of ENV.
func envDoc(env string) string {
	return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n      containers:\n" +
		"        - name: web\n          image: example/web:1.2.3\n          env:\n" + env +
		"          ports:\n            - containerPort: 8080\n"
}

func TestACredentialNamedEnvEntryWithALiteralValueIsASecret(t *testing.T) {
	for name, env := range map[string]string{
		"name then value":  "            - name: DB_PASSWORD\n              value: " + fakeEnvPass + "\n",
		"value then name":  "            - value: " + fakeEnvPass + "\n              name: DB_PASSWORD\n",
		"a short literal":  "            - name: API_TOKEN\n              value: hunter2\n",
		"a quoted literal": "            - name: \"REDIS_PASSWORD\"\n              value: \"plainword\"\n",
		"a flow mapping":   "            - {name: SMTP_PASSWORD, value: " + fakeEnvPass + "}\n",
		"a block scalar":   "            - name: CLIENT_SECRET\n              value: |\n                " + fakeEnvPass + "\n",
		"behind a harmless entry": "            - name: DB_HOST\n              value: db.example.internal\n" +
			"            - name: DB_PASSWORD\n              value: plainword\n",
	} {
		if !HasSecretValueIn("clusters/prod/apps/web/deploy.yaml", envDoc(env)) {
			t.Errorf("%s: HasSecretValueIn = false, want the literal under a credential name refused", name)
		}
	}
}

func TestAnEnvEntryThatHoldsNoLiteralIsNotASecret(t *testing.T) {
	for name, env := range map[string]string{
		"valueFrom secretKeyRef": "            - name: DB_PASSWORD\n              valueFrom:\n                secretKeyRef:\n" +
			"                  name: db-credentials\n                  key: password\n",
		"a SOPS value":         "            - name: DB_PASSWORD\n              value: ENC[AES256_GCM,data:Zm9v,iv:YmFy,tag:YmF6,type:str]\n",
		"a dependent variable": "            - name: DB_PASSWORD\n              value: $(PG_PASSWORD)\n",
		"a template":           "            - name: API_TOKEN\n              value: \"{{ .Values.token }}\"\n",
		"an empty value":       "            - name: API_TOKEN\n              value: \"\"\n",
		"a quoted boolean":     "            - name: REQUIRE_TOKEN\n              value: \"true\"\n",
		"a quoted number":      "            - name: SESSION_TOKEN\n              value: \"3600\"\n",
		"a bare number":        "            - name: SESSION_TOKEN\n              value: 3600\n",
		"a harmless name":      "            - name: DB_HOST\n              value: " + fakeEnvPass + "\n",
		"a *_FILE path name":   "            - name: DB_PASSWORD_FILE\n              value: /run/secrets/db\n",
		"the value is another item's": "            - name: DB_PASSWORD\n              valueFrom:\n                secretKeyRef: {name: db, key: password}\n" +
			"            - name: DB_HOST\n              value: plainword\n",
	} {
		if HasSecretValueIn("clusters/prod/apps/web/deploy.yaml", envDoc(env)) {
			t.Errorf("control %s: HasSecretValueIn = true, want it passed", name)
		}
	}
}

// TestCredentialPairJudgesOnlyAChangedPair: in a diff, the pair is judged when
// EITHER of its lines is changed (an officer that edits only the value line
// beneath an unchanged credential name is the common case); a pair whose two
// lines are both context is not the change's.
func TestCredentialPairJudgesOnlyAChangedPair(t *testing.T) {
	lines := strings.Split("            - name: DB_PASSWORD\n              value: plainword\n            - name: DB_HOST\n              value: db", "\n")
	for name, c := range map[string]struct {
		changed []bool
		want    bool
	}{
		"value line changed":   {[]bool{false, true, false, false}, true},
		"name line changed":    {[]bool{true, false, false, false}, true},
		"every line (nil)":     {nil, true},
		"only the other entry": {[]bool{false, false, true, true}, false},
		"nothing changed":      {[]bool{false, false, false, false}, false},
	} {
		if got := HasSecretPairIn("clusters/prod/apps/web/deploy.yaml", lines, c.changed); got != c.want {
			t.Errorf("%s: HasSecretPairIn = %v, want %v", name, got, c.want)
		}
	}
	// A source file's env-shaped text is code, not a manifest.
	if HasSecretPairIn("cmd/web/main.go", lines, nil) {
		t.Errorf("a .go file: HasSecretPairIn = true, want false")
	}
}
