package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/githubapp"
)

// TestGitHubAppAccessModeIsParsedOrRefusedAtBoot: MUSTER_GITHUB_APP_ACCESS
// unset is `write` (an existing deployment is unchanged), `read` and `write`
// load as themselves, and ANY other value — including the near-misses an
// operator would type for "read only" — refuses to boot, naming the variable.
// 🔴 A typo must never fall back to the default: that would mint WRITE tokens
// on a deployment whose operator asked for read-only ones. Setting the mode
// without the App is refused too (it would configure nothing).
func TestGitHubAppAccessModeIsParsedOrRefusedAtBoot(t *testing.T) {
	keyPEM := testAppKeyPEM(t)
	load := func(extra map[string]string) (config, error) {
		full := map[string]string{envDatabase: "postgres://unused", envStandalone: "1",
			envConnectorEncKey: connectorKey(1), envConnectPort: "18191",
			envAgentConnectURL: "http://muster.example.test:18191/"}
		for k, v := range extra {
			full[k] = v
		}
		cfg, err := loadConfig(func(n string) string { return full[n] })
		if err != nil {
			return cfg, err
		}
		return cfg, cfg.validate()
	}
	app := func(access string, set bool) map[string]string {
		m := map[string]string{envGitHubAppID: "12345", envGitHubAppKey: keyPEM, envGitHubAppAccounts: "example-org:4200"}
		if set {
			m[envGitHubAppAccess] = access
		}
		return m
	}

	for _, c := range []struct {
		name string
		env  map[string]string
		want githubapp.Access
	}{
		{"unset", app("", false), githubapp.AccessWrite},
		{"empty", app("", true), githubapp.AccessWrite},
		{"write", app("write", true), githubapp.AccessWrite},
		{"read", app("read", true), githubapp.AccessRead},
		{"READ, spaced", app(" READ ", true), githubapp.AccessRead},
	} {
		cfg, err := load(c.env)
		if err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
			continue
		}
		if cfg.GitHubAppAccess != c.want {
			t.Errorf("%s: loaded access %q, want %q", c.name, cfg.GitHubAppAccess, c.want)
		}
	}

	for _, v := range []string{"readonly", "read-only", "ro", "rw", "none", "true", "admin", "read,write"} {
		_, err := load(app(v, true))
		if err == nil || !strings.Contains(err.Error(), envGitHubAppAccess) {
			t.Errorf("%s=%q: err = %v, want a boot refusal naming %s", envGitHubAppAccess, v, err, envGitHubAppAccess)
		}
	}

	if _, err := load(map[string]string{envGitHubAppAccess: "read"}); err == nil ||
		!strings.Contains(err.Error(), envGitHubAppAccess) || !strings.Contains(err.Error(), envGitHubAppID) {
		t.Errorf("access without the App: err = %v, want a refusal naming %s and %s", err, envGitHubAppAccess, envGitHubAppID)
	}
}

// bannerAccessApp is a banner stub GitHub App in a chosen mode.
type bannerAccessApp struct {
	bannerStubGitHubApp
	access githubapp.Access
}

func (b bannerAccessApp) Access() githubapp.Access { return b.access }

// TestTheGitHubAppBannerLineStatesTheAccessMode: the boot banner's `github
// app: ON` line names MUSTER_GITHUB_APP_ACCESS with its value and the scope
// that mode mints — the read sentence (never "write") in read mode, the write
// sentence in write mode.
func TestTheGitHubAppBannerLineStatesTheAccessMode(t *testing.T) {
	line := func(access githubapp.Access) string {
		cfg := config{GitHubAppID: 12345, GitHubAppAccounts: []githubapp.Account{{Login: "example-org", ID: 4200}}}
		out := renderBanner(t, cfg, api.Extensions{GitHubApp: bannerAccessApp{access: access}, Connectors: bannerStubConnectorStore{}})
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "github app: ON") {
				return l
			}
		}
		t.Fatalf("no `github app: ON` line:\n%s", out)
		return ""
	}
	r := line(githubapp.AccessRead)
	if !strings.Contains(r, envGitHubAppAccess+"=read: "+githubapp.AccessRead.Scope()) || strings.Contains(r, "read and write") {
		t.Errorf("read-mode banner line: %s", r)
	}
	w := line(githubapp.AccessWrite)
	if !strings.Contains(w, envGitHubAppAccess+"=write: Contents and Pull requests read and write, Checks and Commit statuses read") {
		t.Errorf("write-mode banner line: %s", w)
	}
}

// TestBuildAppMintsInTheConfiguredAccessMode is the WIRING seam: the binary,
// configured from the environment with MUSTER_GITHUB_APP_ACCESS=read, builds a
// GitHub App in read mode — read off the App buildApp constructed (its banner
// line reports App.Access()), not off the config. A buildApp that parsed the
// variable but did not hand it to githubapp.New passes every other test and
// mints WRITE tokens; it fails here.
func TestBuildAppMintsInTheConfiguredAccessMode(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`)) // the boot check: no installations
	}))
	t.Cleanup(fake.Close)
	github.UseAPIBaseURL(fake.URL)
	t.Cleanup(func() { github.UseAPIBaseURL("https://api.github.com") })
	keyPEM := testAppKeyPEM(t)
	for _, c := range []struct {
		env, want string
	}{
		{"read", envGitHubAppAccess + "=read: " + githubapp.AccessRead.Scope()},
		{"", envGitHubAppAccess + "=write: " + githubapp.AccessWrite.Scope()},
	} {
		key, err := loadConfig(func(n string) string {
			switch n {
			case envGitHubAppID:
				return "12345"
			case envGitHubAppKey:
				return keyPEM
			case envGitHubAppAccounts:
				return "example-org:4200"
			case envGitHubAppAccess:
				return c.env
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg := testConfig()
		cfg.Database = dbtest.DSN(t)
		cfg.ConnectorEncKey = []byte(strings.Repeat("k", 32))
		cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAppAccounts, cfg.GitHubAppAccess =
			key.GitHubAppID, key.GitHubAppKey, key.GitHubAppAccounts, key.GitHubAppAccess
		var buf lockedBuffer // buildApp's background App check logs into it too
		a, err := buildApp(context.Background(), cfg, log.New(&buf, "", 0))
		if err != nil {
			t.Fatalf("%s=%q: buildApp: %v", envGitHubAppAccess, c.env, err)
		}
		a.Close()
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("%s=%q: the built App does not report %q; banner:\n%s", envGitHubAppAccess, c.env, c.want, buf.String())
		}
	}
}

// lockedBuffer is a bytes.Buffer safe for a logger written from another
// goroutine while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
