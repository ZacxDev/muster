package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAskRefusesARedirectFromMuster: `muster-mirror ask` with an http:// URL
// and -allow-http, against a muster that 302s to a second server. The second
// server gets NO request (it would have been handed the mirror token), the
// redirecting one was reached, nothing is printed, and stderr names the
// refused host. The second server answers 201, so a hook that followed the
// redirect would register there.
func TestAskRefusesARedirectFromMuster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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

	var out, errb bytes.Buffer
	if code := runAsk([]string{"-url", from.URL, "-allow-http"}, env(map[string]string{envToken: "mmt_x"}), bytes.NewReader(askPayload(t)), &out, &errb); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out.Len() != 0 {
		t.Fatalf("the hook printed %q", out.String())
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("the redirect target got %d requests, want 0", n)
	}
	if n := fromHits.Load(); n != 1 {
		t.Fatalf("the redirecting server got %d requests, want 1", n)
	}
	if host := strings.TrimPrefix(target.URL, "http://"); !strings.Contains(errb.String(), host) || !strings.Contains(errb.String(), "refused to follow a redirect") {
		t.Fatalf("stderr %q does not name the refused redirect to %s", errb.String(), host)
	}
}
