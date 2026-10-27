package main

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// THE REMOVED GITHUB OAUTH CONNECTION'S VARIABLES ARE IGNORED, NOT REFUSED.
//
// A deployment configured for the OAuth connection sets GITHUB_CLIENT_ID,
// GITHUB_CLIENT_SECRET and MUSTER_GITHUB_ENCRYPTION_KEY (and maybe
// MUSTER_GITHUB_TOKEN). This build must boot on that environment unchanged —
// including a key that would once have been refused for not decoding to 32
// bytes, and a key equal to the connector key, which was refused as shared —
// and must say on the banner that each is ignored.

// removedOAuthEnv is a deployment's OAuth settings, with an encryption key that
// is not valid base64 at all: the old loader refused to boot on it.
func removedOAuthEnv() map[string]string {
	return map[string]string{
		envGitHubEncKey:    "not-base64-and-not-32-bytes!",
		envGitHubToken:     "ghp_example_not_a_real_token",
		envGitHubClientID:  "example-client-id",
		envGitHubClientSec: "example-client-secret",
	}
}

// TestTheRemovedGitHubOAuthVariablesAreIgnored: loadConfig and validate accept
// the set, record exactly which are set (in a fixed order), and the banner names
// them as IGNORED. With none set, nothing is recorded and no line is printed.
func TestTheRemovedGitHubOAuthVariablesAreIgnored(t *testing.T) {
	load := func(env map[string]string) config {
		t.Helper()
		full := map[string]string{envDatabase: "postgres://unused", envStandalone: "1"}
		for k, v := range env {
			full[k] = v
		}
		cfg, err := loadConfig(func(n string) string { return full[n] })
		if err != nil {
			t.Fatalf("loadConfig refused the removed OAuth variables: %v", err)
		}
		if err := cfg.validate(); err != nil {
			t.Fatalf("validate refused the removed OAuth variables: %v", err)
		}
		return cfg
	}

	cfg := load(removedOAuthEnv())
	want := []string{envGitHubEncKey, envGitHubToken, envGitHubClientID, envGitHubClientSec}
	if !reflect.DeepEqual(cfg.IgnoredGitHubOAuth, want) {
		t.Fatalf("IgnoredGitHubOAuth = %v, want %v", cfg.IgnoredGitHubOAuth, want)
	}
	out := renderBanner(t, cfg, api.Extensions{})
	line := "github oauth: REMOVED — set and IGNORED: " + strings.Join(want, ", ")
	if !strings.Contains(out, line) {
		t.Errorf("the banner does not say the OAuth variables are ignored; want a line containing\n  %s\ngot:\n%s", line, out)
	}

	// A connector key equal to the old GitHub key was refused as "the same key"
	// while both stores existed; only one store is left, so it boots.
	shared := removedOAuthEnv()
	shared[envGitHubEncKey] = connectorKey(2)
	shared[envConnectorEncKey] = connectorKey(2)
	shared[envAgentConnectURL] = "http://muster.example.test:18191/"
	shared[envConnectPort] = "18191"
	_ = load(shared)

	// Control: none set, nothing recorded and no line.
	none := load(nil)
	if len(none.IgnoredGitHubOAuth) != 0 {
		t.Fatalf("with nothing set, IgnoredGitHubOAuth = %v", none.IgnoredGitHubOAuth)
	}
	if out := renderBanner(t, none, api.Extensions{}); strings.Contains(out, "github oauth:") {
		t.Errorf("with nothing set the banner still prints a github oauth line:\n%s", out)
	}
	// Control: a whitespace-only value is not "set".
	if c := load(map[string]string{envGitHubClientID: "   "}); len(c.IgnoredGitHubOAuth) != 0 {
		t.Errorf("a blank %s counted as set: %v", envGitHubClientID, c.IgnoredGitHubOAuth)
	}
}

// TestAServerBootedWithTheOAuthVariablesServesNoOAuthRoute: the real binary,
// over a real database, booted with the removed variables set: it serves, and
// /github/connect (which used to 302 to GitHub with these set) is a 404.
func TestAServerBootedWithTheOAuthVariablesServesNoOAuthRoute(t *testing.T) {
	cfg := dbConfig(t)
	cfg.UIPassword = "oauth-removed-test-password"
	cfg.IgnoredGitHubOAuth = []string{envGitHubEncKey, envGitHubClientID, envGitHubClientSec}
	base := startServer(t, cfg)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login, err := client.PostForm(base+"/login", url.Values{"password": {cfg.UIPassword}})
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	var session *http.Cookie
	for _, c := range login.Cookies() {
		if c.Value != "" {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("login set no session cookie (%d)", login.StatusCode)
	}
	get := func(path string) int {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.AddCookie(session)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Control: the session works, so a 404 below is the route's absence.
	if code := get("/tasks"); code != http.StatusOK {
		t.Fatalf("control: GET /tasks = %d, want 200", code)
	}
	for _, p := range []string{"/github/connect", "/repos", "/ui/repos"} {
		if code := get(p); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (removed)", p, code)
		}
	}
}
