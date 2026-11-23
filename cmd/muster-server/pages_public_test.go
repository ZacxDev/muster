package main

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// THE PUBLIC PAGES LISTENER'S CONFIGURATION AND WIRING
// (MUSTER_PAGES_PUBLIC_LISTEN, MUSTER_PAGES_PUBLIC_URL). Every address is
// invented.

// TestThePublicPagesListenerIsAPairWithPagesOn: both variables or neither,
// only with MUSTER_PAGES=1, on a port of its own, with an https origin (http
// only to this machine) as the link base; the pair is read as written.
func TestThePublicPagesListenerIsAPairWithPagesOn(t *testing.T) {
	load := func(env map[string]string) (config, error) {
		t.Helper()
		full := map[string]string{envDatabase: "postgres://x/y", envStandalone: "1", envHookToken: fixtureHookToken, envPages: "1"}
		for k, v := range env {
			full[k] = v
		}
		c, err := loadConfig(func(k string) string { return full[k] })
		if err != nil {
			return c, err
		}
		return c, c.validate()
	}
	c, err := load(map[string]string{envPagesPublicListen: ":18108", envPagesPublicURL: "https://pages.example.com/"})
	if err != nil {
		t.Fatalf("control: the pair is refused: %v", err)
	}
	if c.PagesPublicListen != ":18108" || c.PagesPublicPort != 18108 || c.PagesPublicURL != "https://pages.example.com" {
		t.Fatalf("read as %q / %d / %q", c.PagesPublicListen, c.PagesPublicPort, c.PagesPublicURL)
	}
	if c, err := load(nil); err != nil || c.PagesPublicListen != "" || c.PagesPublicURL != "" {
		t.Fatalf("control: neither set: %v %q %q", err, c.PagesPublicListen, c.PagesPublicURL)
	}
	if _, err := load(map[string]string{envPagesPublicListen: "127.0.0.1:18108", envPagesPublicURL: "http://localhost:18108"}); err != nil {
		t.Fatalf("control: a loopback test pair is refused: %v", err)
	}
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"listen alone":     {map[string]string{envPagesPublicListen: ":18108"}, envPagesPublicURL},
		"url alone":        {map[string]string{envPagesPublicURL: "https://pages.example.com"}, envPagesPublicListen},
		"pages off":        {map[string]string{envPages: "", envPagesPublicListen: ":18108", envPagesPublicURL: "https://pages.example.com"}, envPages},
		"no port":          {map[string]string{envPagesPublicListen: "18108", envPagesPublicURL: "https://pages.example.com"}, envPagesPublicListen},
		"named port":       {map[string]string{envPagesPublicListen: ":http", envPagesPublicURL: "https://pages.example.com"}, envPagesPublicListen},
		"port 0":           {map[string]string{envPagesPublicListen: ":0", envPagesPublicURL: "https://pages.example.com"}, envPagesPublicListen},
		"the main port":    {map[string]string{envPagesPublicListen: ":8105", envPagesPublicURL: "https://pages.example.com"}, envPort},
		"the metrics port": {map[string]string{envMetricsPort: "18109", envPagesPublicListen: ":18109", envPagesPublicURL: "https://pages.example.com"}, envMetricsPort},
		"the browser port": {map[string]string{envBrowserPort: "18110", envPagesPublicListen: ":18110", envPagesPublicURL: "https://pages.example.com"}, envBrowserPort},
		"plain http":       {map[string]string{envPagesPublicListen: ":18108", envPagesPublicURL: "http://pages.example.com"}, envPagesPublicURL},
		"a path":           {map[string]string{envPagesPublicListen: ":18108", envPagesPublicURL: "https://pages.example.com/muster"}, envPagesPublicURL},
		"a query":          {map[string]string{envPagesPublicListen: ":18108", envPagesPublicURL: "https://pages.example.com?x=1"}, envPagesPublicURL},
		"credentials":      {map[string]string{envPagesPublicListen: ":18108", envPagesPublicURL: "https://u:p@pages.example.com"}, envPagesPublicURL},
		"no host":          {map[string]string{envPagesPublicListen: ":18108", envPagesPublicURL: "https://"}, envPagesPublicURL},
	} {
		if _, err := load(c.env); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal naming %s", name, err, c.want)
		}
	}
}

// TestThePublicPagesListenerIsBoundAndServesOnlyShareLinks boots the real
// app with MUSTER_PAGES_PUBLIC_LISTEN: a share-link path there reaches
// muster's own share route (its refusal, with no database here), the UI, the
// API, sign-in and health are the listener's 404, and the main port still
// serves sign-in and health.
func TestThePublicPagesListenerIsBoundAndServesOnlyShareLinks(t *testing.T) {
	cfg := testConfig()
	port := freePort(t)
	cfg.PagesPublicListen = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	cfg.PagesPublicPort = port
	cfg.PagesPublicURL = "http://localhost:" + strconv.Itoa(port)
	main := startServer(t, cfg)
	public := "http://" + cfg.PagesPublicListen
	get := func(u string) (int, string) {
		t.Helper()
		resp, err := http.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(public + "/s/" + strings.Repeat("Q", 43)); code != http.StatusNotFound || body != "This link does not exist, or no longer works.\n" {
		t.Errorf("a share link on the public listener = %d %q, want the share route's own refusal", code, body)
	}
	for _, p := range []string{"/", "/login", "/api/tasks", "/health", "/metrics", "/tasks", "/static/app.css"} {
		if code, body := get(public + p); code != http.StatusNotFound || body != "404 page not found\n" {
			t.Errorf("public listener %s = %d %q, want the listener's 404", p, code, body)
		}
	}
	if code, _ := get(main + "/health"); code != http.StatusOK {
		t.Errorf("control: the main port's /health = %d", code)
	}
	if code, _ := get(main + "/login"); code == http.StatusNotFound {
		t.Error("control: the main port does not serve /login")
	}
}
