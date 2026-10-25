package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

const alTestCert = "0123456789abcdef00112233445566778899aabbccddeeff0f1e2d3c4b5a6978"

// TestAndroidAssetLinksVariablesAreValidatedAtBoot: the pair is all or none,
// and a malformed value refuses boot naming both variables, instead of
// shipping a statement Android silently ignores.
func TestAndroidAssetLinksVariablesAreValidatedAtBoot(t *testing.T) {
	load := func(pkg, certs string) (config, error) {
		c, err := loadConfig(func(k string) string {
			switch k {
			case envDatabase:
				return "postgres://x/y"
			case envStandalone:
				return "1"
			case envAndroidPackage:
				return pkg
			case envAndroidCertSHA256:
				return certs
			}
			return ""
		})
		if err != nil {
			return c, err
		}
		return c, c.validate()
	}
	c, err := load("app.example.passkeys", alTestCert)
	if err != nil {
		t.Fatalf("a valid pair refused: %v", err)
	}
	if body, _ := c.assetLinks(); !strings.Contains(string(body), `"package_name":"app.example.passkeys"`) {
		t.Fatalf("the variables did not reach the statement: %s", body)
	}
	if c, err := load("", ""); err != nil {
		t.Fatalf("unset refused: %v", err)
	} else if body, _ := c.assetLinks(); body != nil {
		t.Fatalf("unset built a statement: %s", body)
	}
	for _, bad := range [][2]string{
		{"app.example.passkeys", ""},
		{"", alTestCert},
		{"passkeys", alTestCert},
		{"app.example.passkeys", alTestCert[:40]},
	} {
		_, err := load(bad[0], bad[1])
		if err == nil || !strings.Contains(err.Error(), envAndroidPackage) || !strings.Contains(err.Error(), envAndroidCertSHA256) {
			t.Errorf("%q: err = %v; want a refusal naming %s and %s", bad, err, envAndroidPackage, envAndroidCertSHA256)
		}
	}
}

// TestTheServerServesAssetLinksFromItsConfiguration drives the binary's own
// listen path (startServer): the configuration reaches the route, open, as
// application/json; without it the route is 404.
func TestTheServerServesAssetLinksFromItsConfiguration(t *testing.T) {
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fetch := func(base string) (*http.Response, string) {
		t.Helper()
		resp, err := noRedirect.Get(base + "/.well-known/assetlinks.json")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	cfg := testConfig()
	cfg.AndroidPackage = "app.example.passkeys"
	cfg.AndroidCertSHA256 = alTestCert
	resp, body := fetch(startServer(t, cfg))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("configured: status %d (Location %q), want 200", resp.StatusCode, resp.Header.Get("Location"))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("configured: Content-Type %q, want application/json", ct)
	}
	for _, want := range []string{
		`"package_name":"app.example.passkeys"`,
		`"01:23:45:67:89:AB:CD:EF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:0F:1E:2D:3C:4B:5A:69:78"`,
		`"delegate_permission/common.get_login_creds"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("configured: body lacks %s: %s", want, body)
		}
	}

	resp, body = fetch(startServer(t, testConfig()))
	if resp.StatusCode != http.StatusNotFound || strings.Contains(body, "android_app") {
		t.Fatalf("unset: status %d body %q, want 404 and no statement", resp.StatusCode, body)
	}
}

// TestTheAndroidPasskeysBannerLineMatchesWhatIsServed: the line follows the
// wired statement — serving with it, OFF without it — so an inverted branch
// fails here, not only as "the two renders differ".
func TestTheAndroidPasskeysBannerLineMatchesWhatIsServed(t *testing.T) {
	line := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "android passkeys:") {
				return l
			}
		}
		t.Fatalf("no \"android passkeys:\" line:\n%s", out)
		return ""
	}
	on := config{AndroidPackage: "app.example.passkeys", AndroidCertSHA256: alTestCert}
	body, err := on.assetLinks()
	if err != nil {
		t.Fatal(err)
	}
	if l := line(renderBanner(t, on, api.Extensions{AssetLinks: body})); !strings.Contains(l, " serves ") ||
		strings.Contains(l, "OFF") || !strings.Contains(l, "app.example.passkeys") {
		t.Errorf("configured: %q", l)
	}
	if l := line(renderBanner(t, config{}, api.Extensions{})); !strings.Contains(l, "OFF") || strings.Contains(l, " serves ") {
		t.Errorf("unset: %q", l)
	}
}
