package redact

import "testing"

// ---------------------------------------------------------------------------
// Round 4: the env credential pair outside a dash-led block item (a JSON6902
// add, a flow list, a commented quoted name), and AWS's secret access key as a
// credential name. Every value is synthetic.
// ---------------------------------------------------------------------------

const r4Manifest = "clusters/prod/apps/web/env-patch.yaml"

func TestAnEnvPairOutsideABlockItemIsASecret(t *testing.T) {
	for name, doc := range map[string]string{
		"json6902 nested map": "- op: add\n  path: /spec/template/spec/containers/0/env/-\n  value:\n" +
			"    name: DB_PASSWORD\n    value: plainword\n",
		"json6902 flow map": "- op: add\n  path: /spec/template/spec/containers/0/env/-\n" +
			"  value: {name: DB_PASSWORD, value: plainword}\n",
		"a flow list":              "env: [{name: X_TOKEN, value: \"plainword\"}]\n",
		"a commented quoted name":  "- name: \"DB_PASSWORD\" # the database\n  value: plainword\n",
		"a commented quoted value": "- name: DB_PASSWORD\n  value: \"plainword\" # set by hand\n",
		"aws secret access key":    "- name: AWS_SECRET_ACCESS_KEY\n  value: plainword\n",
	} {
		if !HasSecretValueIn(r4Manifest, doc) {
			t.Errorf("%s: HasSecretValueIn = false, want refused", name)
		}
	}
	for name, doc := range map[string]string{
		"json6902 valueFrom": "- op: add\n  path: /spec/template/spec/containers/0/env/-\n  value:\n" +
			"    name: DB_PASSWORD\n    valueFrom:\n      secretKeyRef: {name: db, key: password}\n",
		"a flow list across items": "env: [{name: X_TOKEN, valueFrom: x}, {name: DB_HOST, value: plainword}]\n",
		"a quoted # in a value":    "- name: DB_HOST\n  value: \"db # primary\"\n",
		"aws access key id":        "- name: AWS_ACCESS_KEY_ID\n  value: plainword\n",
		"a mapping per document":   "name: DB_PASSWORD\n---\nvalue: plainword\n",
	} {
		if HasSecretValueIn(r4Manifest, doc) {
			t.Errorf("control %s: HasSecretValueIn = true, want passed", name)
		}
	}
}

// TestTheSecretAccessKeyIsACredentialKey (🟢2): AWS_SECRET_ACCESS_KEY (and any
// *_SECRET_ACCESS_KEY) holding a literal refuses on the key rule; the access
// key ID is an identifier, not a secret (control).
func TestTheSecretAccessKeyIsACredentialKey(t *testing.T) {
	for _, l := range []string{"AWS_SECRET_ACCESS_KEY=plainword", "minio_secret_access_key: plainword", "S3_SECRET_ACCESS_KEY: \"plainword\""} {
		if !HasSecretValueIn("deploy/.env", l) {
			t.Errorf("HasSecretValueIn(%q) = false, want refused", l)
		}
	}
	for _, l := range []string{"AWS_ACCESS_KEY_ID=plainword", "aws_access_key_id: plainword"} {
		if HasSecretValueIn("deploy/.env", l) {
			t.Errorf("control HasSecretValueIn(%q) = true, want passed", l)
		}
	}
}
