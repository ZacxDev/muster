package api

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func deviceWebServer(t *testing.T, ext Extensions) http.Handler {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	s.UseExtensions(ext)
	return s.Handler()
}

func TestParseSSOOrigins(t *testing.T) {
	// Every accepted input shape, and the exact normalised list it yields
	// (pinned as literals, never re-derived from the parser).
	good := map[string][]string{
		"":                          {},
		"   ":                       {},
		"https://auth.example.test": {"https://auth.example.test"},
		" https://Auth.Example.TEST , https://idp.example.test:8443": {
			"https://auth.example.test", "https://idp.example.test:8443"},
		// :443 is the default port, so it is the same origin as none.
		"https://auth.example.test:443,https://auth.example.test": {"https://auth.example.test"},
		"HTTPS://auth.example.test":                               {"https://auth.example.test"},
		"https://192.0.2.7:9443":                                  {"https://192.0.2.7:9443"},
		"https://[2001:db8::1]":                                   {"https://[2001:db8::1]"},
		"https://a-b.example.test,https://c.example.test,https://d.example.test,https://e.example.test," +
			"https://f.example.test,https://g.example.test,https://h.example.test,https://i.example.test": {
			"https://a-b.example.test", "https://c.example.test", "https://d.example.test", "https://e.example.test",
			"https://f.example.test", "https://g.example.test", "https://h.example.test", "https://i.example.test"},
	}
	for in, want := range good {
		got, err := ParseSSOOrigins(in)
		if err != nil {
			t.Errorf("ParseSSOOrigins(%q) refused: %v", in, err)
			continue
		}
		if got == nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseSSOOrigins(%q) = %#v; want %#v", in, got, want)
		}
	}

	// Each refusal is pinned to the reason its own guard gives, so a mutant that
	// deletes one guard cannot stay green on another guard's refusal.
	bad := map[string][2]string{
		"http":                {"http://auth.example.test", "the scheme must be https"},
		"no scheme":           {"auth.example.test", "the scheme must be https"},
		"other scheme":        {"wss://auth.example.test", "the scheme must be https"},
		"wildcard alone":      {"*", "the scheme must be https"},
		"bare slash path":     {"https://auth.example.test/", "it has a path"},
		"path":                {"https://auth.example.test/login", "it has a path"},
		"query":               {"https://auth.example.test?x=1", "it has a query"},
		"empty query":         {"https://auth.example.test?", "it has a query"},
		"fragment":            {"https://auth.example.test#x", "it has a fragment"},
		"empty fragment":      {"https://auth.example.test#", "it has a fragment"},
		"userinfo":            {"https://user@auth.example.test", "it has user info"},
		"userinfo password":   {"https://user:pw@auth.example.test", "it has user info"},
		"escaped host":        {"https://auth%2eexample.test", "it has a percent-escape"},
		"empty entry":         {"https://auth.example.test,,https://idp.example.test", "an empty entry"},
		"trailing comma":      {"https://auth.example.test,", "an empty entry"},
		"empty port":          {"https://auth.example.test:", "an empty port"},
		"port zero":           {"https://auth.example.test:0", "the port must be 1-65535"},
		"port too big":        {"https://auth.example.test:65536", "the port must be 1-65535"},
		"port leading zero":   {"https://auth.example.test:0443", "the port must be 1-65535"},
		"port not a number":   {"https://auth.example.test:https", "invalid port"},
		"no host":             {"https://", "no host"},
		"opaque":              {"https:auth.example.test", "no host"},
		"wildcard host":       {"https://*.example.test", "the host is not a host name"},
		"underscore host":     {"https://auth_x.example.test", "the host is not a host name"},
		"double dot host":     {"https://auth..example.test", "the host is not a host name"},
		"trailing dot host":   {"https://auth.example.test.", "the host is not a host name"},
		"hyphen-led label":    {"https://-auth.example.test", "the host is not a host name"},
		"space inside":        {"https://auth .example.test", "invalid character"},
		"backslash":           {`https://auth.example.test\evil.example.test`, "invalid character"},
		"one bad among goods": {"https://auth.example.test,http://idp.example.test", "the scheme must be https"},
		"nine origins": {"https://a.example.test,https://b.example.test,https://c.example.test,https://d.example.test," +
			"https://e.example.test,https://f.example.test,https://g.example.test,https://h.example.test,https://i.example.test",
			"at most 8"},
	}
	for name, c := range bad {
		got, err := ParseSSOOrigins(c[0])
		if err == nil || !strings.Contains(err.Error(), c[1]) {
			t.Errorf("%s: ParseSSOOrigins(%q) = %#v, %v; want a refusal saying %q", name, c[0], got, err, c[1])
		}
	}
	// A refusal names the entry, so the operator can find it.
	if _, err := ParseSSOOrigins("https://ok.example.test, http://plain.example.test"); err == nil ||
		!strings.Contains(err.Error(), `"http://plain.example.test"`) {
		t.Errorf("the refusal does not name the bad entry: %v", err)
	}
}

func getDeviceWeb(t *testing.T, h http.Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, DeviceWebPath, nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestTheDeviceWebRouteServesOnlyTheList: the exact body PR #19's parser reads
// (`{"sso_origins":[...]}`), open, no-store — and nothing else, even with every
// other Android setting configured beside it.
func TestTheDeviceWebRouteServesOnlyTheList(t *testing.T) {
	h := deviceWebServer(t, Extensions{
		SSOOrigins:   []string{"https://auth.example.test", "https://idp.example.test:8443"},
		AssetLinks:   []byte(`[{"relation":["x"]}]`),
		AllowPINKeys: true,
	})
	rec := getDeviceWeb(t, h, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (Location %q), want 200 with no credential", rec.Code, rec.Header().Get("Location"))
	}
	const want = `{"sso_origins":["https://auth.example.test","https://idp.example.test:8443"]}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("body\n got %q\nwant %q", got, want)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil || len(keys) != 1 {
		t.Errorf("the body has keys %v (err %v); want exactly sso_origins", keys, err)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("the open route set a cookie: %q", rec.Header().Get("Set-Cookie"))
	}
}

// TestTheDeviceWebRouteDefaultsToAnEmptyList: unset is `[]`, never null and
// never 404 (the app reads a 404 as an older muster, which is the same "none"
// but a different claim), and with no database too.
func TestTheDeviceWebRouteDefaultsToAnEmptyList(t *testing.T) {
	for name, ext := range map[string]Extensions{
		"zero extensions": {},
		"empty list":      {SSOOrigins: []string{}},
	} {
		rec := getDeviceWeb(t, deviceWebServer(t, ext), nil)
		if rec.Code != http.StatusOK || rec.Body.String() != `{"sso_origins":[]}`+"\n" {
			t.Errorf("%s: status %d body %q; want 200 {\"sso_origins\":[]}", name, rec.Code, rec.Body.String())
		}
	}
}

// TestTheDeviceWebRouteTakesNoCredential: it is read before the first sign-in,
// so no credential of any kind is asked for, and none presented changes the
// answer. CONTROL: the sibling device route on the same server refuses the
// same anonymous request, so "200" here is the route's openness, not a server
// that admits everything.
func TestTheDeviceWebRouteTakesNoCredential(t *testing.T) {
	h := deviceWebServer(t, Extensions{SSOOrigins: []string{"https://auth.example.test"}})
	const want = `{"sso_origins":["https://auth.example.test"]}` + "\n"

	ctl := httptest.NewRecorder()
	h.ServeHTTP(ctl, httptest.NewRequest(http.MethodGet, deviceConfigPath, nil))
	if ctl.Code == http.StatusOK {
		t.Fatalf("CONTROL FAILED: anonymous %s answered 200, so this server admits anything", deviceConfigPath)
	}

	for name, mutate := range map[string]func(*http.Request){
		"anonymous":           nil,
		"a junk bearer":       func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") },
		"a device-shaped one": func(r *http.Request) { r.Header.Set("Authorization", "Bearer mdv_"+strings.Repeat("A", 43)) },
		"a junk cookie":       func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "muster_session", Value: "x"}) },
	} {
		rec := getDeviceWeb(t, h, mutate)
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s: status %d body %q; want 200 %s", name, rec.Code, rec.Body.String(), want)
		}
	}
}
