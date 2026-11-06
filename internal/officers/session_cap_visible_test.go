package officers_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// ---------------------------------------------------------------------------
// THE JUDGEMENT SESSION CAP IS VISIBLE (operator, 2026-11-06). A judgement
// pool that has started all of its max_sessions_per_day sessions is not woken
// for new work. Before this, nothing said so: the pool simply stopped, with
// items waiting and zero log lines. Now the loop logs it ONCE per (officer,
// UTC day) — when the cap is first seen with claimable judgement items
// waiting — and muster_officer_judgement_cap_reached counts the officers in
// that state.
//
// Fixture values are pairwise distinct — 4 sessions used, a cap of 3 (the
// operator lowered it mid-day), 5 items waiting, judgement max_instances 2 —
// so a line that printed the wrong one cannot pass.
// ---------------------------------------------------------------------------

// daySessions is a SessionStore whose day's entries are set by the test.
type daySessions struct {
	mu   sync.Mutex
	days map[string][]string
	err  error
}

func (s *daySessions) set(day string, entries ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.days[day] = entries
}

func (s *daySessions) AdmitSession(context.Context, string, string, string, int, time.Time) (officers.Admission, error) {
	return officers.Admission{}, nil
}
func (s *daySessions) ReleaseSession(context.Context, string, string, string) error { return nil }
func (s *daySessions) ResolveSession(context.Context, string, string, string, string) error {
	return nil
}
func (s *daySessions) Sessions(_ context.Context, _, day string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.days[day], s.err
}

const capVisibleTiers = `"tiers":{"routine":{"model":"haiku","max_instances":3},"judgement":{"model":"opus","max_instances":2,"max_sessions_per_day":3}}`

// capGauge is muster_officer_judgement_cap_reached as the default registry
// serves it; ok false when no such metric is registered.
func capGauge(t *testing.T) (v float64, ok bool) {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "muster_officer_judgement_cap_reached" && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return 0, false
}

type capVisRig struct {
	add   func(subject string) string
	store *daySessions
	fleet *poolFleet
	clk   *clock
	rec   *officers.Reconciler
	logs  *bytes.Buffer
}

func newCapVisRig(t *testing.T) *capVisRig {
	t.Helper()
	hq := hqtest.New(map[string]string{tierTok: tierID})
	t.Cleanup(hq.Close)
	clk := &clock{t: time.Date(2001, 1, 1, 18, 20, 0, 0, time.UTC)}
	hq.Now = clk.Now
	list, err := officers.Parse(`[{"id":"`+tierID+`","hq_url":"`+hq.BaseURL()+`","idle_scale_to_zero_after":"10m",`+capVisibleTiers+`}]`,
		env(map[string]string{officers.TokenEnvPrefix + officers.TokenEnvSuffix(tierID): tierTok}), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	r := &capVisRig{store: &daySessions{days: map[string][]string{}}, fleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}},
		clk: clk, logs: &bytes.Buffer{}}
	r.add = func(subject string) string {
		return hq.AddTieredItem(tierID, "gpu.alert", subject, officers.TierJudgement)
	}
	r.rec, err = officers.NewReconciler(officers.Config{Registry: officers.NewRegistry(list), HQ: officers.NewClientHQ(nil), Fleet: r.fleet,
		Logger: log.New(r.logs, "", 0), Now: clk.Now, Judgement: officers.NewJudgementLedger(r.store, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(r.logs.String())
		}
	})
	return r
}

func (r *capVisRig) check(t *testing.T) {
	t.Helper()
	if res := r.rec.Check(context.Background(), tierID, true); res.Err != nil {
		t.Fatalf("pass: %v", res.Err)
	}
}

// capLines is every log line naming the cap.
func (r *capVisRig) capLines() []string {
	var out []string
	for _, l := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(l, "judgement cap reached") {
			out = append(out, l)
		}
	}
	return out
}

// TestAJudgementPoolAtItsSessionCapIsLoggedOncePerUTCDayAndCounted: at the cap
// with nothing waiting, nothing is said (the pool is not being held back);
// once claimable judgement items wait, exactly one line names the officer,
// used/cap, the waiting count and the UTC time the cap resets, and the gauge
// reads 1. The next poll the same day adds no line. The next UTC day the
// ledger is empty, the pool wakes and the gauge reads 0; capped again that
// day, it is logged again, once.
func TestAJudgementPoolAtItsSessionCapIsLoggedOncePerUTCDayAndCounted(t *testing.T) {
	r := newCapVisRig(t)
	r.store.set("2001-01-01", "unknown:a1", "unknown:a2", "unknown:a3", "unknown:a4")

	r.check(t)
	if got := r.capLines(); len(got) != 0 {
		t.Fatalf("at the cap with NO item waiting: %d cap line(s), want none:\n%s", len(got), strings.Join(got, "\n"))
	}
	if v, ok := capGauge(t); !ok || v != 0 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v (registered %v) with nothing waiting, want 0", v, ok)
	}

	for _, s := range []string{"node-a", "node-b", "node-c", "node-d", "node-e"} {
		r.add(s)
	}
	r.check(t)
	want1 := "officers: gpu-health:fleet (judgement pool): judgement cap reached (4/3 until 00:00 UTC): 5 claimable judgement item(s) wait in HQ " +
		"and are not claimed until the cap resets at 2001-01-02T00:00:00Z (tiers.judgement.max_sessions_per_day; logged once per UTC day)"
	if got := r.capLines(); len(got) != 1 || got[0] != want1 {
		t.Fatalf("cap reached with 5 waiting: cap lines\n%s\nwant exactly\n%s", strings.Join(got, "\n"), want1)
	}
	if v, ok := capGauge(t); !ok || v != 1 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v (registered %v) at the cap with items waiting, want 1", v, ok)
	}
	if starts, _ := r.fleet.take(); len(starts) != 0 {
		t.Fatalf("a capped judgement pool was started: %v", starts)
	}

	r.clk.Advance(time.Minute)
	r.check(t)
	if got := r.capLines(); len(got) != 1 {
		t.Fatalf("second poll the same UTC day: %d cap lines, want still 1:\n%s", len(got), strings.Join(got, "\n"))
	}
	if v, _ := capGauge(t); v != 1 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v on the second capped poll, want 1", v)
	}

	// 00:05 UTC the next day: a new day's ledger, empty — the pool wakes.
	r.clk.Advance(5*time.Hour + 44*time.Minute)
	r.check(t)
	if starts, _ := r.fleet.take(); len(starts) != 2 {
		t.Fatalf("the next UTC day: started %v, want the judgement pool's 2 instances", starts)
	}
	if v, _ := capGauge(t); v != 0 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v after the UTC day reset, want 0", v)
	}
	if got := r.capLines(); len(got) != 1 {
		t.Fatalf("the next UTC day, uncapped: %d cap lines, want still 1", len(got))
	}

	// Capped again that day: logged again, once, with that day's numbers.
	r.store.set("2001-01-02", "unknown:b1", "unknown:b2", "unknown:b3")
	r.clk.Advance(time.Minute)
	r.check(t)
	r.clk.Advance(time.Minute)
	r.check(t)
	want2 := "officers: gpu-health:fleet (judgement pool): judgement cap reached (3/3 until 00:00 UTC): 5 claimable judgement item(s) wait in HQ " +
		"and are not claimed until the cap resets at 2001-01-03T00:00:00Z (tiers.judgement.max_sessions_per_day; logged once per UTC day)"
	if got := r.capLines(); len(got) != 2 || got[1] != want2 {
		t.Fatalf("capped again the next day: cap lines\n%s\nwant 2, the second exactly\n%s", strings.Join(got, "\n"), want2)
	}
	if v, _ := capGauge(t); v != 1 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v capped again the next day, want 1", v)
	}
}

// TestAnUnreadableLedgerIsNotReportedAsTheCap: a ledger the loop cannot read
// leaves the judgement pool fail-closed (not woken for new work), but that is
// not the cap: no cap line, gauge 0 — the read failure has its own line.
// (An INVARIANT guard: the base tree already says nothing here; it pins that
// the new line is not printed from the fail-closed path.)
func TestAnUnreadableLedgerIsNotReportedAsTheCap(t *testing.T) {
	r := newCapVisRig(t)
	r.store.err = errors.New("fixture: ledger down")
	r.add("node-a")
	r.check(t)
	if got := r.capLines(); len(got) != 0 {
		t.Fatalf("an unreadable ledger was reported as the cap:\n%s", strings.Join(got, "\n"))
	}
	if v, _ := capGauge(t); v != 0 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v on an unreadable ledger, want 0", v)
	}
	if !strings.Contains(r.logs.String(), "fixture: ledger down") {
		t.Fatalf("the ledger failure was not logged:\n%s", r.logs.String())
	}
}
