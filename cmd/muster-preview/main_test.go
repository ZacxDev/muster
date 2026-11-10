package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/previewgw"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// fakeSelf answers GET /plugin/v1/self from a token → answer map.
func fakeSelf(t *testing.T, answers map[string]previewgw.Self) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s, ok := answers[tok]
		if r.URL.Path != "/plugin/v1/self" || !ok {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func viewer(id int64, login string) previewgw.Self {
	return previewgw.Self{PluginID: id, OwnerID: id, OwnerLogin: login, Capabilities: []string{"messages", "viewer"}}
}

func setup(t *testing.T, answers map[string]previewgw.Self, tokens ...string) map[string]string {
	dir := t.TempDir()
	tf := filepath.Join(dir, "plugin-tokens")
	if err := os.WriteFile(tf, []byte(strings.Join(tokens, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"MUSTER_URL": fakeSelf(t, answers).URL, "PLUGIN_TOKENS_FILE": tf, "STATE_DIR": state}
}

// `host-token create <login> <host>` resolves the login against the tokens'
// /self answers and keys the token on owner_id; an ambiguous login (two
// owners answering to it) or an unknown one is refused and writes nothing.
func TestHostTokenCreateRefusesAnAmbiguousLogin(t *testing.T) {
	answers := map[string]previewgw.Self{
		"mpl_a": viewer(1, "shared@example.invalid"),
		"mpl_b": viewer(2, "shared@example.invalid"),
		"mpl_c": viewer(3, "solo@example.invalid"),
	}
	e := setup(t, answers, "mpl_a", "mpl_b", "mpl_c")
	var out, errb bytes.Buffer
	if rc := run([]string{"host-token", "create", "shared@example.invalid", "alpha"}, env(e), &out, &errb); rc != 1 ||
		!strings.Contains(errb.String(), "ambiguous") {
		t.Fatalf("ambiguous login: rc %d, stderr %q", rc, errb.String())
	}
	errb.Reset()
	if rc := run([]string{"host-token", "create", "nobody@example.invalid", "alpha"}, env(e), &out, &errb); rc != 1 {
		t.Fatalf("unknown login: rc %d", rc)
	}
	errb.Reset()
	if rc := run([]string{"host-token", "create", "alpha"}, env(e), &out, &errb); rc != 1 || !strings.Contains(errb.String(), "name the owner") {
		t.Fatalf("no owner with three tokens: rc %d, %q", rc, errb.String())
	}
	if l, _ := (previewgw.HostTokens{Dir: e["STATE_DIR"]}).List(); len(l) != 0 {
		t.Fatalf("a refused create stored %+v", l)
	}
	out.Reset()
	if rc := run([]string{"host-token", "create", "solo@example.invalid", "alpha"}, env(e), &out, &errb); rc != 0 {
		t.Fatalf("unique login: rc %d, %q", rc, errb.String())
	}
	tok := strings.SplitN(out.String(), "\n", 2)[0]
	if !previewgw.TokenShapeOK(tok) {
		t.Fatalf("printed token %q is not a host token", tok)
	}
	l, _ := previewgw.HostTokens{Dir: e["STATE_DIR"]}.List()
	if len(l) != 1 || l[0].OwnerID != 3 || l[0].Host != "alpha" || l[0].Hash != previewgw.HashToken(tok) {
		t.Fatalf("stored %+v", l)
	}
}

// With exactly one token, <owner> may be left out; ls lists the token and
// revoke removes it.
func TestHostTokenCreateDefaultsToTheOnlyOwnerAndLsRevoke(t *testing.T) {
	e := setup(t, map[string]previewgw.Self{"mpl_a": viewer(7, "me@example.invalid")}, "mpl_a")
	var out, errb bytes.Buffer
	if rc := run([]string{"host-token", "create", "laptop"}, env(e), &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	out.Reset()
	if rc := run([]string{"host-token", "ls"}, env(e), &out, &errb); rc != 0 {
		t.Fatalf("ls rc %d", rc)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "me@example.invalid") || !strings.Contains(lines[1], "laptop") {
		t.Fatalf("ls:\n%s", out.String())
	}
	id := strings.Fields(lines[1])[0]
	out.Reset()
	if rc := run([]string{"host-token", "revoke", id}, env(e), &out, &errb); rc != 0 || !strings.Contains(out.String(), "30s") {
		t.Fatalf("revoke rc %d: %s / %s", rc, out.String(), errb.String())
	}
	if l, _ := (previewgw.HostTokens{Dir: e["STATE_DIR"]}).List(); len(l) != 0 {
		t.Fatalf("after revoke: %+v", l)
	}
}

func TestVersionAndBadConfig(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := run([]string{"version"}, env(nil), &out, &errb); rc != 0 || !strings.HasPrefix(out.String(), "muster-preview ") {
		t.Fatalf("version: rc %d, %q", rc, out.String())
	}
	if rc := run([]string{"serve"}, env(nil), &out, &errb); rc != 2 || !strings.Contains(errb.String(), "MUSTER_URL") {
		t.Fatalf("serve without config: rc %d, %q", rc, errb.String())
	}
	if rc := run([]string{"bogus"}, env(nil), &out, &errb); rc != 1 {
		t.Fatalf("unknown command: rc %d", rc)
	}
}
