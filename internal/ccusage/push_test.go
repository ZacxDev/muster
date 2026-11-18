package ccusage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
)

const (
	pushTestToken = "push-test-token-SENTINEL-7f3a9c"
	pushTestBody  = `{"source_instance":"example","read_at":"2026-11-10T18:00:00Z","accounts":[{"account":"BODY-SENTINEL-alpha"}]}`
)

var pushReasons = []string{PushReasonBuild, PushReasonTimeout, PushReasonTransport, PushReason4xx, PushReason5xx, PushReasonOther, PushReasonCanceled}

func pushCounts() map[string]float64 {
	out := map[string]float64{}
	for _, r := range pushReasons {
		out[r] = testutil.ToFloat64(metrics.CCUsagePushFailures.WithLabelValues(r))
	}
	return out
}

// pushTarget is a dashboard answering status (after delay), recording each
// request. Its answer body echoes the token, so a pusher that logged the
// answer would leak it.
type pushTarget struct {
	mu     sync.Mutex
	status int
	delay  time.Duration
	answer string // the answer body; "" echoes the Authorization header
	reqs   []*http.Request
	bodies [][]byte
}

func (p *pushTarget) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.reqs, p.bodies = append(p.reqs, r), append(p.bodies, b)
	p.mu.Unlock()
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	if p.status == http.StatusFound {
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
		return
	}
	w.WriteHeader(p.status)
	if p.answer != "" {
		_, _ = w.Write([]byte(p.answer))
		return
	}
	_, _ = w.Write([]byte("echo " + r.Header.Get("Authorization")))
}

func (p *pushTarget) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func newPusher(url string, logs io.Writer) *Pusher {
	return &Pusher{URL: url, Token: pushTestToken, Logger: log.New(logs, "", 0), Timeout: 200 * time.Millisecond,
		Now:  func() time.Time { return time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC) },
		Body: func(context.Context, time.Time) ([]byte, error) { return []byte(pushTestBody), nil }}
}

// TestAPushIsOnePUTWithTheBearerAndTheBody: a 204 is success (nothing
// counted); the request is one PUT carrying the bearer and the body as built.
func TestAPushIsOnePUTWithTheBearerAndTheBody(t *testing.T) {
	tgt := &pushTarget{status: http.StatusNoContent}
	srv := httptest.NewServer(tgt)
	defer srv.Close()
	before := pushCounts()
	var logs bytes.Buffer
	if r := newPusher(srv.URL+"/api/usage", &logs).Push(context.Background()); r != "" {
		t.Fatalf("a 204 reported %q", r)
	}
	if tgt.count() != 1 {
		t.Fatalf("%d requests, want 1", tgt.count())
	}
	req := tgt.reqs[0]
	if req.Method != http.MethodPut || req.URL.Path != "/api/usage" || req.Header.Get("Authorization") != "Bearer "+pushTestToken ||
		req.Header.Get("Content-Type") != "application/json" || string(tgt.bodies[0]) != pushTestBody {
		t.Fatalf("request %s %s auth=%q ct=%q body=%s", req.Method, req.URL.Path, req.Header.Get("Authorization"),
			req.Header.Get("Content-Type"), tgt.bodies[0])
	}
	after := pushCounts()
	for _, r := range pushReasons {
		if after[r] != before[r] {
			t.Fatalf("a 204 counted a %s failure", r)
		}
	}
}

// TestAFailedPushIsCountedByReasonAndNotRetried: each failure is one request
// (no retry inside the cycle), one count under exactly its reason, and a log
// line that carries neither the token nor the body — not even when the
// dashboard's answer echoes the token.
func TestAFailedPushIsCountedByReasonAndNotRetried(t *testing.T) {
	cases := []struct {
		name   string
		status int
		delay  time.Duration
		want   string
		reqs   int
	}{
		{"401", http.StatusUnauthorized, 0, PushReason4xx, 1},
		{"422", http.StatusUnprocessableEntity, 0, PushReason4xx, 1},
		{"500", http.StatusInternalServerError, 0, PushReason5xx, 1},
		{"503", http.StatusServiceUnavailable, 0, PushReason5xx, 1},
		{"200 is not 204", http.StatusOK, 0, PushReasonOther, 1},
		{"a redirect is not followed", http.StatusFound, 0, PushReasonOther, 1},
		{"no answer in time", http.StatusNoContent, time.Second, PushReasonTimeout, 1},
	}
	var logs bytes.Buffer
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt := &pushTarget{status: c.status, delay: c.delay}
			srv := httptest.NewServer(tgt)
			defer srv.Close()
			before := pushCounts()
			if r := newPusher(srv.URL, &logs).Push(context.Background()); r != c.want {
				t.Fatalf("reason %q, want %q", r, c.want)
			}
			if n := tgt.count(); n != c.reqs {
				t.Fatalf("%d requests, want %d", n, c.reqs)
			}
			after := pushCounts()
			for _, r := range pushReasons {
				want := before[r]
				if r == c.want {
					want++
				}
				if after[r] != want {
					t.Errorf("%s: count %v → %v, want %v", r, before[r], after[r], want)
				}
			}
		})
	}

	t.Run("transport", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		u := srv.URL
		srv.Close() // nothing listens there now
		if r := newPusher(u, &logs).Push(context.Background()); r != PushReasonTransport {
			t.Fatalf("reason %q, want %q", r, PushReasonTransport)
		}
	})
	t.Run("build", func(t *testing.T) {
		tgt := &pushTarget{status: http.StatusNoContent}
		srv := httptest.NewServer(tgt)
		defer srv.Close()
		p := newPusher(srv.URL, &logs)
		p.Body = func(context.Context, time.Time) ([]byte, error) { return nil, errors.New("store down") }
		if r := p.Push(context.Background()); r != PushReasonBuild || tgt.count() != 0 {
			t.Fatalf("reason %q with %d requests, want %q and none", r, tgt.count(), PushReasonBuild)
		}
	})

	out := logs.String()
	if !strings.Contains(out, "HTTP 401") || !strings.Contains(out, "timeout") {
		t.Fatalf("positive control: the failures were not logged at all: %s", out)
	}
	for _, secret := range []string{pushTestToken, "BODY-SENTINEL"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the log carries %q:\n%s", secret, out)
		}
	}
}

// TestCheckPushURL: https with a host; http only to loopback; no userinfo.
func TestCheckPushURL(t *testing.T) {
	for _, ok := range []string{"https://dash.example.com/api/v1/usage", "https://dash.example.com:8443/x",
		"http://127.0.0.1:8080/x", "http://localhost/x", "http://[::1]:9/x"} {
		if err := CheckPushURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://dash.example.com/x", "http://192.0.2.1/x", "dash.example.com/x", "/api/usage",
		"https://", "ftp://dash.example.com/x", "https://user:pw@dash.example.com/x", "http://localhost.example.com/x", "::"} {
		if err := CheckPushURL(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

// TestPushInstance: explicit wins (≤64 characters), else the public URL's
// host, else "muster".
func TestPushInstance(t *testing.T) {
	for _, c := range []struct{ explicit, public, want string }{
		{"prod-east", "https://muster.example.com", "prod-east"},
		{strings.Repeat("a", 64), "", strings.Repeat("a", 64)},
		{"", "https://muster.example.com:8443/base", "muster.example.com"},
		{"", "", "muster"},
		{"", "https://" + strings.Repeat("h", 65) + ".example.com", "muster"},
	} {
		if got, err := PushInstance(c.explicit, c.public); err != nil || got != c.want {
			t.Errorf("PushInstance(%q, %q) = %q, %v; want %q", c.explicit, c.public, got, err, c.want)
		}
	}
	if _, err := PushInstance(strings.Repeat("a", 65), ""); err == nil {
		t.Fatal("a 65-character instance was accepted")
	}
}

// countingSource is a Source with no accounts that counts its reads: one per
// pass.
type countingSource struct {
	mu sync.Mutex
	n  int
}

func (c *countingSource) Accounts(context.Context, int64) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return map[string]string{}, nil
}

// TestRunCallsAfterPassOncePerPass: the poll loop makes exactly one AfterPass
// call per pass — never two for one pass, never one without a pass.
func TestRunCallsAfterPassOncePerPass(t *testing.T) {
	src := &countingSource{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var seen []int // the pass count at each AfterPass
	p := &Poller{Source: src, Store: &memStore{}, Owner: 1, Interval: 5 * time.Millisecond,
		AfterPass: func(context.Context) {
			src.mu.Lock()
			n := src.n
			src.mu.Unlock()
			mu.Lock()
			seen = append(seen, n)
			if len(seen) == 4 {
				cancel()
			}
			mu.Unlock()
		}}
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if len(seen) != 4 || src.n != 4 {
		t.Fatalf("%d AfterPass calls over %d passes, want 4 and 4", len(seen), src.n)
	}
	for i, n := range seen {
		if n != i+1 {
			t.Fatalf("AfterPass call %d came after pass %d (calls %v), want one per pass", i+1, n, seen)
		}
	}
}

// TestA4xxAnswerIsLoggedAsOneSanitizedLine: a dashboard's 4xx answer (a cap
// exceeded) reaches the log as one line: its sentence kept, control
// characters made spaces, the token redacted, cut at 200 runes. The request
// body never appears.
func TestA4xxAnswerIsLoggedAsOneSanitizedLine(t *testing.T) {
	answer := "too many accounts: 21 > 20\nsecond line\x07 echo Bearer " + pushTestToken + " " + strings.Repeat("é", 400)
	tgt := &pushTarget{status: http.StatusBadRequest, answer: answer}
	srv := httptest.NewServer(tgt)
	defer srv.Close()
	var logs bytes.Buffer
	if r := newPusher(srv.URL, &logs).Push(context.Background()); r != PushReason4xx {
		t.Fatalf("reason %q, want %q", r, PushReason4xx)
	}
	out := logs.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d log lines, want one: %q", len(lines), out)
	}
	line := lines[0]
	if !strings.Contains(line, "HTTP 400: too many accounts: 21 > 20 second line  echo Bearer [redacted] é") {
		t.Fatalf("the answer is not in the log as one sanitized line: %q", line)
	}
	for _, secret := range []string{pushTestToken, "BODY-SENTINEL", "\x07"} {
		if strings.Contains(line, secret) {
			t.Fatalf("the log carries %q: %q", secret, line)
		}
	}
	_, logged, _ := strings.Cut(line, "HTTP 400: ")
	if n := len([]rune(logged)); n != maxPushAnswerRunes+1 { // the runes kept, then "…"
		t.Fatalf("the logged answer is %d runes, want %d (cut, plus the ellipsis): %q", n, maxPushAnswerRunes+1, logged)
	}
	if !strings.HasSuffix(logged, "é…") {
		t.Fatalf("the cut is not on a rune boundary: %q", logged)
	}
}

// TestAPushCutShortByShutdownIsCanceledNotTimeout: the parent context ending
// mid-push (muster shutting down) counts as canceled, never timeout.
func TestAPushCutShortByShutdownIsCanceledNotTimeout(t *testing.T) {
	tgt := &pushTarget{status: http.StatusNoContent, delay: 300 * time.Millisecond}
	srv := httptest.NewServer(tgt)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	p := newPusher(srv.URL, io.Discard)
	p.Timeout = 5 * time.Second
	before := pushCounts()
	if r := p.Push(ctx); r != PushReasonCanceled {
		t.Fatalf("reason %q, want %q", r, PushReasonCanceled)
	}
	if after := pushCounts(); after[PushReasonCanceled] != before[PushReasonCanceled]+1 || after[PushReasonTimeout] != before[PushReasonTimeout] {
		t.Fatalf("counts %v → %v, want one canceled and no timeout", before, after)
	}
}
