package ccusage

import (
	"context"
	"encoding/json"
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

// scripted is a Fetcher answering queues of results, counting each call.
type scripted struct {
	mu            sync.Mutex
	usage, probe  []Result
	usageN, probN int
	toks          []string
}

func (s *scripted) next(q *[]Result, n *int, tok string, dflt Result) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	*n++
	s.toks = append(s.toks, tok)
	if len(*q) == 0 {
		return dflt
	}
	r := (*q)[0]
	*q = (*q)[1:]
	return r
}

func (s *scripted) Usage(_ context.Context, tok string) Result {
	return s.next(&s.usage, &s.usageN, tok, Result{Status: http.StatusForbidden})
}

func (s *scripted) Probe(_ context.Context, tok string) Result {
	return s.next(&s.probe, &s.probN, tok, headers(40))
}

func f(v float64) *float64 { return &v }

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

const iv = 15 * time.Minute

func cfg() StepConfig { return StepConfig{Interval: iv} }

func usageOK(u float64) Result {
	return Result{Status: http.StatusOK, HasReading: true, Reading: Reading{Windows: map[string]Window{FiveHour: {Utilization: f(u)}}}}
}

func headers(u float64) Result {
	return Result{Status: http.StatusOK, HasReading: true, Reading: Reading{Status: "allowed", RepresentativeClaim: "seven_day",
		Windows: map[string]Window{SevenDay: {Utilization: f(u), Status: "allowed"}}}}
}

// TestASetupTokenFallsBackFromTheUsageEndpointToTheProbe: the usage
// endpoint's 403 (a token without the profile scope) is answered by the probe
// in the same read, the endpoint is retired for that token, and the next read
// probes without calling it. A replaced token gets one discovery call again.
func TestASetupTokenFallsBackFromTheUsageEndpointToTheProbe(t *testing.T) {
	for _, refusal := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		fx := &scripted{usage: []Result{{Status: refusal}}}
		r, ok := Step(context.Background(), Row{}, "alpha", "tok-a", t0, cfg(), fx)
		if !ok || fx.usageN != 1 || fx.probN != 1 || r.UsageAPI != UsageAPIRefused || r.Source != SourceRateLimitHeaders ||
			r.ErrorKind != "" || *r.Windows[SevenDay].Utilization != 40 || r.Status != "allowed" || r.RepresentativeClaim != "seven_day" {
			t.Fatalf("%d: row %+v ok=%v usage=%d probe=%d", refusal, r, ok, fx.usageN, fx.probN)
		}
		r, _ = Step(context.Background(), r, "alpha", "tok-a", t0.Add(iv), cfg(), fx)
		if fx.usageN != 1 || fx.probN != 2 || !r.ReadAt.Equal(t0.Add(iv)) {
			t.Fatalf("%d: the second read called the usage endpoint again (usage=%d probe=%d)", refusal, fx.usageN, fx.probN)
		}
		r, _ = Step(context.Background(), r, "alpha", "tok-b", t0.Add(2*iv), cfg(), fx)
		if fx.usageN != 2 || r.UsageAPI != UsageAPIRefused {
			t.Fatalf("%d: a replaced token was not given its discovery call (usage=%d, %+v)", refusal, fx.usageN, r)
		}
	}
}

// TestAFullScopeTokenKeepsTheUsageEndpoint: a 2xx keeps the endpoint for the
// token, and no probe (which spends quota) is made while it answers.
func TestAFullScopeTokenKeepsTheUsageEndpoint(t *testing.T) {
	fx := &scripted{usage: []Result{usageOK(12), usageOK(13), {Status: http.StatusTooManyRequests, RetryAfter: time.Hour}, usageOK(14)}}
	r, _ := Step(context.Background(), Row{}, "alpha", "tok", t0, cfg(), fx)
	r, _ = Step(context.Background(), r, "alpha", "tok", t0.Add(iv), cfg(), fx)
	if fx.usageN != 2 || fx.probN != 0 || r.UsageAPI != UsageAPIOK || r.Source != SourceUsageAPI || *r.Windows[FiveHour].Utilization != 13 {
		t.Fatalf("row %+v usage=%d probe=%d", r, fx.usageN, fx.probN)
	}
	// A 429 from the endpoint rests it for its Retry-After and reads by probe.
	at := t0.Add(2 * iv)
	r, _ = Step(context.Background(), r, "alpha", "tok", at, cfg(), fx)
	if fx.probN != 1 || r.UsageAPI != UsageAPIOK || !r.UsageAPINextAt.Equal(at.Add(time.Hour)) || r.Source != SourceRateLimitHeaders {
		t.Fatalf("after the endpoint's 429: %+v probe=%d", r, fx.probN)
	}
	r, _ = Step(context.Background(), r, "alpha", "tok", at.Add(iv), cfg(), fx)
	if fx.usageN != 3 || fx.probN != 2 {
		t.Fatalf("called the endpoint inside its rest (usage=%d probe=%d)", fx.usageN, fx.probN)
	}
	r, _ = Step(context.Background(), r, "alpha", "tok", at.Add(time.Hour), cfg(), fx)
	if fx.usageN != 4 || r.Source != SourceUsageAPI {
		t.Fatalf("the endpoint was not tried again after its rest (usage=%d, %+v)", fx.usageN, r)
	}
}

// TestTheProbeHonoursRetryAfter: a 429 waits at least its Retry-After (here
// longer than the doubled interval), nothing is called before it, and the
// 429's own headers are kept as the reading.
func TestTheProbeHonoursRetryAfter(t *testing.T) {
	limited := headers(97)
	limited.Status, limited.RetryAfter = http.StatusTooManyRequests, 3581*time.Second
	fx := &scripted{probe: []Result{limited, {Status: http.StatusTooManyRequests}}}
	r, _ := Step(context.Background(), Row{UsageAPI: UsageAPIRefused, TokenFP: ccpool.Fingerprint("tok")}, "alpha", "tok", t0, cfg(), fx)
	if r.ErrorKind != KindRateLimited || r.RateLimitStreak != 1 || !r.NextAt.Equal(t0.Add(3581*time.Second)) || *r.Windows[SevenDay].Utilization != 97 {
		t.Fatalf("first 429: %+v", r)
	}
	if _, ok := Step(context.Background(), r, "alpha", "tok", t0.Add(3580*time.Second), cfg(), fx); ok || fx.probN != 1 {
		t.Fatalf("probed inside Retry-After (probes %d)", fx.probN)
	}
	at := t0.Add(3581 * time.Second)
	r, _ = Step(context.Background(), r, "alpha", "tok", at, cfg(), fx)
	if r.RateLimitStreak != 2 || !r.NextAt.Equal(at.Add(Backoff(iv, 2, 0))) || *r.Windows[SevenDay].Utilization != 97 {
		t.Fatalf("second 429 (no headers): %+v", r)
	}
}

// TestARejectedAccountIsNotProbedUntilItsReset: an overall status `rejected`
// (on a 200, or on a 429 whose reset is days away) holds the account until
// the reset.
func TestARejectedAccountIsNotProbedUntilItsReset(t *testing.T) {
	reset := t0.Add(72 * time.Hour)
	rej := headers(100)
	rej.Reading.Status, rej.Reading.ResetsAt = "rejected", &reset
	rej429 := rej
	rej429.Status, rej429.RetryAfter = http.StatusTooManyRequests, time.Minute
	for _, res := range []Result{rej, rej429} {
		fx := &scripted{probe: []Result{res}}
		r, _ := Step(context.Background(), Row{UsageAPI: UsageAPIRefused, TokenFP: ccpool.Fingerprint("tok")}, "alpha", "tok", t0, cfg(), fx)
		if !r.NextAt.Equal(reset) {
			t.Fatalf("status %d rejected: NextAt %v, want the reset %v", res.Status, r.NextAt, reset)
		}
		if _, ok := Step(context.Background(), r, "alpha", "tok", reset.Add(-time.Second), cfg(), fx); ok || fx.probN != 1 {
			t.Fatalf("status %d: probed before the reset", res.Status)
		}
	}
}

// TestACoolingAccountIsNotProbed: the pool's rate-limit mark holds the probe
// until its cooldown ends (the usage endpoint, which spends no quota, may
// still be read); after it, the account is probed again.
func TestACoolingAccountIsNotProbed(t *testing.T) {
	until := t0.Add(2 * time.Hour)
	fx := &scripted{}
	prev := Row{UsageAPI: UsageAPIRefused, TokenFP: ccpool.Fingerprint("tok")}
	r, ok := Step(context.Background(), prev, "alpha", "tok", t0, StepConfig{Interval: iv, CoolingUntil: until}, fx)
	if !ok || fx.probN != 0 || r.ErrorKind != KindCooling || !r.NextAt.Equal(until) {
		t.Fatalf("cooling: %+v ok=%v probes=%d", r, ok, fx.probN)
	}
	if _, ok := Step(context.Background(), r, "alpha", "tok", t0.Add(time.Hour), StepConfig{Interval: iv, CoolingUntil: until}, fx); ok || fx.probN != 0 {
		t.Fatalf("probed while cooling (probes %d)", fx.probN)
	}
	r, _ = Step(context.Background(), r, "alpha", "tok", until, StepConfig{Interval: iv}, fx)
	if fx.probN != 1 || r.ErrorKind != "" {
		t.Fatalf("after the cooldown: %+v probes=%d", r, fx.probN)
	}
}

// TestAProbeRefusalIsNotRetried: a 401/403 from the PROBE marks the token,
// which is not read again until it is replaced.
func TestAProbeRefusalIsNotRetried(t *testing.T) {
	for status, kind := range map[int]string{http.StatusUnauthorized: KindUnauthorized, http.StatusForbidden: KindForbidden} {
		fx := &scripted{probe: []Result{{Status: status}}}
		r, _ := Step(context.Background(), Row{}, "alpha", "tok-a", t0, cfg(), fx)
		if r.ErrorKind != kind || fx.usageN != 1 || fx.probN != 1 {
			t.Fatalf("%d: %+v", status, r)
		}
		for i := 1; i <= 4; i++ {
			if _, ok := Step(context.Background(), r, "alpha", "tok-a", t0.Add(time.Duration(i)*time.Hour), cfg(), fx); ok {
				t.Fatalf("%d: read again on a refused token", status)
			}
		}
		if r2, _ := Step(context.Background(), r, "alpha", "tok-b", t0.Add(time.Hour), cfg(), fx); r2.ErrorKind != "" || fx.probN != 2 {
			t.Fatalf("%d: a replaced token was not read: %+v", status, r2)
		}
	}
}

func TestBackoffBounds(t *testing.T) {
	for _, c := range []struct {
		streak int
		ra     time.Duration
		want   time.Duration
	}{{1, 0, 30 * time.Minute}, {2, 0, time.Hour}, {3, 0, 2 * time.Hour}, {1, 50 * time.Minute, 50 * time.Minute},
		{20, 0, MaxBackoff}, {1, 48 * time.Hour, MaxBackoff}, {0, 0, 30 * time.Minute}} {
		if got := Backoff(iv, c.streak, c.ra); got != c.want {
			t.Errorf("Backoff(15m, %d, %s) = %s, want %s", c.streak, c.ra, got, c.want)
		}
	}
}

func TestStepWithNoTokenMakesNoCall(t *testing.T) {
	fx := &scripted{}
	r, ok := Step(context.Background(), Row{}, "alpha", "  ", t0, cfg(), fx)
	if !ok || r.ErrorKind != KindNoToken || fx.usageN+fx.probN != 0 {
		t.Fatalf("no token: %+v ok=%v", r, ok)
	}
	if _, ok := Step(context.Background(), r, "alpha", "", t0.Add(time.Hour), cfg(), fx); ok || fx.usageN+fx.probN != 0 {
		t.Fatal("a second pass with no token wrote again or called")
	}
}

// TestAFailedReadKeepsTheLastGoodReading: a probe 5xx or transport error keeps
// the last good windows and ReadAt, records the error, and does not back off.
func TestAFailedReadKeepsTheLastGoodReading(t *testing.T) {
	fx := &scripted{probe: []Result{headers(42), {Status: 502}, {Err: errors.New("ccusage: request failed (timeout)")}, {Status: 200}}}
	prev := Row{UsageAPI: UsageAPIRefused, TokenFP: ccpool.Fingerprint("tok")}
	r, _ := Step(context.Background(), prev, "alpha", "tok", t0, cfg(), fx)
	r, _ = Step(context.Background(), r, "alpha", "tok", t0.Add(iv), cfg(), fx)
	if r.ErrorKind != KindError || !strings.Contains(r.Error, "502") || !r.ReadAt.Equal(t0) || *r.Windows[SevenDay].Utilization != 42 || !r.NextAt.IsZero() {
		t.Fatalf("after a 502: %+v", r)
	}
	r, _ = Step(context.Background(), r, "alpha", "tok", t0.Add(2*iv), cfg(), fx)
	if r.ErrorKind != KindError || !strings.Contains(r.Error, "timeout") || *r.Windows[SevenDay].Utilization != 42 {
		t.Fatalf("after a transport error: %+v", r)
	}
	r, _ = Step(context.Background(), r, "alpha", "tok", t0.Add(3*iv), cfg(), fx)
	if r.ErrorKind != KindError || !strings.Contains(r.Error, "no rate-limit headers") {
		t.Fatalf("a 200 with no headers: %+v", r)
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

// TestPassReadsEveryAccountAndAsksThePoolAboutCooling: each account is read
// with its own token, and the pool's cooldown is asked per account (a read
// error holds the probe).
func TestPassReadsEveryAccountAndAsksThePoolAboutCooling(t *testing.T) {
	src, err := ccpool.NewStatic(map[string]string{"alpha": "tok-a", "beta": "tok-b", "gamma": "tok-c"})
	if err != nil {
		t.Fatal(err)
	}
	fx := &scripted{}
	st := &memStore{}
	cooling := func(_ context.Context, owner int64, account string) (time.Time, error) {
		switch account {
		case "beta":
			return t0.Add(time.Hour), nil
		case "gamma":
			return time.Time{}, errors.New("store down")
		}
		return time.Time{}, nil
	}
	p := &Poller{Source: src, Store: st, Fetch: fx, Cooling: cooling, Owner: 1, Interval: iv, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return t0 }}
	if err := p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.Latest(context.Background(), 1)
	if fx.probN != 1 || rows["alpha"].ErrorKind != "" || rows["beta"].ErrorKind != KindCooling || rows["gamma"].ErrorKind != KindCooling {
		t.Fatalf("probes %d, rows %+v", fx.probN, rows)
	}
	if strings.Join(fx.toks, ",") != "tok-a,tok-a,tok-b,tok-c" {
		t.Fatalf("tokens used: %v, want each account's own (alpha: discovery then probe)", fx.toks)
	}
}

// TestHTTPFetcherSendsBothCallsAndKeepsTheTokenOut: the usage call and the
// probe carry the bearer and the OAuth beta header, the probe is a one-token
// Messages call with its anthropic-version, its headers are read on a 200 and
// a 429 (with Retry-After), and no error carries the token.
func TestHTTPFetcherSendsBothCallsAndKeepsTheTokenOut(t *testing.T) {
	const tok = "sk-ant-oat01-test-token-value"
	type seen struct{ method, path, auth, beta, version, model string }
	var got []seen
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method == http.MethodPost && body.MaxTokens != 1 {
			t.Errorf("the probe asked for %d tokens, want 1", body.MaxTokens)
		}
		got = append(got, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("anthropic-beta"), r.Header.Get("anthropic-version"), body.Model})
		if r.URL.Path == "/v1/messages" {
			w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
			w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.25")
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "120")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"` + tok + `"}]}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"` + tok + `"}}`))
	}))
	defer srv.Close()
	fx := HTTPFetcher{URL: srv.URL + "/api/oauth/usage", ProbeURL: srv.URL + "/v1/messages"}
	if res := fx.Usage(context.Background(), tok); res.Status != 403 || res.HasReading || res.Err != nil {
		t.Fatalf("usage 403: %+v", res)
	}
	res := fx.Probe(context.Background(), tok)
	if res.Status != 200 || !res.HasReading || *res.Reading.Windows[SevenDay].Utilization != 25 || res.Reading.Status != "allowed" {
		t.Fatalf("probe 200: %+v", res)
	}
	want := []seen{{"GET", "/api/oauth/usage", "Bearer " + tok, BetaHeader, "", ""}, {"POST", "/v1/messages", "Bearer " + tok, BetaHeader, anthropicVersion, ProbeModel}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("requests = %+v\nwant %+v", got, want)
	}
	status = http.StatusTooManyRequests
	if res := fx.Probe(context.Background(), tok); res.Status != 429 || res.RetryAfter != 2*time.Minute || !res.HasReading {
		t.Fatalf("probe 429: %+v", res)
	}
	srv.Close()
	for _, res := range []Result{fx.Usage(context.Background(), tok), fx.Probe(context.Background(), tok)} {
		if res.Err == nil || strings.Contains(res.Err.Error(), tok) || strings.Contains(res.Err.Error(), "/v1/") {
			t.Fatalf("transport error %v: want one that names neither the token nor the URL", res.Err)
		}
	}
	r, _ := Step(context.Background(), Row{}, "alpha", tok, t0, cfg(), fx)
	if strings.Contains(r.Error, tok) || r.TokenFP == tok || len(r.TokenFP) != 12 {
		t.Fatalf("the row carries the token: %+v", r)
	}
}

// TestKindsMatchTheMigration: the error kinds the poller writes are exactly
// the ones migration 0106's CHECK admits.
func TestKindsMatchTheMigration(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "db", "migrations", "0106_cc_account_usage.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var quoted []string
	for _, k := range Kinds {
		quoted = append(quoted, "'"+k+"'")
	}
	if want := "CHECK (error_kind IN (" + strings.Join(quoted, ", ") + "))"; !strings.Contains(string(b), want) {
		t.Fatalf("migration 0106 does not spell %s", want)
	}
	for _, want := range []string{"CHECK (source IN ('', '" + SourceUsageAPI + "', '" + SourceRateLimitHeaders + "'))",
		"CHECK (usage_api IN ('', '" + UsageAPIOK + "', '" + UsageAPIRefused + "'))"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("migration 0106 does not spell %s", want)
		}
	}
}
