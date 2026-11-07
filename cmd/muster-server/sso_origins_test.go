package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

func loadSSOConfig(origins string) (config, error) {
	c, err := loadConfig(func(k string) string {
		switch k {
		case envDatabase:
			return "postgres://x/y"
		case envStandalone:
			return "1"
		case envAndroidSSOOrigins:
			return origins
		}
		return ""
	})
	if err != nil {
		return c, err
	}
	return c, c.validate()
}

// TestAndroidSSOOriginsAreValidatedAtBoot: the variable reaches the parser
// through loadConfig and validate, a good list is kept normalised, unset is an
// empty list, and a malformed value refuses boot naming the variable.
func TestAndroidSSOOriginsAreValidatedAtBoot(t *testing.T) {
	c, err := loadSSOConfig(" https://Auth.Example.test , https://idp.example.test:8443 ")
	if err != nil {
		t.Fatalf("a valid list refused: %v", err)
	}
	if got, _ := c.ssoOrigins(); strings.Join(got, " ") != "https://auth.example.test https://idp.example.test:8443" {
		t.Fatalf("the variable did not reach the list: %q", got)
	}
	c, err = loadSSOConfig("")
	if err != nil {
		t.Fatalf("unset refused: %v", err)
	}
	if got, err := c.ssoOrigins(); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("unset = %#v, %v; want an empty, non-nil list", got, err)
	}
	for _, bad := range []string{
		"http://auth.example.test",
		"https://auth.example.test/",
		"https://auth.example.test/login",
		"https://auth.example.test?x",
		"https://user@auth.example.test",
		"https://*.example.test",
		"https://auth.example.test,,https://idp.example.test",
	} {
		if _, err := loadSSOConfig(bad); err == nil || !strings.Contains(err.Error(), envAndroidSSOOrigins) {
			t.Errorf("%q: err = %v; want a boot refusal naming %s", bad, err, envAndroidSSOOrigins)
		}
	}
}

// TestTheServerServesSSOOriginsFromItsConfiguration drives the binary's own
// listen path (startServer): the configuration reaches GET /api/device/web,
// open, with no credential; unset, the same route answers an empty list.
func TestTheServerServesSSOOriginsFromItsConfiguration(t *testing.T) {
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fetch := func(base string) (*http.Response, string) {
		t.Helper()
		resp, err := noRedirect.Get(base + api.DeviceWebPath)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	cfg := testConfig()
	cfg.AndroidSSOOrigins = "https://auth.example.test,https://idp.example.test:8443"
	resp, body := fetch(startServer(t, cfg))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configured: status %d (Location %q), want 200", resp.StatusCode, resp.Header.Get("Location"))
	}
	if want := `{"sso_origins":["https://auth.example.test","https://idp.example.test:8443"]}` + "\n"; body != want {
		t.Fatalf("configured: body %q, want %q", body, want)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("configured: Cache-Control %q, want no-store", cc)
	}

	resp, body = fetch(startServer(t, testConfig()))
	if resp.StatusCode != http.StatusOK || body != `{"sso_origins":[]}`+"\n" {
		t.Fatalf("unset: status %d body %q, want 200 and an empty list", resp.StatusCode, body)
	}
}

// TestTheAndroidSSOOriginsBannerLineMatchesWhatIsServed: ON with the origins
// the route serves, OFF without them, so an inverted branch fails here.
func TestTheAndroidSSOOriginsBannerLineMatchesWhatIsServed(t *testing.T) {
	line := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "android sign-in origins:") {
				return l
			}
		}
		t.Fatalf("no \"android sign-in origins:\" line:\n%s", out)
		return ""
	}
	on := config{AndroidSSOOrigins: "https://auth.example.test"}
	origins, err := on.ssoOrigins()
	if err != nil {
		t.Fatal(err)
	}
	if l := line(renderBanner(t, on, api.Extensions{SSOOrigins: origins})); !strings.Contains(l, "ON") ||
		strings.Contains(l, "OFF") || !strings.Contains(l, "https://auth.example.test") {
		t.Errorf("configured: %q", l)
	}
	if l := line(renderBanner(t, config{}, api.Extensions{})); !strings.Contains(l, "OFF") || strings.Contains(l, "ON —") {
		t.Errorf("unset: %q", l)
	}
}
