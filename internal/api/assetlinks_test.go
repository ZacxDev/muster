package api

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Two fingerprints, every byte distinct between them, given in the two input
// shapes the variable accepts: bare lower-case hex, and colon-separated mixed
// case. The served form is pinned as a literal below, never re-derived.
const (
	alCertA = "0123456789abcdef00112233445566778899aabbccddeeff0f1e2d3c4b5a6978"
	alCertB = "F0:E1:D2:c3:b4:a5:96:87:78:69:5A:4B:3C:2D:1E:0F:FE:DC:BA:98:76:54:32:10:01:23:45:67:89:AB:CD:EF"
)

// alWant is the exact body for package app.example.passkeys with both certs.
const alWant = `[{"relation":["delegate_permission/common.get_login_creds"],` +
	`"target":{"namespace":"android_app","package_name":"app.example.passkeys","sha256_cert_fingerprints":[` +
	`"01:23:45:67:89:AB:CD:EF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:0F:1E:2D:3C:4B:5A:69:78",` +
	`"F0:E1:D2:C3:B4:A5:96:87:78:69:5A:4B:3C:2D:1E:0F:FE:DC:BA:98:76:54:32:10:01:23:45:67:89:AB:CD:EF"]}}]`

func assetLinksServer(t *testing.T, body []byte) http.Handler {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{AssetLinks: body})
	return s.Handler()
}

func TestBuildAssetLinks(t *testing.T) {
	got, err := BuildAssetLinks(" app.example.passkeys ", alCertA+" , "+alCertB+","+strings.ToUpper(alCertA))
	if err != nil {
		t.Fatalf("BuildAssetLinks: %v", err)
	}
	if string(got) != alWant {
		t.Fatalf("body\n got %s\nwant %s", got, alWant)
	}

	if got, err := BuildAssetLinks("", " "); got != nil || err != nil {
		t.Fatalf("both unset = %q, %v; want nil, nil (not configured)", got, err)
	}

	// Half a pair is named as such, not as a malformed value.
	for _, in := range [][2]string{{"app.example.passkeys", ""}, {"", alCertA}} {
		if _, err := BuildAssetLinks(in[0], in[1]); err == nil || !strings.Contains(err.Error(), "only one is set") {
			t.Errorf("BuildAssetLinks(%q, %q) err = %v; want the pair refusal", in[0], in[1], err)
		}
	}

	refused := map[string][2]string{
		"package without certs":   {"app.example.passkeys", ""},
		"certs without package":   {"", alCertA},
		"one-segment package":     {"passkeys", alCertA},
		"package segment digit":   {"app.9example", alCertA},
		"short fingerprint":       {"app.example.passkeys", alCertA[:62]},
		"long fingerprint":        {"app.example.passkeys", alCertA + "00"},
		"sha-1 fingerprint":       {"app.example.passkeys", "0123456789abcdef0123456789abcdef01234567"},
		"non-hex fingerprint":     {"app.example.passkeys", "zz" + alCertA[2:]},
		"empty entry in the list": {"app.example.passkeys", alCertA + ",,"},
	}
	for name, in := range refused {
		if got, err := BuildAssetLinks(in[0], in[1]); err == nil {
			t.Errorf("%s: BuildAssetLinks(%q, %q) = %s, want an error", name, in[0], in[1], got)
		}
	}
}

// TestAssetLinksIsServedOpenAsJSON: Android's verifier fetches the file with
// no session and accepts only 200, application/json, no redirect.
func TestAssetLinksIsServedOpenAsJSON(t *testing.T) {
	body, err := BuildAssetLinks("app.example.passkeys", alCertA+","+alCertB)
	if err != nil {
		t.Fatal(err)
	}
	h := assetLinksServer(t, body)
	ck := loginCookie(t, h)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, withSession := range []bool{false, true} {
			req := httptest.NewRequest(method, "/.well-known/assetlinks.json", nil)
			if withSession {
				req.AddCookie(ck)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			label := method
			if withSession {
				label += " (signed in)"
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d (Location %q), want 200", label, rec.Code, rec.Header().Get("Location"))
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("%s Content-Type = %q, want application/json", label, ct)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
				t.Errorf("%s Cache-Control = %q, want public, max-age=300", label, cc)
			}
			if sc := rec.Header().Values("Set-Cookie"); len(sc) > 0 {
				t.Errorf("%s sets a cookie: %v", label, sc)
			}
			if method == http.MethodGet && rec.Body.String() != alWant {
				t.Errorf("%s body\n got %s\nwant %s", label, rec.Body.String(), alWant)
			}
		}
	}
}

// TestAssetLinksIs404WhenUnset: no configuration, no statement — and a 404,
// not the login redirect, for a visitor with no session.
func TestAssetLinksIs404WhenUnset(t *testing.T) {
	h := assetLinksServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/assetlinks.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unset = %d (Location %q), want 404", rec.Code, rec.Header().Get("Location"))
	}
	if strings.Contains(rec.Body.String(), "android_app") {
		t.Fatalf("unset served a statement: %s", rec.Body.String())
	}
}
