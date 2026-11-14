package ccusage

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ccpool"
)

// ---------------------------------------------------------------------------
// THE LIVE READING CLEARS A COOLING ACCOUNT'S RATE-LIMIT MARK. The pool's mark
// says "cooling until the CLI's stated reset"; the poller keeps reading the
// account, and when the account's own live answer shows it usable the mark is
// cleared — only while it is still the mark read before the reading.
//
// These run the REAL ccpool.Pool (its Cooldown and ClearRateLimit, wired the
// way cmd/muster-server wires them) over an in-memory ccpool.Store.
// ---------------------------------------------------------------------------

const (
	liveTokA = "sk-ant-oat01-FAKE-live-alpha"
	liveTokB = "sk-ant-oat01-FAKE-live-beta"
)

// liveMarks is ccpool.Store in memory, with PGStore's compare-and-clear.
type liveMarks struct {
	mu sync.Mutex
	m  map[string]ccpool.Mark
}

func (s *liveMarks) Marks(context.Context, int64) (map[string]ccpool.Mark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]ccpool.Mark{}
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}
func (s *liveMarks) LiveCounts(context.Context, int64) (map[string]int, error) { return nil, nil }
func (s *liveMarks) MarkRateLimited(_ context.Context, _ int64, account, detail string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m[account]
	m.Account, m.RateLimitedAt, m.RateLimitedDetail = account, at, detail
	s.m[account] = m
	return nil
}
func (s *liveMarks) MarkAuthFailed(_ context.Context, _ int64, account, fp, detail string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m[account]
	m.Account, m.AuthFailedAt, m.AuthFailedDetail, m.AuthFailedToken = account, at, detail, fp
	s.m[account] = m
	return nil
}
func (s *liveMarks) ClearRateLimited(_ context.Context, _ int64, account string, markedAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[account]
	if !ok || !m.RateLimitedAt.Equal(markedAt) {
		return false, nil
	}
	m.RateLimitedAt, m.RateLimitedDetail = time.Time{}, ""
	s.m[account] = m
	return true, nil
}

// hooked runs during to inside the probe call (a turn writing a mark while the
// reading is in flight), then answers like inner.
type hooked struct {
	inner  Fetcher
	during func()
}

func (h hooked) Usage(ctx context.Context, tok string) Result { return h.inner.Usage(ctx, tok) }
func (h hooked) Probe(ctx context.Context, tok string) Result {
	if h.during != nil {
		h.during()
	}
	return h.inner.Probe(ctx, tok)
}

type liveRig struct {
	marks  *liveMarks
	pool   *ccpool.Pool
	poller *Poller
	logs   *bytes.Buffer
	// markedAt is alpha's rate-limit mark's time: within the pool's cooldown of
	// the pool's (real) clock, so alpha is cooling when the pass begins.
	markedAt time.Time
}

// liveWeekly is alpha's mark's detail: the weekly limit, with no stated reset
// (so the 6h cooldown applies).
const liveWeekly = "You've hit your weekly limit"

// newLiveRig: alpha is rate-limited (the weekly limit) with no stated reset
// (the 6h cooldown applies), beta is not marked at all.
func newLiveRig(t *testing.T, fetch Fetcher) *liveRig {
	t.Helper()
	toks := map[string]string{"alpha": liveTokA, "beta": liveTokB}
	src, err := ccpool.NewStatic(toks)
	if err != nil {
		t.Fatal(err)
	}
	markedAt := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	marks := &liveMarks{m: map[string]ccpool.Mark{"alpha": {Account: "alpha", RateLimitedAt: markedAt,
		RateLimitedDetail: liveWeekly}}}
	pool, err := ccpool.NewWithLimits(src, marks, ccpool.Limits{Cooldown: 6 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if cd, err := pool.Cooldown(context.Background(), 1, "alpha"); err != nil || cd.Until.IsZero() || !cd.MarkedAt.Equal(markedAt) {
		t.Fatalf("precondition: alpha's cooldown = %+v, %v; want it cooling since %s", cd, err, markedAt)
	}
	logs := &bytes.Buffer{}
	p := &Poller{Source: src, Store: &memStore{}, Fetch: fetch, Cooling: pool.Cooldown, ClearCooling: pool.ClearRateLimit,
		Owner: 1, Interval: iv, Logger: log.New(logs, "", 0)}
	return &liveRig{marks: marks, pool: pool, poller: p, logs: logs, markedAt: markedAt}
}

func (r *liveRig) alpha() ccpool.Mark {
	m, _ := r.marks.Marks(context.Background(), 1)
	return m["alpha"]
}

func (r *liveRig) clearLines() []string {
	var out []string
	for _, l := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(l, "cleared its rate-limit mark") {
			out = append(out, l)
		}
	}
	return out
}

// TestALiveReadingThatSaysUsableClearsTheMark (a): alpha is cooling; its live
// reading — the probe's status "allowed" (a token the usage endpoint refuses),
// or the usage endpoint's windows all under 100% — shows it usable, so the
// pass clears its mark: the pool no longer reports it cooling and a failover
// off beta may now land on it. One log line names the account and the reason.
func TestALiveReadingThatSaysUsableClearsTheMark(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fx     *scripted
		reason string
	}{
		{"the probe says allowed", &scripted{}, "the live probe's rate-limit status is allowed and its 7d window is under its limit"},
		{"the probe says allowed_warning", &scripted{probe: []Result{{Status: http.StatusOK, HasReading: true,
			Reading: Reading{Status: "allowed_warning", Windows: map[string]Window{SevenDay: {Utilization: f(93), Status: "allowed_warning"}}}}}},
			"the live probe's rate-limit status is allowed_warning and its 7d window is under its limit"},
		{"the usage endpoint says under 100%", &scripted{usage: []Result{usageOK(40)}},
			"the usage endpoint reports every window below 100% (or past its reset)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newLiveRig(t, tc.fx)
			if err := rig.poller.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if m := rig.alpha(); !m.RateLimitedAt.IsZero() || m.RateLimitedDetail != "" {
				t.Fatalf("alpha's mark after a usable live reading = %+v, want cleared", m)
			}
			states, err := rig.pool.States(context.Background(), 1)
			if err != nil || !states["alpha"].CoolingUntil.IsZero() {
				t.Fatalf("alpha's state = %+v, %v; want not cooling", states["alpha"], err)
			}
			if to, ok, err := rig.pool.FailoverTarget(context.Background(), 1, "beta"); err != nil || !ok || to != "alpha" {
				t.Fatalf("failover off beta = %q, %v, %v; want alpha (selectable again)", to, ok, err)
			}
			lines := rig.clearLines()
			want := "ccusage: account alpha: cleared its rate-limit mark of " + rig.markedAt.UTC().Format(time.RFC3339)
			if len(lines) != 1 || !strings.HasPrefix(lines[0], want) || !strings.HasSuffix(lines[0], ": "+tc.reason) {
				t.Fatalf("log lines %q, want one starting %q and ending %q", lines, want, tc.reason)
			}
		})
	}
}

// TestALiveReadingThatSaysLimitedKeepsTheMark (b): the live reading says alpha
// is still limited — the probe answers 429 rejected, or 200 with status
// "rejected", or the usage endpoint reports a window at 100% that has not
// reset — so its mark stands exactly as it was, and nothing is logged as
// cleared.
func TestALiveReadingThatSaysLimitedKeepsTheMark(t *testing.T) {
	later := time.Now().Add(3 * time.Hour)
	rejected := Reading{Status: "rejected", ResetsAt: &later, Windows: map[string]Window{FiveHour: {Utilization: f(100), Status: "rejected"}}}
	for _, tc := range []struct {
		name string
		fx   *scripted
	}{
		{"the probe answers 429 rejected", &scripted{probe: []Result{{Status: http.StatusTooManyRequests, HasReading: true, Reading: rejected}}}},
		{"the probe answers 200 rejected", &scripted{probe: []Result{{Status: http.StatusOK, HasReading: true, Reading: rejected}}}},
		{"the probe's answer carries no status", &scripted{probe: []Result{{Status: http.StatusOK, HasReading: true,
			Reading: Reading{Windows: map[string]Window{SevenDay: {Utilization: f(10)}}}}}}},
		{"the probe fails", &scripted{probe: []Result{{Status: http.StatusInternalServerError}}}},
		{"the usage endpoint says 100%", &scripted{usage: []Result{{Status: http.StatusOK, HasReading: true,
			Reading: Reading{Windows: map[string]Window{FiveHour: {Utilization: f(100), ResetsAt: &later}, SevenDay: {Utilization: f(20)}}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newLiveRig(t, tc.fx)
			if err := rig.poller.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if m := rig.alpha(); !m.RateLimitedAt.Equal(rig.markedAt) || m.RateLimitedDetail != liveWeekly {
				t.Fatalf("alpha's mark = %+v, want it kept (marked %s)", m, rig.markedAt)
			}
			if lines := rig.clearLines(); len(lines) != 0 {
				t.Fatalf("logged a clear: %q", lines)
			}
		})
	}
}

// TestANewerMarkWrittenDuringTheProbeIsNotCleared (c): while alpha's probe is
// in flight, a turn on alpha hits the limit again and writes a NEWER mark. The
// probe then answers "allowed" — but that reading was decided against the
// mark read before it, and the newer one stands.
func TestANewerMarkWrittenDuringTheProbeIsNotCleared(t *testing.T) {
	var rig *liveRig
	var newer time.Time
	fx := hooked{inner: &scripted{}, during: func() {
		if newer.IsZero() { // alpha's probe is the first; beta's must not move it
			newer = rig.markedAt.Add(30 * time.Second)
			if err := rig.marks.MarkRateLimited(context.Background(), 1, "alpha", "You've hit your weekly limit again", newer); err != nil {
				t.Fatal(err)
			}
		}
	}}
	rig = newLiveRig(t, fx)
	if err := rig.poller.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := rig.alpha(); !m.RateLimitedAt.Equal(newer) || m.RateLimitedDetail != "You've hit your weekly limit again" {
		t.Fatalf("alpha's mark = %+v, want the newer one (%s) kept", m, newer)
	}
	if lines := rig.clearLines(); len(lines) != 0 {
		t.Fatalf("logged a clear: %q", lines)
	}
	// The control: the next pass, with no mark written during it, reads the
	// newer mark first and clears it on the same "allowed" answer. (The rows
	// wait one interval between reads, so the store starts over.)
	rig.poller.Store = &memStore{}
	if err := rig.poller.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := rig.alpha(); !m.RateLimitedAt.IsZero() {
		t.Fatalf("control: alpha's mark = %+v, want cleared", m)
	}
}

// TestNoClearCoolingMeansNoClear: a poller not wired to clear (ClearCooling
// nil) still reads a cooling account and never touches its mark.
func TestNoClearCoolingMeansNoClear(t *testing.T) {
	fx := &scripted{}
	rig := newLiveRig(t, fx)
	rig.poller.ClearCooling = nil
	if err := rig.poller.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := rig.alpha(); !m.RateLimitedAt.Equal(rig.markedAt) || fx.probN != 2 {
		t.Fatalf("alpha's mark = %+v, probes %d; want it kept and both accounts probed", m, fx.probN)
	}
}

// TestLiveUsableCountsOnlyAReadingTakenNow: a kept older reading (ReadAt
// before now) or a failed attempt is not live evidence, whatever it says.
func TestLiveUsableCountsOnlyAReadingTakenNow(t *testing.T) {
	good := Row{Source: SourceRateLimitHeaders, Status: "allowed", ReadAt: t0, Windows: map[string]Window{SevenDay: {Utilization: f(40), Status: "allowed"}}}
	if _, ok := LiveUsable(good, liveWeekly, t0); !ok {
		t.Fatal("control: an allowed reading taken now is not usable")
	}
	if _, ok := LiveUsable(good, liveWeekly, t0.Add(time.Second)); ok {
		t.Fatal("an allowed reading from before now counted as live")
	}
	failed := good
	failed.ErrorKind = KindRateLimited
	if _, ok := LiveUsable(failed, liveWeekly, t0); ok {
		t.Fatal("a 429 attempt counted as usable")
	}
	past := t0.Add(-time.Minute)
	reset := Row{Source: SourceUsageAPI, ReadAt: t0, Windows: map[string]Window{FiveHour: {Utilization: f(100), ResetsAt: &past}}}
	if _, ok := LiveUsable(reset, liveWeekly, t0); !ok {
		t.Fatal("a window at 100% whose reset has passed kept the account limited")
	}
	if _, ok := LiveUsable(Row{Source: SourceUsageAPI, ReadAt: t0, Windows: map[string]Window{}}, liveWeekly, t0); ok {
		t.Fatal("a usage reading with no window counted as usable")
	}
}

// TestACoolingAccountIsNotHeldUntilARejectedReset: a non-cooling account whose
// probe says `rejected` is held until the answer's reset (it is out of quota),
// but a COOLING one is read again at the ordinary cadence — 2xx: one interval
// (less a tenth); 429: the backoff — so a limit lifted before that reset is
// seen within an interval, not days later.
func TestACoolingAccountIsNotHeldUntilARejectedReset(t *testing.T) {
	reset := t0.Add(48 * time.Hour)
	rejected := Reading{Status: "rejected", ResetsAt: &reset, Windows: map[string]Window{SevenDay: {Utilization: f(100), Status: "rejected"}}}
	prev := Row{UsageAPI: UsageAPIRefused, TokenFP: ccpool.Fingerprint("tok")}
	for _, tc := range []struct {
		name    string
		res     Result
		cooling bool
		want    time.Time
	}{
		{"200 rejected, not cooling", Result{Status: http.StatusOK, HasReading: true, Reading: rejected}, false, reset},
		{"200 rejected, cooling", Result{Status: http.StatusOK, HasReading: true, Reading: rejected}, true, t0.Add(iv - iv/10)},
		{"429 rejected, not cooling", Result{Status: http.StatusTooManyRequests, HasReading: true, Reading: rejected}, false, reset},
		{"429 rejected, cooling", Result{Status: http.StatusTooManyRequests, HasReading: true, Reading: rejected}, true, t0.Add(2 * iv)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := StepConfig{Interval: iv}
			if tc.cooling {
				sc.CoolingUntil = t0.Add(72 * time.Hour)
			}
			r, ok := Step(context.Background(), prev, "alpha", "tok", t0, sc, &scripted{probe: []Result{tc.res}})
			if !ok || !r.NextAt.Equal(tc.want) {
				t.Fatalf("NextAt = %s (ok=%v), want %s", r.NextAt.UTC().Format(time.RFC3339), ok, tc.want.UTC().Format(time.RFC3339))
			}
		})
	}
}
