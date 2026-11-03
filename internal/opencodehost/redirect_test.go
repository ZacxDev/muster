package opencodehost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/noredirect"
)

// TestTheClientRefusesARedirectFromTheServer: NewClient + Info against a
// loopback server that 302s to a second loopback server (same hostname,
// another port — where net/http would re-send the Basic credential). The
// second server gets NO request, the redirecting one was reached, and Info
// fails naming the refused host. The second server answers like opencode, so
// a client that followed the redirect would succeed.
func TestTheClientRefusesARedirectFromTheServer(t *testing.T) {
	var targetHits, fromHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"0.0.0-test"}`))
	}))
	t.Cleanup(target.Close)
	from := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromHits.Add(1)
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(from.Close)

	c, err := NewClient(from.URL, func() string { return "test-secret-not-real" })
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.Info(context.Background())
	var refused *noredirect.Error
	if !errors.As(err, &refused) {
		t.Fatalf("Info = %q, %v; want a *noredirect.Error", v, err)
	}
	if u, _ := url.Parse(target.URL); refused.Host != u.Host {
		t.Fatalf("refused host %q, want %q", refused.Host, u.Host)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("the redirect target got %d requests, want 0", n)
	}
	if n := fromHits.Load(); n != 1 {
		t.Fatalf("the redirecting server got %d requests, want 1", n)
	}
}
