package ccusage

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ccpool"
)

// scripted is a Fetcher answering a queue of results, counting calls.
type scripted struct {
	mu    sync.Mutex
	queue []Result
	calls int
	toks  []string
}

func (s *scripted) Fetch(_ context.Context, token string) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.toks = append(s.toks, token)
	if len(s.queue) == 0 {
		return Result{Status: http.StatusOK, Reading: Reading{Windows: map[string]Window{SevenDay: {Utilization: f(1)}}}}
	}
	r := s.queue[0]
	s.queue = s.queue[1:]
	return r
}

func f(v float64) *float64 { return &v }

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

const iv = 5 * time.Minute

func good(u float64) Result {
	return Result{Status: http.StatusOK, Reading: Reading{Windows: map[string]Window{FiveHour: {Utilization: f(u)}}}}
}

// TestStepMarksA401AndDoesNotSpin: a 401 is recorded on the account, and the
// same token is NOT called again on later passes — only a replaced token is.
func TestStepMarksA401AndDoesNotSpin(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		fx := &scripted{queue: []Result{{Status: status}}}
		r, ok := Step(context.Background(), Row{}, "alpha", "tok-a", t0, iv, fx)
		want := map[int]string{401: KindUnauthorized, 403: KindForbidden}[status]
		if !ok || r.ErrorKind != want || r.TokenFP != ccpool.Fingerprint("tok-a") || fx.calls != 1 {
			t.Fatalf("%d: row %+v ok=%v calls=%d", status, r, ok, fx.calls)
		}
		for i := 1; i <= 5; i++ {
			if _, ok := Step(context.Background(), r, "alpha", "tok-a", t0.Add(time.Duration(i)*time.Hour), iv, fx); ok {
				t.Fatalf("%d: pass %d attempted again on the refused token", status, i)
			}
		}
		if fx.calls != 1 {
			t.Fatalf("%d: %d calls on a refused token, want 1", status, fx.calls)
		}
		r2, ok := Step(context.Background(), r, "alpha", "tok-b", t0.Add(time.Hour), iv, fx)
		if !ok || fx.calls != 2 || r2.ErrorKind != "" || r2.TokenFP != ccpool.Fingerprint("tok-b") {
			t.Fatalf("%d: a replaced token was not read: %+v ok=%v calls=%d", status, r2, ok, fx.calls)
		}
	}
}

// TestStepBacksOffOn429: a 429 sets NextAt (interval doubled per consecutive
// 429, at least Retry-After), no call is made before it, and a good answer
// clears the streak.
func TestStepBacksOffOn429(t *testing.T) {
	fx := &scripted{queue: []Result{{Status: 429}, {Status: 429}, {Status: 429, RetryAfter: 3 * time.Hour}}}
	r, _ := Step(context.Background(), Row{}, "alpha", "tok", t0, iv, fx)
	if r.ErrorKind != KindRateLimited || r.RateLimitStreak != 1 || !r.NextAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("first 429: %+v", r)
	}
	if _, ok := Step(context.Background(), r, "alpha", "tok", t0.Add(9*time.Minute), iv, fx); ok || fx.calls != 1 {
		t.Fatalf("called before NextAt (calls %d)", fx.calls)
	}
	at := t0.Add(10 * time.Minute)
	r, _ = Step(context.Background(), r, "alpha", "tok", at, iv, fx)
	if r.RateLimitStreak != 2 || !r.NextAt.Equal(at.Add(20*time.Minute)) {
		t.Fatalf("second 429: %+v", r)
	}
	at = r.NextAt
	r, _ = Step(context.Background(), r, "alpha", "tok", at, iv, fx)
	if r.RateLimitStreak != 3 || !r.NextAt.Equal(at.Add(3*time.Hour)) {
		t.Fatalf("third 429 with Retry-After 3h: %+v (want NextAt %v)", r, at.Add(3*time.Hour))
	}
	at = r.NextAt
	r, _ = Step(context.Background(), r, "alpha", "tok", at, iv, fx)
	if r.ErrorKind != "" || r.RateLimitStreak != 0 || !r.NextAt.IsZero() || !r.ReadAt.Equal(at) {
		t.Fatalf("good answer after 429s: %+v", r)
	}
	// A replaced token is read at once, even inside the old token's backoff,
	// and its first 429 counts from 1, not from the old token's streak.
	held := Row{Account: "alpha", TokenFP: ccpool.Fingerprint("tok"), ErrorKind: KindRateLimited, RateLimitStreak: 4, NextAt: t0.Add(5 * time.Hour)}
	fx.queue = []Result{{Status: 429}}
	r, ok := Step(context.Background(), held, "alpha", "tok-new", t0, iv, fx)
	if !ok || r.RateLimitStreak != 1 || !r.NextAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("a 429 on a replaced token: %+v ok=%v", r, ok)
	}
}

func TestBackoffBounds(t *testing.T) {
	for _, c := range []struct {
		streak int
		ra     time.Duration
		want   time.Duration
	}{{1, 0, 10 * time.Minute}, {2, 0, 20 * time.Minute}, {3, 0, 40 * time.Minute}, {1, time.Hour, time.Hour},
		{20, 0, MaxBackoff}, {1, 48 * time.Hour, MaxBackoff}, {0, 0, 10 * time.Minute}} {
		if got := Backoff(iv, c.streak, c.ra); got != c.want {
			t.Errorf("Backoff(5m, %d, %s) = %s, want %s", c.streak, c.ra, got, c.want)
		}
	}
}

// TestStepWithNoTokenMakesNoCall, and records it once.
func TestStepWithNoTokenMakesNoCall(t *testing.T) {
	fx := &scripted{}
	r, ok := Step(context.Background(), Row{}, "alpha", "  ", t0, iv, fx)
	if !ok || r.ErrorKind != KindNoToken || fx.calls != 0 {
		t.Fatalf("no token: %+v ok=%v calls=%d", r, ok, fx.calls)
	}
	if _, ok := Step(context.Background(), r, "alpha", "", t0.Add(time.Hour), iv, fx); ok || fx.calls != 0 {
		t.Fatalf("a second pass with no token wrote again or called (calls %d)", fx.calls)
	}
}

// TestAFailedReadKeepsTheLastGoodReading: a 5xx after a good reading keeps its
// windows and ReadAt, records the error, and does not back off.
func TestAFailedReadKeepsTheLastGoodReading(t *testing.T) {
	fx := &scripted{queue: []Result{good(42), {Status: 502}, {Err: errors.New("ccusage: request failed (timeout)")}}}
	r, _ := Step(context.Background(), Row{}, "alpha", "tok", t0, iv, fx)
	r, _ = Step(context.Background(), r, "alpha", "tok", t0.Add(iv), iv, fx)
	if r.ErrorKind != KindError || !strings.Contains(r.Error, "502") || !r.ReadAt.Equal(t0) || *r.Windows[FiveHour].Utilization != 42 || !r.NextAt.IsZero() {
		t.Fatalf("after a 502: %+v", r)
	}
	r, _ = Step(context.Background(), r, "alpha", "tok", t0.Add(2*iv), iv, fx)
	if r.ErrorKind != KindError || !strings.Contains(r.Error, "timeout") || *r.Windows[FiveHour].Utilization != 42 || !r.CheckedAt.Equal(t0.Add(2*iv)) {
		t.Fatalf("after a transport error: %+v", r)
	}
}

// memStore is Store in memory.
type memStore struct {
	mu   sync.Mutex
	rows map[int64]map[string]Row
}

func (m *memStore) Latest(_ context.Context, owner int64) (map[string]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Row{}
	for k, v := range m.rows[owner] {
		out[k] = v
	}
	return out, nil
}

func (m *memStore) Save(_ context.Context, owner int64, r Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[int64]map[string]Row{}
	}
	if m.rows[owner] == nil {
		m.rows[owner] = map[string]Row{}
	}
	m.rows[owner][r.Account] = r
	return nil
}

// TestPassReadsEveryAccountOfTheOwnerOnly: each pass reads each account of
// Owner with its own token, and the refused one is not read on the next.
func TestPassReadsEveryAccountOfTheOwnerOnly(t *testing.T) {
	src, err := ccpool.NewStatic(map[string]string{"alpha": "tok-a", "beta": "tok-b"})
	if err != nil {
		t.Fatal(err)
	}
	fx := &scripted{queue: []Result{good(10), {Status: 401}}}
	st := &memStore{}
	now := t0
	p := &Poller{Source: src, Store: st, Fetch: fx, Owner: 1, Interval: iv, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return now }}
	if err := p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fx.toks, ",") != "tok-a,tok-b" {
		t.Fatalf("tokens used: %v, want each account's own, by name", fx.toks)
	}
	rows, _ := st.Latest(context.Background(), 1)
	if rows["alpha"].ErrorKind != "" || rows["beta"].ErrorKind != KindUnauthorized || len(st.rows) != 1 {
		t.Fatalf("rows: %+v (owners %d)", rows, len(st.rows))
	}
	now = now.Add(iv)
	if err := p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fx.toks, ",") != "tok-a,tok-b,tok-a" {
		t.Fatalf("second pass tokens: %v, want only alpha again", fx.toks)
	}
}

// TestHTTPFetcherSendsTheOAuthHeadersAndKeepsTheTokenOut: the request carries
// the bearer and the OAuth beta header; a 429's Retry-After is read; and no
// error the fetcher returns carries the token.
func TestHTTPFetcherSendsTheOAuthHeadersAndKeepsTheTokenOut(t *testing.T) {
	const tok = "sk-ant-oat01-test-token-value"
	var gotAuth, gotBeta, gotPath string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta, gotPath = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta"), r.URL.Path
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "120")
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"five_hour": {"utilization": 7, "resets_at": "2026-11-10T20:00:00Z"}}`))
		} else {
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"x","message":"` + tok + `"}}`))
		}
	}))
	defer srv.Close()
	fx := HTTPFetcher{URL: srv.URL + "/api/oauth/usage"}
	res := fx.Fetch(context.Background(), tok)
	if res.Status != 200 || res.Err != nil || *res.Reading.Windows[FiveHour].Utilization != 7 {
		t.Fatalf("200: %+v", res)
	}
	if gotAuth != "Bearer "+tok || gotBeta != BetaHeader || gotPath != "/api/oauth/usage" {
		t.Fatalf("request headers: auth=%q beta=%q path=%q", gotAuth, gotBeta, gotPath)
	}
	status = http.StatusTooManyRequests
	if res := fx.Fetch(context.Background(), tok); res.Status != 429 || res.RetryAfter != 2*time.Minute {
		t.Fatalf("429: %+v", res)
	}
	srv.Close()
	res = fx.Fetch(context.Background(), tok)
	if res.Err == nil || strings.Contains(res.Err.Error(), tok) || strings.Contains(res.Err.Error(), "oauth/usage") {
		t.Fatalf("transport error %v: want one that names neither the token nor the URL", res.Err)
	}
	r, _ := Step(context.Background(), Row{}, "alpha", tok, t0, iv, fx)
	if strings.Contains(r.Error, tok) || r.TokenFP == tok || len(r.TokenFP) != 12 {
		t.Fatalf("the row carries the token: %+v", r)
	}
}

// TestKindsMatchTheMigration: the error kinds the poller writes are exactly
// the ones migration 0106's CHECK admits.
func TestKindsMatchTheMigration(t *testing.T) {
	b, err := readMigration()
	if err != nil {
		t.Fatal(err)
	}
	var quoted []string
	for _, k := range Kinds {
		quoted = append(quoted, "'"+k+"'")
	}
	want := "CHECK (error_kind IN (" + strings.Join(quoted, ", ") + "))"
	if !strings.Contains(b, want) {
		t.Fatalf("migration 0106 does not spell %s", want)
	}
}

func readMigration() (string, error) {
	b, err := os.ReadFile(filepath.Join("..", "db", "migrations", "0106_cc_account_usage.sql"))
	return string(b), err
}
