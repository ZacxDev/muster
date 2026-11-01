package officership

import (
	"strings"
	"testing"
)

// TestRedactWithholdsMoreCredentialShapes: the provider token shapes the
// filter knows by their PREFIX, each in a line no key=value or env rule would
// catch, so only the shape can withhold it. Every fixture is built at run
// time: a token-shaped literal in the source trips the repository's leak
// gate.
func TestRedactWithholdsMoreCredentialShapes(t *testing.T) {
	r := strings.Repeat
	for name, secret := range map[string]string{
		"stripe live":     "sk_live_" + r("x9Y8", 6),
		"stripe test":     "sk_test_" + r("x9Y8", 6),
		"stripe restrict": "rk_live_" + r("x9Y8", 6),
		"google":          "AIza" + "Sy" + r("aB3-", 8) + "c",
		"gitlab":          "glpat-" + r("Ab1_", 5),
		"hugging face":    "hf_" + r("Ab1c", 8),
		"aws session":     "ASIA" + r("QWER", 4),
		"npm":             "npm_" + r("Ab1C", 9),
		"pypi":            "pypi-" + r("AgEI", 14),
		"openai project":  "sk-proj-" + r("Ab1c", 6),
		"anthropic":       "sk-ant-" + r("Ab1c", 6),
		"slack":           "xoxb-" + r("1234-", 4),
	} {
		line := "the request went out with " + secret + " attached"
		if got := Redact(line); strings.Contains(got, secret) || got != withheld {
			t.Errorf("%s: %q -> %q, want it withheld", name, line, got)
		}
	}
	for name, line := range map[string]string{
		"kubeconfig client key":  "    client-key-data: " + r("TFMw", 12),
		"kubeconfig client cert": "    client-certificate-data: " + r("TFMw", 12),
		"aws secret key":         "aws_secret_access_key = " + r("Ab1/", 10),
	} {
		if got := Redact(line); got != withheld {
			t.Errorf("%s: %q -> %q, want it withheld", name, line, got)
		}
	}
	// POSITIVE CONTROLS: ordinary lines naming the same words pass unchanged.
	for _, l := range []string{
		"the token was refreshed at 12:00",
		"rotate the API key monthly",
		"read /etc/ssl/private/server.key",
		"key: value pairs are parsed in order",
		"client-key-data is set in the kubeconfig",
		"npm_config_cache points at the shared cache",
		"hf_hub_download fetched the weights",
		"AIzawa is not a key",
		"see sk_live_ prefixes in the docs",
	} {
		if got := Redact(l); got != l {
			t.Errorf("an ordinary line was changed: %q -> %q", l, got)
		}
	}
}

// TestRedactCutTailWithholdsATokenPrefix: a field capped with "…" can end in
// the PREFIX of a token, shorter than any full-token pattern. That last line
// is withheld; an ordinary capped line is not.
func TestRedactCutTailWithholdsATokenPrefix(t *testing.T) {
	cut := "ghp_" + strings.Repeat("A1b2", 3)
	if got := RedactCutTail("ok\nclone with "+cut+"…", nil); strings.Contains(got, cut) || got != "ok\n"+withheld {
		t.Fatalf("a cut token prefix: %q", got)
	}
	tok := "opaque-" + strings.Repeat("9z", 10)
	if got := RedactCutTail("the value was "+tok[:9]+"…", []string{tok}); strings.Contains(got, tok[:9]) {
		t.Fatalf("a cut known token: %q", got)
	}
	for _, l := range []string{"an ordinary long output, cut…", "no cut at all ghp_", "skipped … in the middle"} {
		if got := RedactCutTail(l, []string{tok}); got != l {
			t.Errorf("an ordinary line was changed: %q -> %q", l, got)
		}
	}
}
