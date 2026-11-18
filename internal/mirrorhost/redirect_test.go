package mirrorhost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// 🔴 THE MIRROR NEVER FOLLOWS A REDIRECT WITH ITS TOKEN. net/http re-sends
// the Authorization header on a redirect to the same hostname on any port,
// so each test below 302s (or 307s) from one 127.0.0.1 server to ANOTHER
// 127.0.0.1 server — exactly the case net/http would hand the token to — and
// asserts the second server is never asked at all.
//
// These tests deliberately do not name internal/noredirect: they pin the
// behaviour, so they run unchanged against a tree without the fix.

// redirectTarget counts every request it gets and the Authorization headers.
type redirectTarget struct {
	mu    sync.Mutex
	hits  int
	auths []string
	srv   *httptest.Server
}

func newRedirectTarget(t *testing.T) *redirectTarget {
	t.Helper()
	rt := &redirectTarget{}
	rt.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.mu.Lock()
		rt.hits++
		rt.auths = append(rt.auths, r.Header.Get("Authorization"))
		rt.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(rt.srv.Close)
	return rt
}

func (rt *redirectTarget) host(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(rt.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// mustBeUntouched fails unless the target received no request and so saw no
// Authorization header.
func (rt *redirectTarget) mustBeUntouched(t *testing.T, what string) {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.hits != 0 || len(rt.auths) != 0 {
		t.Fatalf("%s: the redirect target got %d request(s), Authorization %q — the token followed the redirect", what, rt.hits, rt.auths)
	}
}

// redirectingMuster answers every request with code to target's URL, and
// counts what it was asked.
func redirectingMuster(t *testing.T, code int, target string) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		http.Redirect(w, r, target+r.URL.Path, code)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// TestTheTunnelDialRefusesARedirect: a muster that answers the tunnel's
// WebSocket connect with a redirect gets a failed connect, the Location is
// never dialled, and the error names the Location's host.
func TestTheTunnelDialRefusesARedirect(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		target := newRedirectTarget(t)
		from, asked := redirectingMuster(t, code, target.srv.URL)
		c, err := New(Config{URL: from.URL, AllowHTTP: true, Token: mirror.TokenPrefix + "redirect-test-token", Host: "laptop", Backend: Tmux{}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = c.RunOnce(ctx)
		cancel()
		if err == nil {
			t.Fatalf("%d: RunOnce succeeded through a redirect", code)
		}
		if *asked == 0 {
			t.Fatalf("%d: the redirecting muster was never dialled (%v); the test proves nothing", code, err)
		}
		target.mustBeUntouched(t, http.StatusText(code))
		if host := target.host(t); !strings.Contains(err.Error(), host) || !strings.Contains(err.Error(), "redirect") {
			t.Fatalf("%d: the error does not name the refused redirect to %s: %v", code, host, err)
		}
	}
}

// TestTheMCPProxyRefusesARedirect: the MCP proxy forwards with the mirror
// token; a muster that answers with a redirect gets a 502 naming the
// Location's host, and the Location is never asked — with the default client
// AND with a configured client that would follow redirects itself.
func TestTheMCPProxyRefusesARedirect(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		for name, client := range map[string]*http.Client{"the default client": nil, "a configured client": {}} {
			what := http.StatusText(code) + ", " + name
			target := newRedirectTarget(t)
			from, asked := redirectingMuster(t, code, target.srv.URL)
			p := proxyFor(from.URL)
			p.Client = client
			w := postBody(t, p, map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX},
				`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			if *asked != 1 {
				t.Fatalf("%s: the redirecting muster was asked %d times; the test proves nothing", what, *asked)
			}
			target.mustBeUntouched(t, what)
			if host := target.host(t); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), host) {
				t.Fatalf("%s: %d %s, want a 502 naming %s", what, w.Code, w.Body, host)
			}
		}
	}
}
