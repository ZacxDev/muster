package previewgw

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// owner_login is a label: two owners sharing one is ambiguous and refused,
// an unknown one is refused, and with no login the single live owner is the
// answer. A token that is not live (401, no viewer) cannot be named.
func TestResolveOwnerRefusesAnAmbiguousOrUnknownLogin(t *testing.T) {
	live := func(id int64, login string) SelfAnswer {
		return SelfAnswer{Self: viewerSelf(id, login), Verdict: SelfLive}
	}
	dead := SelfAnswer{Self: Self{OwnerID: 9, OwnerLogin: "shared@example.invalid"}, Verdict: SelfDead}

	if _, err := ResolveOwner("shared@example.invalid", []SelfAnswer{live(1, "shared@example.invalid"), live(2, "shared@example.invalid")}); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "1, 2") {
		t.Fatalf("two owners, one login: %v; want an ambiguity refusal naming both ids", err)
	}
	// Two tokens of the SAME owner are not ambiguous.
	s, err := ResolveOwner("one@example.invalid", []SelfAnswer{live(1, "one@example.invalid"), live(1, "one@example.invalid")})
	if err != nil || s.OwnerID != 1 {
		t.Fatalf("one owner, two tokens: %+v, %v", s, err)
	}
	if s, err := ResolveOwner("two@example.invalid", []SelfAnswer{live(1, "one@example.invalid"), live(2, "two@example.invalid")}); err != nil || s.OwnerID != 2 {
		t.Fatalf("distinct logins: %+v, %v; want owner 2", s, err)
	}
	if _, err := ResolveOwner("nobody@example.invalid", []SelfAnswer{live(1, "one@example.invalid")}); err == nil || !strings.Contains(err.Error(), "no live plugin token") {
		t.Fatalf("unknown login: %v", err)
	}
	// A dead token's login is not resolvable, and the skip is reported.
	if _, err := ResolveOwner("shared@example.invalid", []SelfAnswer{live(1, "one@example.invalid"), dead}); err == nil || !strings.Contains(err.Error(), "1 token(s)") {
		t.Fatalf("a dead token's login: %v", err)
	}
	if s, err := ResolveOwner("", []SelfAnswer{live(1, "one@example.invalid"), dead}); err != nil || s.OwnerID != 1 {
		t.Fatalf("no login, one live owner: %+v, %v", s, err)
	}
	if _, err := ResolveOwner("", []SelfAnswer{live(1, "one@example.invalid"), live(2, "two@example.invalid")}); err == nil || !strings.Contains(err.Error(), "name the owner") {
		t.Fatalf("no login, two owners: %v", err)
	}
}

// /self's verdicts from muster's real answer shapes.
func TestSelfVerdicts(t *testing.T) {
	fm := newFakeMuster(t)
	fm.set("mpl_v", 200, viewerSelf(4, "v@example.invalid"))
	fm.set("mpl_m", 200, Self{PluginID: 5, OwnerID: 5, OwnerLogin: "m@example.invalid", Capabilities: []string{"messages"}})
	fm.set("mpl_r", http.StatusUnauthorized, Self{})
	fm.set("mpl_off", http.StatusNotFound, Self{})
	mc := NewMusterClient(fm.srv.URL, "test")
	for tok, want := range map[string]SelfVerdict{"mpl_v": SelfLive, "mpl_m": SelfDead, "mpl_r": SelfDead, "mpl_off": SelfUnknown} {
		_, v, _ := mc.Self(context.Background(), tok)
		if v != want {
			t.Errorf("%s: verdict %v; want %v", tok, v, want)
		}
	}
	unreachable := NewMusterClient("http://127.0.0.1:1", "test")
	if _, v, err := unreachable.Self(context.Background(), "mpl_v"); v != SelfUnknown || err == nil || strings.Contains(err.Error(), "mpl_v") {
		t.Errorf("unreachable: %v, %v; want unknown, an error without the token", v, err)
	}
}

// Tokens are stored as sha256 only, in a 0600 file; revoke removes them; the
// shape check accepts exactly what Create mints.
func TestHostTokensStoreOnlyTheHash(t *testing.T) {
	dir := t.TempDir()
	s := HostTokens{Dir: dir}
	tok, rec, err := s.Create(1, "one@example.invalid", "alpha", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !TokenShapeOK(tok) || TokenShapeOK(tok+"A") || TokenShapeOK(tok[:len(tok)-1]) || TokenShapeOK("mpl_"+tok[4:]) {
		t.Fatalf("shape check is wrong around %q", tok)
	}
	b, err := os.ReadFile(filepath.Join(dir, hostTokensFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), tok[4:]) || !strings.Contains(string(b), HashToken(tok)) {
		t.Fatal("the file holds the secret, or not its hash")
	}
	if st, _ := os.Stat(filepath.Join(dir, hostTokensFile)); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v; want 0600", st.Mode().Perm())
	}
	if _, _, err := s.Create(1, "x", "Not_A_Label", time.Now()); err == nil {
		t.Fatal("an invalid host name was accepted")
	}
	if _, err := s.Revoke(rec.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List(); len(l) != 0 {
		t.Fatalf("after revoke: %+v", l)
	}
	if _, err := s.Revoke(rec.ID); err != ErrNoHostToken {
		t.Fatalf("second revoke: %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "tokens")
	if err := os.WriteFile(tf, []byte("mpl_a\n\nmpl_b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"MUSTER_URL": "http://muster.svc:8080/", "MUSTER_PUBLIC_URL": "https://muster.example",
		"PLUGIN_TOKENS_FILE": tf, "PREVIEW_DOMAIN": "preview.example", "STATE_DIR": dir, "PREVIEW_MESH_LISTEN": ":9090",
		"PREVIEW_SIGNING_KEY": strings.Repeat("k", MinSigningKey)}
	env := func(over map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := over[k]; ok {
				return v
			}
			return base[k]
		}
	}
	c, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.MusterURL != "http://muster.svc:8080" || c.Listen != DefaultListen || c.MeshListen != ":9090" || len(c.PluginTokens) != 2 ||
		string(c.SigningKey) != strings.Repeat("k", MinSigningKey) {
		t.Fatalf("config %+v", c)
	}
	for name, over := range map[string]map[string]string{
		"no muster url":     {"MUSTER_URL": ""},
		"no public url":     {"MUSTER_PUBLIC_URL": ""},
		"bad domain":        {"PREVIEW_DOMAIN": "Preview.Example"},
		"domain with port":  {"PREVIEW_DOMAIN": "preview.example:443"},
		"single label":      {"PREVIEW_DOMAIN": "localhost"},
		"no state dir":      {"STATE_DIR": filepath.Join(dir, "missing")},
		"same listeners":    {"PREVIEW_LISTEN": ":9090"},
		"bad mesh listen":   {"PREVIEW_MESH_LISTEN": "9090"},
		"no tokens file":    {"PLUGIN_TOKENS_FILE": ""},
		"unreadable tokens": {"PLUGIN_TOKENS_FILE": filepath.Join(dir, "missing")},
		"no signing key":    {"PREVIEW_SIGNING_KEY": ""},
		"short signing key": {"PREVIEW_SIGNING_KEY": strings.Repeat("k", MinSigningKey-1)},
	} {
		if _, err := LoadConfig(env(over)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	bad := filepath.Join(dir, "bad")
	_ = os.WriteFile(bad, []byte("mst_notaplugin\n"), 0o600)
	if _, err := LoadConfig(env(map[string]string{"PLUGIN_TOKENS_FILE": bad})); err == nil || strings.Contains(err.Error(), "mst_notaplugin") {
		t.Errorf("a non-plugin token: %v (must be refused without echoing it)", err)
	}
}
