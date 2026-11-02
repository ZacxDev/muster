package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The HQ MCP server in the chief's pod (hqmcp.go). The fixture host and token
// are pairwise distinct and match no constant the code names.
const fakeHQHost = "hq.fixture-host.test"

// hqMuster answers GET /agent/connectors/hq with a scripted view, and records
// every request (a POST would be ccd ASKING for access, which it never does).
type hqMuster struct {
	srv    *httptest.Server
	mu     sync.Mutex
	view   map[string]any
	paths  []string
	posted bool
}

func newHQMuster(t *testing.T) *hqMuster {
	m := &hqMuster{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.paths = append(m.paths, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodGet {
			m.posted = true
		}
		if r.Header.Get("X-Muster-Token") != fakeAgentToken || r.URL.Path != "/agent/connectors/hq" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(m.view)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *hqMuster) set(exists bool, grant string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.view = map[string]any{"name": "hq", "exists": exists, "grant": grant}
	if exists {
		m.view["hosts"] = []string{fakeHQHost}
	}
}

func (m *hqMuster) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.paths...)
}

// stubHQBinary makes hqLookPath find (or not find) the hq CLI.
func stubHQBinary(t *testing.T, present bool) {
	t.Helper()
	prev := hqLookPath
	hqLookPath = func(string) (string, error) {
		if present {
			return "/usr/local/bin/hq", nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { hqLookPath = prev })
}

// hqSeedDir is a config dir holding an operator's own server, and the image's
// settings template.
func hqSeedDir(t *testing.T, hq string) seedOpts {
	t.Helper()
	dir := t.TempDir()
	o := seedOpts{configDir: filepath.Join(dir, "claude"), workspace: filepath.Join(dir, "ws"),
		settingsTemplate: imageSettingsTemplate, hqMCP: hq}
	if err := os.MkdirAll(o.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.configDir, ".claude.json"),
		[]byte(`{"mcpServers":{"other":{"type":"http","url":"https://mcp.example.com"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return o
}

func readServers(t *testing.T, o seedOpts) map[string]any {
	t.Helper()
	st, err := readJSONObject(filepath.Join(o.configDir, ".claude.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	servers, _ := st["mcpServers"].(map[string]any)
	if servers["other"] == nil {
		t.Fatalf("an operator's server was dropped: %v", servers)
	}
	return servers
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSeedRegistersTheHQServerByTheConnectorsGrant: for the chief (CCD_HQ_MCP=1)
// with the connector store offered and the hq CLI present, seed registers `hq`
// — exactly a stdio `hq mcp` whose HQ_URL is the `hq` connector's prefix on
// muster's proxy and whose HQ_TOKEN is the agent's OWN muster token — while the
// connector exists and the grant is approved, pending or not yet filed, and
// REMOVES it (an entry the previous start wrote) once the grant is denied or
// revoked or the connector is gone. checkSeed agrees in every state, the
// operator's own server survives, and ccd never asks for access.
func TestSeedRegistersTheHQServerByTheConnectorsGrant(t *testing.T) {
	stubHQBinary(t, true)
	m := newHQMuster(t)
	t.Setenv(envConnectURL, m.srv.URL+"/")
	t.Setenv(envAgentToken, fakeAgentToken)
	o := hqSeedDir(t, "1")
	wantEntry := `{"args":["mcp"],"command":"hq","env":{"HQ_TOKEN":"` + fakeAgentToken + `","HQ_URL":"` +
		m.srv.URL + `/connect/hq/` + fakeHQHost + `"},"type":"stdio"}`

	for _, c := range []struct {
		exists     bool
		grant      string
		registered bool
	}{
		{true, "approved", true},
		{true, "denied", false},
		{true, "none", true}, // re-registered after the removal above
		{true, "revoked", false},
		{true, "pending", true},
		{false, "none", false},
		{true, "approved", true},
	} {
		m.set(c.exists, c.grant)
		var stderr bytes.Buffer
		if err := seed(o, &stderr); err != nil {
			t.Fatal(err)
		}
		servers := readServers(t, o)
		e, ok := servers["hq"]
		if ok != c.registered {
			t.Fatalf("exists=%v grant=%s: hq registered=%v, want %v (stderr %s)", c.exists, c.grant, ok, c.registered, stderr.String())
		}
		if ok && mustJSON(t, e) != wantEntry {
			t.Fatalf("exists=%v grant=%s: hq entry\n got %s\nwant %s", c.exists, c.grant, mustJSON(t, e), wantEntry)
		}
		if _, ok := servers["muster"]; !ok {
			t.Fatal("muster's own server is gone")
		}
		if err := checkSeed(o); err != nil {
			t.Fatalf("exists=%v grant=%s: checkSeed: %v", c.exists, c.grant, err)
		}
	}
	m.mu.Lock()
	posted := m.posted
	m.mu.Unlock()
	if posted {
		t.Fatal("seed POSTed to muster: it must never ask for access")
	}
	// A count, so the loop cannot have read nothing: one GET per seed.
	if calls := m.calls(); len(calls) != 7 || calls[0] != "GET /agent/connectors/hq" {
		t.Fatalf("muster saw %v, want 7 × GET /agent/connectors/hq", calls)
	}
}

// TestSeedOffersTheHQServerOnlyToTheChiefWithTheStoreAndTheCLI: without
// CCD_HQ_MCP (every agent but the chief), without the connector store, or
// without the hq CLI in the image, an entry seed wrote before is removed and
// muster is not asked — whatever the grant says. An operator's own server
// NAMED hq (not one seed wrote) is never touched.
func TestSeedOffersTheHQServerOnlyToTheChiefWithTheStoreAndTheCLI(t *testing.T) {
	for _, c := range []struct {
		name          string
		hq            string
		store, binary bool
	}{
		{"not offered (a worker)", "", true, true},
		{"no connector store", "1", false, true},
		{"no hq CLI", "1", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newHQMuster(t)
			m.set(true, "approved")
			t.Setenv(envConnectURL, m.srv.URL)
			t.Setenv(envAgentToken, fakeAgentToken)
			// Positive control: registered first, as the chief with everything.
			stubHQBinary(t, true)
			o := hqSeedDir(t, "1")
			if err := seed(o, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if _, ok := readServers(t, o)["hq"]; !ok {
				t.Fatal("control: the chief with the store and the CLI got no hq server")
			}
			before := len(m.calls())

			o.hqMCP = c.hq
			if !c.store {
				t.Setenv(envConnectURL, "")
			}
			stubHQBinary(t, c.binary)
			var stderr bytes.Buffer
			if err := seed(o, &stderr); err != nil {
				t.Fatal(err)
			}
			if _, ok := readServers(t, o)["hq"]; ok {
				t.Fatalf("hq is still registered (stderr %s)", stderr.String())
			}
			if !strings.Contains(stderr.String(), "removed the HQ MCP server") {
				t.Errorf("seed did not say it removed the server: %s", stderr.String())
			}
			if n := len(m.calls()) - before; n != 0 {
				t.Errorf("muster was asked %d time(s) for a pod that cannot use the server", n)
			}
			if err := checkSeed(o); err != nil {
				t.Fatalf("checkSeed: %v", err)
			}

			// An operator's own `hq` server survives, and checkSeed ignores it.
			p := filepath.Join(o.configDir, ".claude.json")
			st, _ := readJSONObject(p, nil)
			st["mcpServers"].(map[string]any)["hq"] = map[string]any{"type": "http", "url": "https://hq-mcp.example.com"}
			if err := writeJSONAtomic(p, st); err != nil {
				t.Fatal(err)
			}
			if err := seed(o, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if e, _ := readServers(t, o)["hq"].(map[string]any); e["url"] != "https://hq-mcp.example.com" {
				t.Fatalf("an operator's own hq server was changed: %v", e)
			}
			if err := checkSeed(o); err != nil {
				t.Fatalf("checkSeed with an operator's own hq server: %v", err)
			}
		})
	}
}

// TestSeedLeavesTheHQServerAsItWasWhenMusterCannotBeRead: a failed read of the
// connector (muster down, an error answer) changes nothing, in either
// direction, and never fails the pod's start.
func TestSeedLeavesTheHQServerAsItWasWhenMusterCannotBeRead(t *testing.T) {
	stubHQBinary(t, true)
	m := newHQMuster(t)
	m.set(true, "approved")
	t.Setenv(envConnectURL, m.srv.URL)
	t.Setenv(envAgentToken, fakeAgentToken)
	o := hqSeedDir(t, "1")
	if err := seed(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readServers(t, o)["hq"]; !ok {
		t.Fatal("control: not registered with the connector readable")
	}

	// The pod restarts with a NEW token and connect URL, and muster cannot be
	// read: the entry is kept (the decision) but carries THIS pod's URL and
	// token, so checkSeed agrees with it.
	const newToken = "a-rotated-agent-token-55e0"
	down := "http://127.0.0.1:9"
	t.Setenv(envAgentToken, newToken)
	t.Setenv(envConnectURL, down)
	var stderr bytes.Buffer
	if err := seed(o, &stderr); err != nil {
		t.Fatalf("an unreadable connector failed the start: %v", err)
	}
	want := `{"args":["mcp"],"command":"hq","env":{"HQ_TOKEN":"` + newToken + `","HQ_URL":"` +
		down + `/connect/hq/` + fakeHQHost + `"},"type":"stdio"}`
	if got := mustJSON(t, readServers(t, o)["hq"]); got != want {
		t.Fatalf("the kept entry on a failed read:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(stderr.String(), "left as it was") {
		t.Errorf("seed did not say why it left the entry: %s", stderr.String())
	}
	if err := checkSeed(o); err != nil {
		t.Fatalf("checkSeed after a failed read: %v", err)
	}

	// And an ABSENT entry stays absent.
	o2 := hqSeedDir(t, "1")
	if err := seed(o2, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readServers(t, o2)["hq"]; ok {
		t.Fatal("a failed read registered the server")
	}
}

// TestCheckSeedRefusesAStaleOrAlteredHQEntry: checkSeed fails on an entry seed
// wrote that this pod is no longer offered, and on one whose token or URL is not
// the one seed would write now.
func TestCheckSeedRefusesAStaleOrAlteredHQEntry(t *testing.T) {
	stubHQBinary(t, true)
	m := newHQMuster(t)
	m.set(true, "approved")
	t.Setenv(envConnectURL, m.srv.URL)
	t.Setenv(envAgentToken, fakeAgentToken)
	o := hqSeedDir(t, "1")
	if err := seed(o, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := checkSeed(o); err != nil {
		t.Fatalf("control: %v", err)
	}
	off := o
	off.hqMCP = ""
	// seedSettings would differ too (no HQ allow entry); the .claude.json check
	// runs first, so its error is the one asserted.
	if err := checkSeed(off); err == nil || !strings.Contains(err.Error(), "HQ MCP server is registered but this pod is not offered it") {
		t.Errorf("check with the server no longer offered: %v", err)
	}
	t.Setenv(envAgentToken, "another-agent-token-entirely")
	if err := checkSeed(o); err == nil || !strings.Contains(err.Error(), "differs from the one seed writes") {
		t.Errorf("check with another token: %v", err)
	}
	t.Setenv(envAgentToken, fakeAgentToken)
	t.Setenv(envConnectURL, "http://another-muster.example.test:1")
	if err := checkSeed(o); err == nil || !strings.Contains(err.Error(), "not this pod's proxy prefix") {
		t.Errorf("check with another connect URL: %v", err)
	}
}

// TestTheChiefsSettingsAllowEveryHQTool: with CCD_HQ_MCP=1 settings.json's
// permissions.allow carries exactly "mcp__hq__*" (every HQ tool, the operator's
// decision) after any muster entries; without it there is no HQ entry at all.
// checkSeed agrees with each.
func TestTheChiefsSettingsAllowEveryHQTool(t *testing.T) {
	stubHQBinary(t, false)
	t.Setenv(envConnectURL, "")
	for _, c := range []struct {
		hq, muster string
		want       string
	}{
		{"1", "", `["mcp__hq__*"]`},
		{"1", "officer_claim", `["mcp__muster__officer_claim","mcp__hq__*"]`},
		{"", "", `null`},
		{"", "officer_claim", `["mcp__muster__officer_claim"]`},
	} {
		o := hqSeedDir(t, c.hq)
		o.allowMCPTools = c.muster
		if err := seed(o, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		set, err := readJSONObject(filepath.Join(o.configDir, "settings.json"), nil)
		if err != nil {
			t.Fatal(err)
		}
		perms, _ := set["permissions"].(map[string]any)
		if got := mustJSON(t, perms["allow"]); got != c.want {
			t.Errorf("hq=%q muster=%q: permissions.allow = %s, want %s", c.hq, c.muster, got, c.want)
		}
		// The rest of the posture is the template's, unchanged.
		if perms["defaultMode"] != "default" || perms["disableBypassPermissionsMode"] != "disable" {
			t.Errorf("hq=%q: the permission posture changed: %v", c.hq, perms)
		}
		if err := checkSeed(o); err != nil {
			t.Errorf("hq=%q muster=%q: checkSeed: %v", c.hq, c.muster, err)
		}
	}
}

// TestSeedRefusesAnHQValueItDidNotWrite: only "1" turns the server on.
func TestSeedRefusesAnHQValueItDidNotWrite(t *testing.T) {
	for _, v := range []string{"true", " 1", "yes", "0"} {
		o := hqSeedDir(t, v)
		if err := seed(o, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "CCD_HQ_MCP") {
			t.Errorf("CCD_HQ_MCP=%q: seed = %v, want a refusal naming the variable", v, err)
		}
	}
}

// TestRunSeedReadsTheHQFlagFromTheEnvironment: the entrypoint's `ccd seed`
// reads CCD_HQ_MCP itself.
func TestRunSeedReadsTheHQFlagFromTheEnvironment(t *testing.T) {
	stubHQBinary(t, false)
	t.Setenv(envConnectURL, "")
	o := hqSeedDir(t, "")
	t.Setenv("CCD_HQ_MCP", "1")
	var out, errb bytes.Buffer
	if rc := runSeed([]string{"--config-dir", o.configDir, "--workspace", o.workspace,
		"--settings-template", o.settingsTemplate}, &out, &errb); rc != 0 {
		t.Fatalf("runSeed = %d: %s", rc, errb.String())
	}
	set, _ := readJSONObject(filepath.Join(o.configDir, "settings.json"), nil)
	perms, _ := set["permissions"].(map[string]any)
	if got := mustJSON(t, perms["allow"]); got != `["mcp__hq__*"]` {
		t.Fatalf("permissions.allow = %s, want [\"mcp__hq__*\"]", got)
	}
}
