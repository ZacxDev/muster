package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// TestPluginOriginsReachEveryListenerTheBinaryBuilds: MUSTER_PLUGIN_ORIGINS,
// read by loadConfig and wired by buildApp, makes each listener the binary
// builds (main, MUSTER_CONNECT_PORT, MUSTER_BROWSER_PORT) refuse a plugin host
// with 421 — the seam between the config and the middleware, which the api
// package's own test (a hand-built Extensions) cannot see. The control is the
// same binary with the variable unset serving the same request.
func TestPluginOriginsReachEveryListenerTheBinaryBuilds(t *testing.T) {
	build := func(origins string) *app {
		t.Helper()
		loaded, err := loadConfig(func(k string) string {
			if k == envPluginOrigins {
				return origins
			}
			return ""
		})
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		cfg := testConfig()
		cfg.PluginOrigins = loaded.PluginOrigins
		cfg.ConnectorEncKey = bytes.Repeat([]byte{1}, 32)
		cfg.ConnectPort = freePort(t)
		cfg.BrowserPort = freePort(t)
		a, err := buildApp(context.Background(), cfg, quietLogger())
		if err != nil {
			t.Fatalf("buildApp: %v", err)
		}
		t.Cleanup(a.Close)
		if a.connect == nil || a.browser == nil {
			t.Fatal("instrument: the connect and browser listeners were not built")
		}
		return a
	}
	probe := func(h http.Handler) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/health", nil)
		r.Host = "p-abc.preview.example.net"
		r.Header.Set("Origin", "https://p-abc.preview.example.net")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	on := build("https://*.preview.example.net")
	off := build("")
	for name, pair := range map[string][2]*http.Server{
		"main":    {on.http, off.http},
		"connect": {on.connect, off.connect},
		"browser": {on.browser, off.browser},
	} {
		if rec := probe(pair[0].Handler); rec.Code != http.StatusMisdirectedRequest || rec.Body.String() != api.PluginOriginHostRefusal {
			t.Errorf("%s listener, MUSTER_PLUGIN_ORIGINS set: %d %q; want 421", name, rec.Code, rec.Body.String())
		}
		if rec := probe(pair[1].Handler); rec.Code != http.StatusOK {
			t.Errorf("CONTROL: %s listener, unset: %d; want 200", name, rec.Code)
		}
	}
}

// TestABadPluginOriginsRefusesToBoot: a value that does not parse, or one that
// covers MUSTER_PUBLIC_URL's own host, refuses to boot with an error naming the
// variable; a good value beside a public URL outside it loads.
func TestABadPluginOriginsRefusesToBoot(t *testing.T) {
	env := func(origins, public string) func(string) string {
		return func(k string) string {
			switch k {
			case envPluginOrigins:
				return origins
			case envPublicURL:
				return public
			}
			return ""
		}
	}
	for _, c := range []struct{ origins, public, want string }{
		{"http://*.preview.example.net", "", "MUSTER_PLUGIN_ORIGINS"},
		{"https://*.preview.example.net/x", "", "MUSTER_PLUGIN_ORIGINS"},
		{"https://*.example.net", "https://muster.example.net", "would refuse its own pages"},
		{"https://muster.example.net", "https://muster.example.net:8443/", "would refuse its own pages"},
	} {
		if _, err := loadConfig(env(c.origins, c.public)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("origins %q, public %q: %v; want a refusal containing %q", c.origins, c.public, err, c.want)
		}
	}
	cfg, err := loadConfig(env("https://*.preview.example.net", "https://muster.example.net"))
	if err != nil || len(cfg.PluginOrigins) != 1 || cfg.PluginOrigins[0].String() != "https://*.preview.example.net" {
		t.Fatalf("CONTROL: a good value: %v, %v; want it loaded", cfg.PluginOrigins, err)
	}
}
