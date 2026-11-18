package officers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// THE POOLS PUSH (pools.go): PUT /officers/{id}/pools with the officer's own
// token, the officer's whole declared set, sessions_today from the counter
// that enforces the cap. Fixture values are pairwise distinct: judgement
// max_instances 2, cap 30, 4 sessions used; routine max_instances 3; untiered
// max_instances 5.
// ---------------------------------------------------------------------------

const (
	poolsOfficer = "officer-a:area"
	poolsTok     = "officer-a-token-SENTINEL"
)

func tieredOfficer(hq *hqtest.Server) officers.Officer {
	return officers.Officer{ID: poolsOfficer, HQURL: hq.BaseURL(), Owner: 1, Tiers: &officers.Tiers{
		Routine:   officers.TierPool{Model: "haiku", MaxInstances: 3},
		Judgement: officers.TierPool{Model: "opus", MaxInstances: 2, MaxSessionsPerDay: 30}}}.WithToken(poolsTok)
}

func untieredOfficer(hq *hqtest.Server) officers.Officer {
	return officers.Officer{ID: poolsOfficer, HQURL: hq.BaseURL(), Owner: 1, Model: "sonnet", MaxInstances: 5}.WithToken(poolsTok)
}

type poolsSleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *poolsSleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return nil
}

type poolsRig struct {
	hq    *hqtest.Server
	store *daySessions
	logs  *bytes.Buffer
	slp   *poolsSleeps
	clock time.Time
}

func newPoolsRig(t *testing.T) *poolsRig {
	t.Helper()
	hq := hqtest.New(map[string]string{poolsTok: poolsOfficer})
	t.Cleanup(hq.Close)
	st := &daySessions{days: map[string][]string{}}
	// Day 2000-01-03: four items counted, as the cap's ledger holds them.
	st.set(poolsOfficer, "2000-01-03", "101", "102", "103", "104")
	return &poolsRig{hq: hq, store: st, logs: &bytes.Buffer{}, slp: &poolsSleeps{}, clock: time.Date(2000, 1, 3, 17, 25, 0, 0, time.UTC)}
}

func (r *poolsRig) reporter(t *testing.T, o officers.Officer) *officers.PoolsReporter {
	t.Helper()
	p, err := officers.NewPoolsReporter(officers.PoolsReporterConfig{Officers: []officers.Officer{o},
		Poster: officers.NewBackendPoolsPoster(officers.NewBackends(nil)), Ledger: officers.NewJudgementLedger(r.store, nil, nil),
		Logger: log.New(r.logs, "", 0), Now: func() time.Time { return r.clock }, Sleep: r.slp.sleep})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	tieredPools = `{"pools":[` +
		`{"pool":"judgement","model":"opus","max_instances":2,"max_sessions_per_day":30,"sessions_today":4,"sessions_day_resets_at":"2000-01-04T00:00:00Z"},` +
		`{"pool":"routine","model":"haiku","max_instances":3,"max_sessions_per_day":null,"sessions_today":0,"sessions_day_resets_at":"2000-01-04T00:00:00Z"}],` +
		`"reported_at":"2000-01-03T17:24:58Z"}`
	untieredPools = `{"pools":[` +
		`{"pool":"","model":"sonnet","max_instances":5,"max_sessions_per_day":null,"sessions_today":0,"sessions_day_resets_at":"2000-01-04T00:00:00Z"}],` +
		`"reported_at":"2000-01-03T17:24:58Z"}`
)

// TestThePoolsPushIsTheOfficersDeclaredSet: a tiered officer sends both pools
// (judgement with its cap and the day's count, routine with neither) and an
// untiered one its single "" pool — exactly these bytes, with its own token.
func TestThePoolsPushIsTheOfficersDeclaredSet(t *testing.T) {
	r := newPoolsRig(t)
	r.reporter(t, tieredOfficer(r.hq)).Tick(context.Background())
	r.reporter(t, untieredOfficer(r.hq)).Tick(context.Background())
	puts := r.hq.PoolsPuts()
	if len(puts) != 2 || string(puts[0].Raw) != tieredPools || string(puts[1].Raw) != untieredPools || puts[0].Officer != poolsOfficer {
		var got []string
		for _, p := range puts {
			got = append(got, p.Officer+" "+string(p.Raw))
		}
		t.Fatalf("PUTs:\n%s\nwant:\n%s\n%s", strings.Join(got, "\n"), tieredPools, untieredPools)
	}
	for _, q := range r.hq.Requests() {
		if q.Auth != "Bearer "+poolsTok || q.Path != "/api/v1/officers/"+poolsOfficer+"/pools" {
			t.Fatalf("request %s auth %q", q.Path, q.Auth)
		}
	}
}

// TestAPoolRemovedFromTheConfigLeavesHQsSet: the officer pushed tiered, then
// reconfigured untiered (its tiers removed): the second body names only "",
// and HQ's set is exactly that.
func TestAPoolRemovedFromTheConfigLeavesHQsSet(t *testing.T) {
	r := newPoolsRig(t)
	r.reporter(t, tieredOfficer(r.hq)).Tick(context.Background())
	if got := strings.Join(r.hq.PoolSet(poolsOfficer), ","); got != "judgement,routine" {
		t.Fatalf("after the tiered push HQ holds %q", got)
	}
	r.reporter(t, untieredOfficer(r.hq)).Tick(context.Background())
	if got := r.hq.PoolSet(poolsOfficer); len(got) != 1 || got[0] != "" {
		t.Fatalf("after the untiered push HQ holds %q, want only the untiered pool", got)
	}
}

// TestSessionsTodayIsTheCapCounterAndResetsWithTheDay: 4 on 2000-01-03 (the
// ledger's count), and the same reporter at 00:05 the next day reads that
// day's ledger — 0 — with the reset moved to the following midnight.
func TestSessionsTodayIsTheCapCounterAndResetsWithTheDay(t *testing.T) {
	r := newPoolsRig(t)
	p := r.reporter(t, tieredOfficer(r.hq))
	p.Tick(context.Background())
	r.clock = time.Date(2000, 1, 4, 0, 5, 0, 0, time.UTC)
	p.Tick(context.Background())
	puts := r.hq.PoolsPuts()
	want := `{"pools":[` +
		`{"pool":"judgement","model":"opus","max_instances":2,"max_sessions_per_day":30,"sessions_today":0,"sessions_day_resets_at":"2000-01-05T00:00:00Z"},` +
		`{"pool":"routine","model":"haiku","max_instances":3,"max_sessions_per_day":null,"sessions_today":0,"sessions_day_resets_at":"2000-01-05T00:00:00Z"}],` +
		`"reported_at":"2000-01-04T00:04:58Z"}`
	if len(puts) != 2 || string(puts[0].Raw) != tieredPools || string(puts[1].Raw) != want {
		t.Fatalf("PUTs %d:\n%s", len(puts), puts)
	}
	// The cap's own reading agrees with what was pushed (one counter).
	r.store.set(poolsOfficer, "2000-01-04", "201", "202", "203")
	used, _, err := officers.NewJudgementLedger(r.store, nil, nil).Usage(context.Background(), tieredOfficer(r.hq).Pool(officers.TierJudgement), r.clock)
	p.Tick(context.Background())
	if err != nil || used != 3 || !strings.Contains(string(r.hq.PoolsPuts()[2].Raw), `"sessions_today":3,`) {
		t.Fatalf("ledger %d %v; pushed %s", used, err, r.hq.PoolsPuts()[2].Raw)
	}
}

// TestAPoolsRefusalIsLoggedOnceAndNotRetried: HQ answers 400 three ticks
// running — three PUTs, no backoff, one log line — then takes it, logged once.
func TestAPoolsRefusalIsLoggedOnceAndNotRetried(t *testing.T) {
	r := newPoolsRig(t)
	p := r.reporter(t, untieredOfficer(r.hq))
	r.hq.FailPools(400, 400, 400)
	for i := 0; i < 3; i++ {
		p.Tick(context.Background())
	}
	if n := len(r.hq.Requests()); n != 3 || len(r.slp.d) != 0 {
		t.Fatalf("%d PUTs, waits %v; want 3 and none", n, r.slp.d)
	}
	if n := strings.Count(r.logs.String(), "HQ refused the pools push (400)"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, r.logs)
	}
	p.Tick(context.Background())
	if len(r.hq.PoolsPuts()) != 1 || strings.Count(r.logs.String(), "pushed to HQ again") != 1 {
		t.Fatalf("recovery: %d accepted\n%s", len(r.hq.PoolsPuts()), r.logs)
	}
	if strings.Contains(r.logs.String(), poolsTok) {
		t.Fatal("a log line carries the token")
	}
}

// TestAPools5xxIsRetriedWithBackoff: 503 then 502, then taken — three PUTs
// after waits of 2 s and 5 s; HQ down throughout is three attempts, logged
// once over two ticks.
func TestAPools5xxIsRetriedWithBackoff(t *testing.T) {
	r := newPoolsRig(t)
	p := r.reporter(t, untieredOfficer(r.hq))
	r.hq.FailPools(503, 502)
	p.Tick(context.Background())
	if n := len(r.hq.Requests()); n != 3 || len(r.hq.PoolsPuts()) != 1 {
		t.Fatalf("%d PUTs, %d accepted; want 3 and 1", n, len(r.hq.PoolsPuts()))
	}
	if len(r.slp.d) != 2 || r.slp.d[0] != 2*time.Second || r.slp.d[1] != 5*time.Second {
		t.Fatalf("waits %v, want [2s 5s]", r.slp.d)
	}
	r.hq.FailPools(503, 503, 503, 503, 503, 503)
	p.Tick(context.Background())
	p.Tick(context.Background())
	if n := len(r.hq.Requests()); n != 9 {
		t.Fatalf("%d PUTs, want 3 + 3 + 3", n)
	}
	if n := strings.Count(r.logs.String(), "HQ did not take the pools push"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, r.logs)
	}
}

// TestRunPushesAtBoot: Run pushes before its first interval.
func TestRunPushesAtBoot(t *testing.T) {
	r := newPoolsRig(t)
	p := r.reporter(t, untieredOfficer(r.hq))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(r.hq.PoolsPuts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n := len(r.hq.PoolsPuts()); n != 1 {
		t.Fatalf("%d pushes at boot, want 1 (the interval is 10 minutes)", n)
	}
}

// TestAnUnreadableSessionCountSendsNothing: the ledger failing, no pools are
// pushed (a wrong sessions_today is worse than a late one), logged once over
// two ticks; once it reads again the push goes out.
func TestAnUnreadableSessionCountSendsNothing(t *testing.T) {
	r := newPoolsRig(t)
	p := r.reporter(t, tieredOfficer(r.hq))
	r.store.err = errors.New("ledger unavailable")
	p.Tick(context.Background())
	p.Tick(context.Background())
	if n := len(r.hq.Requests()); n != 0 {
		t.Fatalf("%d PUTs with the session count unreadable", n)
	}
	if n := strings.Count(r.logs.String(), "the session count could not be read"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, r.logs)
	}
	r.store.err = nil
	p.Tick(context.Background())
	if puts := r.hq.PoolsPuts(); len(puts) != 1 || string(puts[0].Raw) != tieredPools {
		t.Fatalf("after recovery: %v", puts)
	}
}

// TestPoolsBodiesMeetHQsShapeRules pins, structurally, the two rules HQ
// refuses a body over: pool "" is an untiered officer's ONLY pool and never
// sent with another, and every pool carries max_instances (0-100).
func TestPoolsBodiesMeetHQsShapeRules(t *testing.T) {
	r := newPoolsRig(t)
	r.reporter(t, tieredOfficer(r.hq)).Tick(context.Background())
	r.reporter(t, untieredOfficer(r.hq)).Tick(context.Background())
	puts := r.hq.PoolsPuts()
	if len(puts) != 2 {
		t.Fatalf("%d accepted pools PUTs, want 2", len(puts))
	}
	for k, want := range []string{"judgement,routine", ""} {
		var body struct {
			Pools []map[string]any `json:"pools"`
		}
		if err := json.Unmarshal(puts[k].Raw, &body); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range body.Pools {
			n, ok := p["max_instances"].(float64)
			if !ok || n < 0 || n > 100 {
				t.Fatalf("body %d: pool %v has max_instances %v", k, p["pool"], p["max_instances"])
			}
			names = append(names, p["pool"].(string))
		}
		if got := strings.Join(names, ","); got != want || (len(names) > 1 && strings.Contains(","+got+",", ",,")) {
			t.Fatalf("body %d pools %q, want %q", k, got, want)
		}
	}
}

// TestReportedAtIsTwoSecondsBackOnTheSecond: HQ refuses a reported_at in its
// future, so a small clock skew must not make it one: the push says now - 2 s,
// truncated to the second (17:25:00.9 -> 17:24:58).
func TestReportedAtIsTwoSecondsBackOnTheSecond(t *testing.T) {
	r := newPoolsRig(t)
	r.clock = time.Date(2000, 1, 3, 17, 25, 0, 900_000_000, time.UTC)
	r.reporter(t, untieredOfficer(r.hq)).Tick(context.Background())
	puts := r.hq.PoolsPuts()
	if len(puts) != 1 || !strings.HasSuffix(string(puts[0].Raw), `"reported_at":"2000-01-03T17:24:58Z"}`) {
		t.Fatalf("PUTs %s", puts)
	}
}

// TestAPoolModelHQWouldRefuseIsSentAsDefault: muster accepts a model with a
// context suffix ("opus[1m]") that HQ's officer model pattern
// (^[A-Za-z0-9._:/-]{1,128}$) refuses — and HQ refuses the WHOLE pools PUT
// over it. Such a pool is sent with model "" (HQ shows its default) and the
// push is accepted; the other pool keeps its model; it is logged once per
// officer over two ticks.
func TestAPoolModelHQWouldRefuseIsSentAsDefault(t *testing.T) {
	r := newPoolsRig(t)
	o := tieredOfficer(r.hq)
	o.Tiers.Judgement.Model = "opus[1m]"
	p := r.reporter(t, o)
	p.Tick(context.Background())
	p.Tick(context.Background())
	puts := r.hq.PoolsPuts()
	want := `{"pools":[` +
		`{"pool":"judgement","model":"","max_instances":2,"max_sessions_per_day":30,"sessions_today":4,"sessions_day_resets_at":"2000-01-04T00:00:00Z"},` +
		`{"pool":"routine","model":"haiku","max_instances":3,"max_sessions_per_day":null,"sessions_today":0,"sessions_day_resets_at":"2000-01-04T00:00:00Z"}],` +
		`"reported_at":"2000-01-03T17:24:58Z"}`
	if len(puts) != 2 || string(puts[0].Raw) != want {
		t.Fatalf("accepted %d PUTs:\n%s\nwant:\n%s", len(puts), puts, want)
	}
	if n := strings.Count(r.logs.String(), `"opus[1m]"`); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, r.logs)
	}
}

// TestTheFakeRefusesAPoolModelHQRefuses: the instrument check for the test
// above — the fake 400s a pools body whose model HQ would refuse.
func TestTheFakeRefusesAPoolModelHQRefuses(t *testing.T) {
	r := newPoolsRig(t)
	body := `{"pools":[{"pool":"","model":"opus[1m]","max_instances":1,"max_sessions_per_day":null,"sessions_today":0,` +
		`"sessions_day_resets_at":"2000-01-04T00:00:00Z"}],"reported_at":"2000-01-03T17:24:58Z"}`
	req, _ := http.NewRequest(http.MethodPut, r.hq.BaseURL()+"/officers/"+poolsOfficer+"/pools", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+poolsTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || len(r.hq.PoolsPuts()) != 0 {
		t.Fatalf("status %d, accepted %d", resp.StatusCode, len(r.hq.PoolsPuts()))
	}
}
