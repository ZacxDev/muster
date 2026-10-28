package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// TestMusterPublicURLReachesTheSameOriginCheck: MUSTER_PUBLIC_URL, read by the
// real loader and wired by buildApp, is the host the running server accepts as
// its own Origin when a proxy rewrote Host. A state-changing request whose
// Origin is the public host gets past requireSameOrigin; the same request from
// another origin is refused with the cross-origin refusal (the control that the
// route is guarded at all), and so is the public origin on a server built
// without the variable (the control that the acceptance comes from it).
func TestMusterPublicURLReachesTheSameOriginCheck(t *testing.T) {
	const public = "https://muster.example.test"
	boot := func(publicURL string) (base string, session *http.Cookie) {
		t.Helper()
		env := map[string]string{
			envUIPassword: "public-url-wiring-password-0123456789", envStandalone: "1",
			envPublicURL: publicURL,
		}
		cfg, err := loadConfig(func(n string) string { return env[n] })
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		cfg.Port = 0
		base = startServer(t, cfg)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.PostForm(base+"/login", url.Values{"password": {cfg.UIPassword}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Value != "" {
				session = c
			}
		}
		if session == nil {
			t.Fatalf("login set no session cookie (%d)", resp.StatusCode)
		}
		return base, session
	}
	post := func(base string, session *http.Cookie, origin string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+"/tasks/1/complete", nil)
		req.Host = "upstream.invalid:8105" // what a proxy forwards
		req.Header.Set("Origin", origin)
		req.AddCookie(session)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	refused := func(code int, body string) bool {
		return code == http.StatusForbidden && strings.Contains(body, api.CrossOriginRefusal)
	}

	base, session := boot(public)
	if code, body := post(base, session, public); refused(code, body) {
		t.Errorf("with %s=%s, a POST from that origin (Host rewritten) was refused as cross-origin: %d %s",
			envPublicURL, public, code, body)
	}
	if code, body := post(base, session, "https://elsewhere.example.test"); !refused(code, body) {
		t.Errorf("control: a POST from another origin answered %d %q; want the cross-origin refusal", code, body)
	}
	base, session = boot("")
	if code, body := post(base, session, public); !refused(code, body) {
		t.Errorf("control: without %s the public origin answered %d %q; want the cross-origin refusal", envPublicURL, code, body)
	}
}
