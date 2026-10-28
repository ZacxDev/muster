package api

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// TestThePublicURLHostIsMustersOwnOriginBehindAProxy: behind a proxy that
// rewrites Host, the browser's Origin names the PUBLIC host while r.Host names
// the upstream one. Extensions.PublicURL is what makes that Origin muster's
// own — for the WebSockets (wsAllowedOrigins) and for every state-changing
// request (requireSameOrigin). Without it the same request is refused, which
// is the control that the acceptance comes from PublicURL and not from r.Host.
//
// (This input was GitHubOAuth.BaseURL until the OAuth connection was removed;
// nothing pinned it on either end then.)
func TestThePublicURLHostIsMustersOwnOriginBehindAProxy(t *testing.T) {
	const public = "https://muster.example.test"
	serve := func(publicURL string) (code int, origins []string) {
		s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{PublicURL: publicURL})
		r := httptest.NewRequest(http.MethodPost, "/tasks/1/complete", nil)
		r.Host = "upstream.invalid:8105" // what the proxy forwarded
		r.Header.Set("Origin", public)
		rec := httptest.NewRecorder()
		reached := false
		s.requireSameOrigin(func(http.ResponseWriter, *http.Request) { reached = true })(rec, r)
		if reached {
			return http.StatusOK, s.wsAllowedOrigins(r)
		}
		return rec.Code, s.wsAllowedOrigins(r)
	}

	code, origins := serve(public)
	if code != http.StatusOK {
		t.Errorf("with PublicURL %s, a POST whose Origin is that host (Host rewritten by a proxy) was refused: %d", public, code)
	}
	if !slices.Contains(origins, "muster.example.test") || !slices.Contains(origins, "upstream.invalid:8105") {
		t.Errorf("wsAllowedOrigins = %v; want both the request's Host and PublicURL's host", origins)
	}

	// Control: no PublicURL, the same request is cross-origin.
	if code, origins := serve(""); code != http.StatusForbidden || slices.Contains(origins, "muster.example.test") {
		t.Errorf("without PublicURL: %d %v; want 403 and no public host", code, origins)
	}
}
