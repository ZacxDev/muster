package askq

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestTheHookRefusesARedirectWithNoClientConfigured: Hook.Run with Client nil
// (the fallback that used to be http.DefaultClient) against a muster that 302s
// to a second server. The second server gets NO request (it would have been
// handed the bearer token), the redirecting one was reached, and Run reports
// the call not taken, logging the refused host. The second server answers
// 201, so a hook that followed the redirect would report success.
func TestTheHookRefusesARedirectWithNoClientConfigured(t *testing.T) {
	var targetHits, fromHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(target.Close)
	from := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromHits.Add(1)
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(from.Close)

	var logs []string
	h := Hook{Base: from.URL, Path: "/mirror/ask", Auth: func(r *http.Request) { r.Header.Set("Authorization", "Bearer mmt_test") },
		Client: nil, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	if h.Run(context.Background(), gatePayload(t)) {
		t.Fatal("Run reported the call taken through a redirect")
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("the redirect target got %d requests, want 0", n)
	}
	if n := fromHits.Load(); n != 1 {
		t.Fatalf("the redirecting server got %d requests, want 1", n)
	}
	if host := strings.TrimPrefix(target.URL, "http://"); !strings.Contains(strings.Join(logs, " "), host) {
		t.Fatalf("the log %q does not name the refused host %s", logs, host)
	}
}
