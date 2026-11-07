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

// daySessions is a SessionStore whose entries, per officer and day, are set
// by the test.
type daySessions struct {
	mu   sync.Mutex
	days map[string][]string
	err  error
}

func (s *daySessions) set(officer, day string, entries ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.days[officer+"|"+day] = entries
}

func (s *daySessions) AdmitSession(context.Context, string, string, string, int, time.Time) (officers.Admission, error) {
	return officers.Admission{}, nil
}
func (s *daySessions) ReleaseSession(context.Context, string, string, string) error { return nil }
func (s *daySessions) ResolveSession(context.Context, string, string, string, string) error {
	return nil
}
func (s *daySessions) Sessions(_ context.Context, officer, day string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.days[officer+"|"+day], s.err
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
	hq    *hqtest.Server
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
	clk := &clock{t: time.Date(2000, 1, 1, 18, 20, 0, 0, time.UTC)}
	hq.Now = clk.Now
	list, err := officers.Parse(`[{"id":"`+tierID+`","hq_url":"`+hq.BaseURL()+`","idle_scale_to_zero_after":"10m",`+capVisibleTiers+`}]`,
		env(map[string]string{officers.TokenEnvPrefix + officers.TokenEnvSuffix(tierID): tierTok}), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	r := &capVisRig{hq: hq, store: &daySessions{days: map[string][]string{}}, fleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}},
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
	r.store.set(tierID, "2000-01-01", "unknown:a1", "unknown:a2", "unknown:a3", "unknown:a4")

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
		"and are not claimed until the cap resets at 2000-01-02T00:00:00Z (tiers.judgement.max_sessions_per_day; logged once per UTC day)"
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

	// A pass that fails cannot see the cap, so the gauge does not keep a
	// stale 1 (round 1 F2): an outage clears it, and so does a confirmed HQ
	// fault (two 404s, then the fault scale-down). The next good pass sets it
	// again without a second line.
	r.hq.SetDown(true)
	if res := r.rec.Check(context.Background(), tierID, true); res.Err == nil {
		t.Fatal("HQ down: the pass did not fail")
	}
	if v, _ := capGauge(t); v != 0 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v after a failed pass, want 0", v)
	}
	r.hq.SetDown(false)
	r.check(t)
	if v, _ := capGauge(t); v != 1 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v once HQ answers again, want 1", v)
	}
	r.hq.SetStatus(404)
	r.rec.Check(context.Background(), tierID, true)
	if res := r.rec.Check(context.Background(), tierID, true); res.Err == nil || !strings.Contains(r.logs.String(), "HQ refuses this officer") {
		t.Fatalf("two 404s: err %v; want a confirmed configuration fault", res.Err)
	}
	if v, _ := capGauge(t); v != 0 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v after a fault scale-down, want 0", v)
	}
	r.hq.SetStatus(0)
	r.check(t)
	if v, _ := capGauge(t); v != 1 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v once HQ answers again after the fault, want 1", v)
	}
	if got := r.capLines(); len(got) != 1 {
		t.Fatalf("outage and fault the same UTC day: %d cap lines, want still 1:\n%s", len(got), strings.Join(got, "\n"))
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
	r.store.set(tierID, "2000-01-02", "unknown:b1", "unknown:b2", "unknown:b3")
	r.clk.Advance(time.Minute)
	r.check(t)
	r.clk.Advance(time.Minute)
	r.check(t)
	want2 := "officers: gpu-health:fleet (judgement pool): judgement cap reached (3/3 until 00:00 UTC): 5 claimable judgement item(s) wait in HQ " +
		"and are not claimed until the cap resets at 2000-01-03T00:00:00Z (tiers.judgement.max_sessions_per_day; logged once per UTC day)"
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

// TestTheCapLineIsPerOfficerAndResetsAtUTCMidnightWhateverTheClocksZone
// (round 1 F3): two officers in one loop each get their own line the day
// each is capped — the once-per-day memory is per officer, not per loop —
// and the reset is UTC midnight even when muster's clock is in another zone.
// The clock reads 20:00 on 1 January at UTC-6, which is 02:00 on 2 January
// UTC: a reset computed on the local date would say 2000-01-02, not
// 2000-01-03.
func TestTheCapLineIsPerOfficerAndResetsAtUTCMidnightWhateverTheClocksZone(t *testing.T) {
	const (
		edgeID  = "disk-health:edge"
		edgeTok = "tok-edge-7c41d0" // a fixture, nobody's token
	)
	hq := hqtest.New(map[string]string{tierTok: tierID, edgeTok: edgeID})
	t.Cleanup(hq.Close)
	west := time.FixedZone("UTC-6", -6*60*60)
	clk := &clock{t: time.Date(2000, 1, 1, 20, 0, 0, 0, west)}
	hq.Now = clk.Now
	edgeTiers := `"tiers":{"routine":{"model":"haiku","max_instances":3},"judgement":{"model":"opus","max_instances":2,"max_sessions_per_day":2}}`
	list, err := officers.Parse(`[{"id":"`+tierID+`","hq_url":"`+hq.BaseURL()+`","idle_scale_to_zero_after":"10m",`+capVisibleTiers+`},`+
		`{"id":"`+edgeID+`","hq_url":"`+hq.BaseURL()+`","idle_scale_to_zero_after":"10m",`+edgeTiers+`}]`,
		env(map[string]string{
			officers.TokenEnvPrefix + officers.TokenEnvSuffix(tierID): tierTok,
			officers.TokenEnvPrefix + officers.TokenEnvSuffix(edgeID): edgeTok,
		}), "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	store := &daySessions{days: map[string][]string{}}
	logs := &bytes.Buffer{}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry(list), HQ: officers.NewClientHQ(nil),
		Fleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}}, Logger: log.New(logs, "", 0), Now: clk.Now,
		Judgement: officers.NewJudgementLedger(store, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	pass := func() {
		t.Helper()
		for _, id := range []string{tierID, edgeID} {
			if res := rec.Check(context.Background(), id, true); res.Err != nil {
				t.Fatalf("pass %s: %v", id, res.Err)
			}
		}
	}
	capLines := func() []string {
		var out []string
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, "judgement cap reached") {
				out = append(out, l)
			}
		}
		return out
	}
	for _, s := range []string{"node-a", "node-b", "node-c", "node-d", "node-e"} {
		hq.AddTieredItem(tierID, "gpu.alert", s, officers.TierJudgement)
	}
	hq.AddTieredItem(edgeID, "disk.alert", "disk-a", officers.TierJudgement)

	// The ledger is keyed by the UTC day: 2000-01-02, though the clock's
	// local date is 1 January.
	store.set(tierID, "2000-01-02", "unknown:a1", "unknown:a2", "unknown:a3", "unknown:a4")
	pass()
	wantA := "officers: gpu-health:fleet (judgement pool): judgement cap reached (4/3 until 00:00 UTC): 5 claimable judgement item(s) wait in HQ " +
		"and are not claimed until the cap resets at 2000-01-03T00:00:00Z (tiers.judgement.max_sessions_per_day; logged once per UTC day)"
	if got := capLines(); len(got) != 1 || got[0] != wantA {
		t.Fatalf("the first officer capped on a UTC-6 clock: cap lines\n%s\nwant exactly\n%s", strings.Join(got, "\n"), wantA)
	}
	if v, _ := capGauge(t); v != 1 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v with one officer capped, want 1", v)
	}

	// The second officer reaches its own cap later the same UTC day: its
	// own line, and the first officer's is not repeated.
	store.set(edgeID, "2000-01-02", "unknown:b1", "unknown:b2")
	clk.Advance(time.Minute)
	pass()
	wantB := "officers: disk-health:edge (judgement pool): judgement cap reached (2/2 until 00:00 UTC): 1 claimable judgement item(s) wait in HQ " +
		"and are not claimed until the cap resets at 2000-01-03T00:00:00Z (tiers.judgement.max_sessions_per_day; logged once per UTC day)"
	if got := capLines(); len(got) != 2 || got[0] != wantA || got[1] != wantB {
		t.Fatalf("the second officer capped the same day: cap lines\n%s\nwant\n%s\n%s", strings.Join(got, "\n"), wantA, wantB)
	}
	if v, _ := capGauge(t); v != 2 {
		t.Fatalf("muster_officer_judgement_cap_reached = %v with both officers capped, want 2", v)
	}
}
