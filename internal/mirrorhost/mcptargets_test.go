package mirrorhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// targetToken is an invented mirror token, distinct from proxyToken, so a
// message that carried it is seen.
const targetToken = "mmt_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"

func writeTargetToken(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(targetToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeTargets(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "targets.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheTargetsFileIsValidatedWhole: a valid file loads (names, URLs and
// tokens as written); each rule refuses on its own — a bad name, a duplicate
// name, the primary's name, a token file readable by group or others, a
// symlinked token file, a file that holds no mirror token, http:// to a public
// address, an unknown field — and no refusal ever spells the token.
func TestTheTargetsFileIsValidatedWhole(t *testing.T) {
	dir := t.TempDir()
	tok := writeTargetToken(t, dir, "la.token", 0o600)
	tight := writeTargetToken(t, dir, "tight.token", 0o400)
	loose := writeTargetToken(t, dir, "loose.token", 0o640)
	link := filepath.Join(dir, "link.token")
	if err := os.Symlink(tok, link); err != nil {
		t.Fatal(err)
	}
	notTok := filepath.Join(dir, "not.token")
	if err := os.WriteFile(notTok, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	good := `[{"name":"la","url":"https://la.example/","token_file":"` + tok + `"},
	          {"name":"lab-2","url":"http://[fdaa:bbbb:cccc::5]:8080","token_file":"` + tight + `"},
	          {"name":"local","url":"http://127.0.0.1:9","token_file":"` + tok + `"}]`
	got, err := LoadMCPTargets(writeTargets(t, dir, good), "default")
	if err != nil {
		t.Fatalf("a valid file: %v", err)
	}
	if len(got) != 3 || got[0].Name != "la" || got[0].URL != "https://la.example" || got[0].Token != targetToken ||
		got[1].Name != "lab-2" || got[2].URL != "http://127.0.0.1:9" {
		t.Fatalf("loaded %+v", got)
	}

	entry := func(name, url, file string) string {
		return `{"name":"` + name + `","url":"` + url + `","token_file":"` + file + `"}`
	}
	for what, c := range map[string]struct{ body, want string }{
		"an upper-case name":        {"[" + entry("LA", "https://la.example", tok) + "]", "the name must match"},
		"a name with a dot":         {"[" + entry("la.prod", "https://la.example", tok) + "]", "the name must match"},
		"a 33-character name":       {"[" + entry(strings.Repeat("a", 33), "https://la.example", tok) + "]", "the name must match"},
		"an empty name":             {"[" + entry("", "https://la.example", tok) + "]", "the name must match"},
		"a duplicate name":          {"[" + entry("la", "https://a.example", tok) + "," + entry("la", "https://b.example", tok) + "]", "used twice"},
		"the primary's name":        {"[" + entry("default", "https://la.example", tok) + "]", "the primary's"},
		"a group-readable token":    {"[" + entry("la", "https://la.example", loose) + "]", "0600 or tighter"},
		"a symlinked token":         {"[" + entry("la", "https://la.example", link) + "]", "symlink"},
		"a file holding no token":   {"[" + entry("la", "https://la.example", notTok) + "]", "does not hold a mirror token"},
		"a missing token file":      {"[" + entry("la", "https://la.example", filepath.Join(dir, "nope")) + "]", "token_file"},
		"a relative token file":     {"[" + entry("la", "https://la.example", "la.token") + "]", "absolute path"},
		"http to a public address":  {"[" + entry("la", "http://192.0.2.7", tok) + "]", "public address"},
		"http to a public hostname": {"[" + entry("la", "http://la.example", tok) + "]", "public address"},
		"another scheme":            {"[" + entry("la", "ftp://la.example", tok) + "]", "https://"},
		"credentials in the url":    {"[" + entry("la", "https://u:p@la.example", tok) + "]", "plain base URL"},
		"an unknown field":          {`[{"name":"la","url":"https://la.example","token_file":"` + tok + `","token":"x"}]`, "not a JSON list"},
		"not a list":                {`{"name":"la"}`, "not a JSON list"},
		"two values":                {"[] []", "more than one JSON value"},
	} {
		_, err := LoadMCPTargets(writeTargets(t, dir, c.body), "default")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want an error containing %q", what, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), targetToken) {
			t.Errorf("%s: the error spells the token: %v", what, err)
		}
	}
	if _, err := LoadMCPTargets(writeTargets(t, dir, "[]"), "Bad Name"); err == nil || !strings.Contains(err.Error(), "primary's name") {
		t.Errorf("a bad primary name: %v", err)
	}
	// A custom primary name frees "default" for a target.
	if _, err := LoadMCPTargets(writeTargets(t, dir, "["+entry("default", "https://la.example", tok)+"]"), "home"); err != nil {
		t.Errorf("primary home, target default: %v", err)
	}
}
