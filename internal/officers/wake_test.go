package officers_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

const (
	relID  = "release:example-web"
	relTok = "tok-release-0f9e8d" // a value nobody's: a fixture
)

// fakeFleet is a Fleet over a map of instance -> up.
type fakeFleet struct {
	mu      sync.Mutex
	up      map[int]bool
	exists  map[int]bool
	starts  []int
	stops   []int
	kickoff []string
	blind   bool
}

func newFleet() *fakeFleet { return &fakeFleet{up: map[int]bool{}, exists: map[int]bool{}} }

func (f *fakeFleet) Instances(_ context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blind {
		return nil, errors.New("provisioner blind")
	}
	var out []officers.InstanceState
	for n := 1; n <= o.MaxInstances; n++ {
		out = append(out, officers.InstanceState{N: n, Name: officers.InstanceName(o.ID, n), Exists: f.exists[n], Up: f.up[n]})
	}
	return out, nil
}

func (f *fakeFleet) Start(_ context.Context, o officers.Officer, n int, kickoff string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up[n], f.exists[n] = true, true
	f.starts = append(f.starts, n)
	f.kickoff = append(f.kickoff, kickoff)
	return nil
}

func (f *fakeFleet) Stop(_ context.Context, o officers.Officer, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up[n] = false
	f.stops = append(f.stops, n)
	return nil
}

func (f *fakeFleet) upCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := 0
	for _, u := range f.up {
		if u {
			c++
		}
	}
	return c
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type rig struct {
	hq    *hqtest.Server
	fleet *fakeFleet
	clk   *clock
	rec   *officers.Reconciler
	o     officers.Officer
	logs  *bytes.Buffer
}

func newRig(t *testing.T, max int, idle time.Duration) *rig {
	t.Helper()
	hq := hqtest.New(map[string]string{relTok: relID})
	t.Cleanup(hq.Close)
	clk := &clock{t: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
	hq.Now = clk.Now
	o := officers.Officer{ID: relID, HQURL: hq.BaseURL(), MaxInstances: max, IdleAfter: idle, PollInterval: time.Minute, Owner: 1}.WithToken(relTok)
	r := &rig{hq: hq, fleet: newFleet(), clk: clk, o: o, logs: &bytes.Buffer{}}
	r.rec = r.newReconciler(t)
	return r
}

func (r *rig) newReconciler(t *testing.T) *officers.Reconciler {
	t.Helper()
	rec, err := officers.NewReconciler(officers.Config{
		Registry: officers.NewRegistry([]officers.Officer{r.o}),
		HQ:       officers.NewClientHQ(nil),
		Fleet:    r.fleet,
		Logger:   log.New(r.logs, "", 0),
		Now:      r.clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (r *rig) check(t *testing.T) officers.PassResult {
	t.Helper()
	return r.rec.Check(context.Background(), relID, false)
}

func TestAnOpenItemWakesOneInstance(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	if res := r.check(t); res.Err != nil || len(res.Decision.Start) != 0 {
		t.Fatalf("no items: want no start, got %+v", res)
	}
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	res := r.check(t)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := r.fleet.upCount(); got != 1 {
		t.Fatalf("one open item: %d instances up, want 1 (decision %+v)", got, res.Decision)
	}
	if r.fleet.starts[0] != 1 {
		t.Fatalf("started instance %d, want 1", r.fleet.starts[0])
	}
	// A second pass with the instance up changes nothing.
	if res := r.check(t); len(res.Decision.Start) != 0 || len(res.Decision.Stop) != 0 {
		t.Fatalf("steady state moved: %+v", res.Decision)
	}
}

func TestMoreItemsScaleToMaxInstances(t *testing.T) {
	r := newRig(t, 2, 30*time.Minute)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	if got := r.fleet.upCount(); got != 1 {
		t.Fatalf("one item: %d up, want 1", got)
	}
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.hq.AddItem(relID, "release.evaluate", "svc:v3")
	r.check(t)
	if got := r.fleet.upCount(); got != 2 {
		t.Fatalf("three items, max 2: %d up, want 2", got)
	}
	r.hq.AddItem(relID, "release.evaluate", "svc:v4")
	r.check(t)
	if got := r.fleet.upCount(); got != 2 {
		t.Fatalf("never past max_instances: %d up", got)
	}
}

func TestIdleForTheWindowScalesToZero(t *testing.T) {
	r := newRig(t, 2, 30*time.Minute)
	id := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	if r.fleet.upCount() != 1 {
		t.Fatal("not woken")
	}
	// The instance claims and works the item: busy, nothing stops.
	r.hq.SetClaim(id, officers.InstanceName(relID, 1), r.clk.Now().Add(10*time.Minute))
	r.clk.Advance(5 * time.Minute)
	if res := r.check(t); res.Decision.Idle || len(res.Decision.Stop) != 0 {
		t.Fatalf("a held claim is not idle: %+v", res.Decision)
	}
	// It completes: the officer is idle from now.
	completeVia(t, r, id)
	r.check(t) // first idle pass: idleSince = now
	r.clk.Advance(29 * time.Minute)
	if res := r.check(t); len(res.Decision.Stop) != 0 {
		t.Fatalf("29m into a 30m window: stopped %v", res.Decision.Stop)
	}
	r.clk.Advance(1 * time.Minute)
	res := r.check(t)
	if len(res.Decision.Stop) != 1 || res.Decision.Stop[0] != 1 {
		t.Fatalf("at 30m idle: stop = %v, want [1]", res.Decision.Stop)
	}
	if r.fleet.upCount() != 0 {
		t.Fatal("still up after the idle window")
	}
}

// completeVia completes item id as instance 1 through the real client.
func completeVia(t *testing.T, r *rig, id string) {
	t.Helper()
	c := officers.NewClient(r.o, nil)
	if _, err := c.Complete(context.Background(), officers.ItemID(id), officers.InstanceName(relID, 1), officers.Completion{Outcome: "done", Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
}

func TestAnOpenItemArrivingMidWindowResetsIdle(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	r.fleet.up[1], r.fleet.exists[1] = true, true
	r.check(t) // idle since now
	r.clk.Advance(20 * time.Minute)
	id := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	if res := r.check(t); res.Decision.Idle {
		t.Fatal("an open item is not idle")
	}
	completeAfterClaim(t, r, id)
	r.check(t) // idle again, fresh window
	r.clk.Advance(20 * time.Minute)
	if res := r.check(t); len(res.Decision.Stop) != 0 {
		t.Fatalf("the idle window did not restart: stopped after 20m of a fresh 30m window")
	}
}

func completeAfterClaim(t *testing.T, r *rig, id string) {
	t.Helper()
	c := officers.NewClient(r.o, nil)
	if _, err := c.Claim(context.Background(), officers.ItemID(id), officers.InstanceName(relID, 1), 60); err != nil {
		t.Fatal(err)
	}
	completeVia(t, r, id)
}

func TestHQDownChangesNothingAndBacksOff(t *testing.T) {
	r := newRig(t, 2, time.Minute)
	r.fleet.up[1], r.fleet.exists[1] = true, true
	r.check(t)
	r.clk.Advance(2 * time.Minute) // past the idle window: a working HQ would stop it
	r.hq.SetDown(true)
	res := r.check(t)
	if !errors.Is(res.Err, officers.ErrHQUnavailable) {
		t.Fatalf("err = %v, want ErrHQUnavailable", res.Err)
	}
	if len(r.fleet.starts)+len(r.fleet.stops) != 0 || r.fleet.upCount() != 1 {
		t.Fatalf("HQ down moved the fleet: starts %v stops %v", r.fleet.starts, r.fleet.stops)
	}
	// Backoff: the next pass inside the backoff window is skipped without
	// touching HQ.
	r.hq.ResetRequests()
	r.clk.Advance(30 * time.Second)
	if res := r.check(t); !res.Skipped {
		t.Fatalf("inside backoff: want skipped, got %+v", res)
	}
	if n := len(r.hq.Requests()); n != 0 {
		t.Fatalf("a skipped pass sent %d HQ requests", n)
	}
	// Failures double the wait: 1 failure = 2m, 2 = 4m, capped at 10m.
	for i, want := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		if got := officers.Backoff(r.o, i+1); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, want)
		}
	}
	// HQ back: the pass after the backoff runs and acts.
	r.hq.SetDown(false)
	r.clk.Advance(2 * time.Minute)
	if res := r.check(t); res.Skipped || res.Err != nil {
		t.Fatalf("after backoff with HQ up: %+v", res)
	}
}

func TestABlindFleetChangesNothing(t *testing.T) {
	r := newRig(t, 1, time.Minute)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.fleet.blind = true
	if res := r.check(t); res.Err == nil || len(r.fleet.starts) != 0 {
		t.Fatalf("a blind fleet must change nothing: %+v starts %v", res, r.fleet.starts)
	}
}

func TestARestartedLoopMakesTheSameDecisions(t *testing.T) {
	r := newRig(t, 2, 30*time.Minute)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.check(t)
	if r.fleet.upCount() != 2 {
		t.Fatal("not at 2")
	}
	// "Restart": a brand-new reconciler over the same HQ and fleet state.
	r.rec = r.newReconciler(t)
	if res := r.check(t); len(res.Decision.Start) != 0 || len(res.Decision.Stop) != 0 {
		t.Fatalf("a restart re-decided: %+v", res.Decision)
	}
	// Restart with instance 2 having died: it is started again, 1 is left.
	r.fleet.up[2] = false
	r.rec = r.newReconciler(t)
	res := r.check(t)
	if len(res.Decision.Start) != 1 || res.Decision.Start[0] != 2 {
		t.Fatalf("after restart with 2 down: start %v, want [2]", res.Decision.Start)
	}
	// Idle after a restart: the window starts again (it can only delay a stop).
	for id := 1; id <= 2; id++ {
		completeAfterClaim(t, r, itoa(id))
	}
	r.rec = r.newReconciler(t)
	if res := r.check(t); len(res.Decision.Stop) != 0 {
		t.Fatalf("first idle pass after a restart stopped %v", res.Decision.Stop)
	}
	r.clk.Advance(30 * time.Minute)
	if res := r.check(t); len(res.Decision.Stop) != 2 {
		t.Fatalf("after a full window post-restart: stop %v, want both", res.Decision.Stop)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }

func TestAClaimHeldByAStoppedInstanceIsReleased(t *testing.T) {
	r := newRig(t, 2, 30*time.Minute)
	id := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	holder := officers.InstanceName(relID, 1)
	r.hq.SetClaim(id, holder, r.clk.Now().Add(20*time.Minute))
	// The operator (or a crash) stops instance 1 while it holds the claim.
	r.fleet.up[1] = false
	res := r.check(t)
	if len(res.Decision.Release) != 1 {
		t.Fatalf("release = %v, want the one held item", res.Decision.Release)
	}
	if it := r.hq.Item(id); it.Status != "open" || it.ClaimedBy != "" {
		t.Fatalf("item after release: %+v, want open and unheld", it)
	}
	var sawRelease bool
	for _, q := range r.hq.Requests() {
		if q.Path == "/api/v1/officers/"+relID+"/items/"+id+"/release" && q.Body["instance"] == holder {
			sawRelease = true
		}
	}
	if !sawRelease {
		t.Fatal("no release request naming the stopped holder reached HQ")
	}
	// A live claim held by an UP instance is never released.
	r.hq.SetClaim(id, officers.InstanceName(relID, 2), r.clk.Now().Add(20*time.Minute))
	r.fleet.up[2] = true
	if res := r.check(t); len(res.Decision.Release) != 0 {
		t.Fatalf("released a claim whose holder is up: %v", res.Decision.Release)
	}
	// Nor one held by somebody else's instance.
	r.hq.SetClaim(id, "some-other-harness-session", r.clk.Now().Add(20*time.Minute))
	if res := r.check(t); len(res.Decision.Release) != 0 {
		t.Fatalf("released a foreign claim: %v", res.Decision.Release)
	}
}

func TestAnExpiredLeaseIsClaimableAndWakes(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	id := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.SetClaim(id, "gone-session", r.clk.Now().Add(-time.Minute))
	res := r.check(t)
	if res.Decision.Claimable != 1 || r.fleet.upCount() != 1 {
		t.Fatalf("expired lease: claimable %d, up %d; want 1, 1", res.Decision.Claimable, r.fleet.upCount())
	}
}

func TestTheKickoffNamesTheOfficerAndCarriesNoToken(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	if len(r.fleet.kickoff) != 1 {
		t.Fatal("no kickoff")
	}
	k := r.fleet.kickoff[0]
	if !bytes.Contains([]byte(k), []byte(relID)) || !bytes.Contains([]byte(k), []byte(officers.ToolBrief)) {
		t.Fatalf("kickoff does not name the officer and officer_brief: %q", k)
	}
	if bytes.Contains([]byte(k), []byte(relTok)) {
		t.Fatal("the kickoff carries the officer token")
	}
	if bytes.Contains(r.logs.Bytes(), []byte(relTok)) {
		t.Fatal("the loop logged the officer token")
	}
}

// TestAllItemsFollowsHQsNextCursor: HQ pages the items list as {items, next}
// (civ-hq docs/OFFICERS.md). AllItems must follow `next`, or an officer with
// more open items than one page would be counted short.
func TestAllItemsFollowsHQsNextCursor(t *testing.T) {
	r := newRig(t, 1, time.Minute)
	for i := 0; i < 5; i++ {
		r.hq.AddItem(relID, "release.evaluate", "svc:v"+itoa(i))
	}
	r.hq.PageSize = 2
	got, err := officers.NewClient(r.o, nil).AllItems(context.Background(), officers.StatusOpen)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("AllItems = %d items over pages of 2, want 5", len(got))
	}
}
