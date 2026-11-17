package main

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// fixtureHookToken arms the hook tier in the fixtures whose subject is some
// other refusal: validate() refuses every configuration without a
// MUSTER_HOOK_TOKEN (validateListeners), so a fixture meant to boot is armed.
const fixtureHookToken = "fixture-hook-token-0123456789abcdef"

// TestTheHookTierBootPolicy: MUSTER_HOOK_TOKEN is required in both auth modes,
// whatever else is configured. A table of mode x token (set / empty / blank /
// unset) x a few layouts — a loopback public URL, a public https one, a browser
// listener — so that no layout can be the one that lets an unarmed server boot.
//
// "Empty" is its own row because a Kubernetes Secret whose key holds "" delivers
// exactly that. Each row goes through loadConfig — the parse a real boot uses —
// and then validate.
func TestTheHookTierBootPolicy(t *testing.T) {
	tokens := []struct {
		name string
		env  map[string]string // added to the env; "unset" adds nothing
	}{
		{"set", map[string]string{envHookToken: fixtureHookToken}},
		{"empty", map[string]string{envHookToken: ""}},
		{"blank", map[string]string{envHookToken: strings.Repeat(" ", 16)}},
		{"unset", nil},
	}
	layouts := []struct {
		name string
		env  map[string]string
	}{
		{"no public URL", nil},
		{"a loopback public URL", map[string]string{envPublicURL: "http://localhost:8105"}},
		{"a public https URL", map[string]string{envPublicURL: "https://muster.example.com"}},
		{"a browser listener", map[string]string{envBrowserPort: "18106"}},
	}
	modes := map[string]map[string]string{
		"password": {},
		"gotrue": {
			envAuthMode:     "gotrue",
			envGoTrueURL:    "http://auth.example.invalid:9999",
			envGoTrueSecret: accountsSecret,
			envBrowserPort:  "18106",
		},
	}
	for mode, modeEnv := range modes {
		for _, tok := range tokens {
			for _, l := range layouts {
				t.Run(mode+"/token "+tok.name+"/"+l.name, func(t *testing.T) {
					env := map[string]string{
						envDatabase:   "postgres://x/y",
						envStandalone: "1",
						envUIPassword: "correct-horse-battery-staple",
					}
					for _, m := range []map[string]string{modeEnv, tok.env, l.env} {
						for k, v := range m {
							env[k] = v
						}
					}
					c, err := loadConfig(func(k string) string { return env[k] })
					if err != nil {
						t.Fatalf("loadConfig: %v", err)
					}
					err = c.validate()
					if tok.name == "set" {
						if err != nil {
							t.Fatalf("validate() = %v, want it to boot", err)
						}
						return
					}
					if err == nil {
						t.Fatalf("validate() = nil: a server with no hook token boots")
					}
					if !strings.Contains(err.Error(), envHookToken) {
						t.Fatalf("the refusal does not name %s: %v", envHookToken, err)
					}
					// The refusal says how to make a value. (A blank value of 16
					// spaces passes the length floor, so it reaches the same check
					// as unset; gotrue mode's unset refusal is validateAuthMode's.)
					if (mode == "password" || tok.name == "blank") && !strings.Contains(err.Error(), "openssl rand -hex 32") {
						t.Fatalf("the refusal does not say how to generate a value: %v", err)
					}
				})
			}
		}
	}
}

// TestTheMetricsPortIsParsedAndKeptOffTheOtherListeners: MUSTER_METRICS_PORT is
// a port of its own; sharing one with another listener is refused.
func TestTheMetricsPortIsParsedAndKeptOffTheOtherListeners(t *testing.T) {
	if _, err := loadConfig(func(k string) string {
		if k == envMetricsPort {
			return "0"
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), envMetricsPort) {
		t.Fatalf("%s=0: loadConfig = %v, want a refusal naming it", envMetricsPort, err)
	}
	c := memoBase()
	c.Port = 8105
	c.MetricsPort = 8105
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), envMetricsPort) {
		t.Fatalf("metrics port = main port: validate() = %v, want a refusal naming %s", err, envMetricsPort)
	}
	c.MetricsPort = 9105
	if err := c.validate(); err != nil {
		t.Fatalf("control: a metrics port of its own is refused: %v", err)
	}
}

// TestSecureCookiesFollowAnHTTPSPublicURL: unset, MUSTER_SECURE_COOKIES is on
// exactly when MUSTER_PUBLIC_URL is https; an explicit value wins either way,
// so an operator can still opt out.
func TestSecureCookiesFollowAnHTTPSPublicURL(t *testing.T) {
	for _, tc := range []struct {
		secure, publicURL string
		want              bool
	}{
		{"", "https://muster.example.com", true},
		{"", "HTTPS://muster.example.com", true},
		{"", "http://muster.example.com", false},
		{"", "", false},
		{"", "https://", false},
		{"0", "https://muster.example.com", false},
		{"off", "https://muster.example.com", false},
		{"1", "http://192.0.2.10:8105", true},
		{"1", "", true},
	} {
		c, err := loadConfig(func(k string) string {
			switch k {
			case envSecureCookies:
				return tc.secure
			case envPublicURL:
				return tc.publicURL
			}
			return ""
		})
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if c.SecureCookies != tc.want {
			t.Errorf("%s=%q %s=%q: SecureCookies = %v, want %v", envSecureCookies, tc.secure, envPublicURL, tc.publicURL, c.SecureCookies, tc.want)
		}
	}
}

// TestAnHTTPSPublicURLGetsASecureCookieAndHSTS drives the real loader and the
// real server: with an https MUSTER_PUBLIC_URL and MUSTER_SECURE_COOKIES
// unset, the password-mode sign-in sets a Secure session cookie and every
// response carries Strict-Transport-Security. The controls: an explicit
// MUSTER_SECURE_COOKIES=0 still opts out of Secure (HSTS stays), and a server
// with no public URL sends neither.
func TestAnHTTPSPublicURLGetsASecureCookieAndHSTS(t *testing.T) {
	signIn := func(env map[string]string) (cookie, hsts string) {
		t.Helper()
		env[envUIPassword] = "secure-cookie-wiring-password-0123456789"
		env[envStandalone] = "1"
		env[envHookToken] = fixtureHookToken
		cfg, err := loadConfig(func(n string) string { return env[n] })
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		// It boots (validate needs a database URL; the served copy needs none).
		withDB := cfg
		withDB.Database = "postgres://x/y"
		if err := withDB.validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		cfg.Port = 0
		base := startServer(t, cfg)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.PostForm(base+"/login", url.Values{"password": {cfg.UIPassword}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, v := range resp.Header.Values("Set-Cookie") {
			if strings.Contains(v, "=") && !strings.HasPrefix(strings.SplitN(v, "=", 2)[1], ";") {
				cookie = v
			}
		}
		if cookie == "" {
			t.Fatalf("login set no session cookie (%d)", resp.StatusCode)
		}
		return cookie, resp.Header.Get("Strict-Transport-Security")
	}
	secure := func(setCookie string) bool {
		for _, attr := range strings.Split(setCookie, ";") {
			if strings.EqualFold(strings.TrimSpace(attr), "Secure") {
				return true
			}
		}
		return false
	}

	c, hsts := signIn(map[string]string{envPublicURL: "https://muster.example.com"})
	if !secure(c) {
		t.Errorf("https public URL, %s unset: the session cookie is not Secure", envSecureCookies)
	}
	if hsts != "max-age=31536000" {
		t.Errorf("https public URL: Strict-Transport-Security = %q; want max-age=31536000", hsts)
	}

	c, hsts = signIn(map[string]string{envPublicURL: "https://muster.example.com", envSecureCookies: "0"})
	if secure(c) {
		t.Errorf("https public URL, %s=0: the opt-out did not hold, the cookie is Secure", envSecureCookies)
	}
	if hsts == "" {
		t.Errorf("https public URL, %s=0: no Strict-Transport-Security; the cookie opt-out must not drop it", envSecureCookies)
	}

	c, hsts = signIn(map[string]string{})
	if secure(c) || hsts != "" {
		t.Errorf("no public URL: cookie Secure=%v, Strict-Transport-Security=%q; want neither", secure(c), hsts)
	}
}

// TestTheMetricsListenerIsWired boots the real app with a MUSTER_METRICS_PORT:
// /metrics is a 404 on the main listener, and the metrics port serves the
// registry.
func TestTheMetricsListenerIsWired(t *testing.T) {
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metricsPort := free.Addr().(*net.TCPAddr).Port
	free.Close()

	cfg := testConfig()
	cfg.MetricsPort = metricsPort
	base := startServer(t, cfg)
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
	if code, _ := get(base + "/metrics"); code != http.StatusNotFound {
		t.Errorf("GET /metrics on the main listener = %d; want 404", code)
	}
	if code, _ := get(base + "/health"); code != http.StatusOK {
		t.Fatalf("control: GET /health on the main listener = %d", code)
	}
	code, body := get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(metricsPort)) + "/metrics")
	if code != http.StatusOK || !strings.Contains(body, "go_goroutines") {
		t.Fatalf("GET /metrics on %s=%d = %d (has go_goroutines: %v); want 200 with the registry",
			envMetricsPort, metricsPort, code, strings.Contains(body, "go_goroutines"))
	}
}
