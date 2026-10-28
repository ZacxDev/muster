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
	// reprompts records instance numbers re-prompted; reprompt holds the text.
	reprompts []int
	reprompt  []string
	pending   map[int]bool // a prompt is owed and not yet delivered
	busy      map[int]bool // a turn holds the instance's session: Reprompt answers ErrInstanceBusy
	errored   map[int]bool // the instance is in error (holds its slot)
}

func newFleet() *fakeFleet {
	return &fakeFleet{up: map[int]bool{}, exists: map[int]bool{}, pending: map[int]bool{}, errored: map[int]bool{}, busy: map[int]bool{}}
}

func (f *fakeFleet) Reprompt(_ context.Context, o officers.Officer, n int, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy[n] {
		return officers.ErrInstanceBusy
	}
	f.reprompts = append(f.reprompts, n)
	f.reprompt = append(f.reprompt, text)
	return nil
}

func (f *fakeFleet) Instances(_ context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blind {
		return nil, errors.New("provisioner blind")
	}
	var out []officers.InstanceState
	for n := 1; n <= o.MaxInstances; n++ {
		out = append(out, officers.InstanceState{N: n, Name: officers.InstanceName(o.ID, n), Exists: f.exists[n], Up: f.up[n] || f.errored[n],
			Error: f.errored[n], PromptPending: f.pending[n]})
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
	// A second pass right away, the instance just prompted: nothing more.
	if res := r.check(t); len(res.Decision.Start) != 0 || len(res.Decision.Stop) != 0 || len(res.Decision.Reprompt) != 0 {
		t.Fatalf("a just-prompted instance was moved again: %+v", res.Decision)
	}
	// The item is still open and unclaimed a reprompt interval later: the up
	// instance is prompted again rather than left waiting for ever.
	r.clk.Advance(officers.RepromptAfter)
	if res := r.check(t); len(res.Decision.Reprompt) != 1 || res.Decision.Reprompt[0] != 1 {
		t.Fatalf("an open item %s after the up instance's last prompt: reprompt = %v, want [1]", officers.RepromptAfter, res.Decision.Reprompt)
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

// TestAnUpInstanceThatFinishedIsPromptedForNewWork is the audit's scenario: an
// instance claims and completes item 1 and its turn ends; item 2 arrives. The
// instance is up, holds nothing, and no start is due (it is up) — so before
// the fix nothing ever prompted it, and the officer never went idle either.
// It must be re-prompted (rate-limited), then work item 2, then the officer
// idles to zero.
func TestAnUpInstanceThatFinishedIsPromptedForNewWork(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	first := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	completeAfterClaim(t, r, first)
	r.clk.Advance(time.Minute)
	second := r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	for i := 0; i < 120; i++ {
		r.check(t)
		r.clk.Advance(time.Minute)
		if len(r.fleet.reprompts) > 0 {
			break
		}
	}
	if len(r.fleet.reprompts) == 0 {
		t.Fatalf("120 polls with an open item and an up, idle instance: 0 reprompts (starts %v, stops %v)", r.fleet.starts, r.fleet.stops)
	}
	if !strings.Contains(r.fleet.reprompt[0], officers.ToolItems) || strings.Contains(r.fleet.reprompt[0], relTok) {
		t.Fatalf("reprompt text %q must name officer_items and never carry the token", r.fleet.reprompt[0])
	}
	if len(r.fleet.starts) != 1 {
		t.Fatalf("starts = %v: an up instance is re-prompted, not started again", r.fleet.starts)
	}
	// Rate limit: the next pass does not prompt again.
	n := len(r.fleet.reprompts)
	r.check(t)
	if len(r.fleet.reprompts) != n {
		t.Fatal("re-prompted twice inside one reprompt interval")
	}
	// It works item 2; then the officer idles out.
	completeAfterClaim(t, r, second)
	r.check(t)
	r.clk.Advance(30 * time.Minute)
	if res := r.check(t); len(res.Decision.Stop) != 1 {
		t.Fatalf("after item 2, idle for the window: %+v", res.Decision)
	}
}

// TestNoRepromptWhileAPromptIsPendingOrAClaimIsHeld: an instance whose last
// prompt has not been delivered, or that holds a live claim, is never
// re-prompted, however long the item waits; a SECOND idle instance is, when
// there is a second claimable item.
func TestNoRepromptWhileAPromptIsPendingOrAClaimIsHeld(t *testing.T) {
	r := newRig(t, 2, 30*time.Minute)
	held := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.check(t) // starts 1 and 2
	r.hq.SetClaim(held, officers.InstanceName(relID, 1), r.clk.Now().Add(time.Hour))
	r.fleet.pending[2] = true
	r.clk.Advance(officers.RepromptAfter * 3)
	if res := r.check(t); len(res.Decision.Reprompt) != 0 {
		t.Fatalf("re-prompted a claim holder or a pending prompt: %v", res.Decision.Reprompt)
	}
	r.fleet.pending[2] = false
	if res := r.check(t); len(res.Decision.Reprompt) != 1 || res.Decision.Reprompt[0] != 2 {
		t.Fatalf("instance 2, delivered and idle, one claimable item: reprompt = %v, want [2]", res.Decision.Reprompt)
	}
}

// TestAnErrorSlotIsLoggedOnceAndCounted: an instance in error holds its slot
// (no start), and the loop says so — once per change, not once per poll — and
// counts it on muster_officer_error_slots.
func TestAnErrorSlotIsLoggedOnceAndCounted(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.fleet.errored[1], r.fleet.exists[1] = true, true
	for i := 0; i < 5; i++ {
		r.check(t)
		r.clk.Advance(time.Minute)
	}
	if len(r.fleet.starts) != 0 || len(r.fleet.reprompts) != 0 {
		t.Fatalf("started (%v) or re-prompted (%v) an error slot", r.fleet.starts, r.fleet.reprompts)
	}
	if got := strings.Count(r.logs.String(), "instance 1 is in error"); got != 1 {
		t.Fatalf("error-slot log lines over 5 polls = %d, want exactly 1:\n%s", got, r.logs.String())
	}
	if got := officers.ErrorSlots(); got != 1 {
		t.Fatalf("error slots gauge = %v, want 1", got)
	}
	r.fleet.errored[1] = false
	r.check(t)
	if !strings.Contains(r.logs.String(), "instance 1 left error") || officers.ErrorSlots() != 0 {
		t.Fatalf("recovery not logged or gauge not cleared (gauge %v):\n%s", officers.ErrorSlots(), r.logs.String())
	}
}

// TestAPermanentHQRefusalIsAConfigFault: a revoked token (401/403) or HQ
// without the officer API (404) is not an outage — once it has been seen on
// FaultStrikes consecutive passes. Then the loop logs it once, counts it on
// muster_officer_hq_faults, and scales the officer's up instances to zero,
// naming the fault as the reason. A 5xx in between neither confirms nor clears
// it; only a successful answer clears it ("HQ answers again").
func TestAPermanentHQRefusalIsAConfigFault(t *testing.T) {
	for _, code := range []int{401, 403, 404} {
		t.Run(itoa3(code), func(t *testing.T) {
			r := newRig(t, 2, 30*time.Minute)
			r.fleet.up[1], r.fleet.up[2], r.fleet.exists[1], r.fleet.exists[2] = true, true, true, true
			r.hq.SetStatus(code)
			if res := r.check(t); res.Err == nil || len(r.fleet.stops) != 0 || officers.HQFaults() != 0 {
				t.Fatalf("HQ %d once: err %v, stops %v, gauge %v; want an error but no scale change yet", code, res.Err, r.fleet.stops, officers.HQFaults())
			}
			r.clk.Advance(time.Hour)
			if res := r.check(t); res.Err == nil || len(r.fleet.stops) != 2 {
				t.Fatalf("HQ %d twice: err %v, stops %v; want a fault and both instances stopped", code, res.Err, r.fleet.stops)
			}
			if officers.HQFaults() != 1 {
				t.Fatalf("hq faults gauge = %v, want 1", officers.HQFaults())
			}
			if want := "scaled instance 1 to zero (HQ configuration fault: " + itoa3(code) + ")"; !strings.Contains(r.logs.String(), want) {
				t.Fatalf("the scale-down does not name the fault (%q):\n%s", want, r.logs.String())
			}
			if strings.Contains(r.logs.String(), "to zero (idle") {
				t.Fatal("a fault scale-down was logged as idle")
			}
			r.clk.Advance(time.Hour)
			r.check(t)
			if got := strings.Count(r.logs.String(), "HQ refuses this officer"); got != 1 {
				t.Fatalf("fault log lines = %d, want 1", got)
			}
			// A 5xx in between: still a fault, not "answers again".
			r.hq.SetStatus(503)
			r.clk.Advance(time.Hour)
			r.check(t)
			if officers.HQFaults() != 1 || strings.Contains(r.logs.String(), "HQ answers again") {
				t.Fatalf("a 5xx after the fault cleared it (gauge %v)", officers.HQFaults())
			}
			r.hq.SetStatus(0)
			r.clk.Advance(time.Hour)
			if res := r.check(t); res.Err != nil || officers.HQFaults() != 0 || !strings.Contains(r.logs.String(), "HQ answers again") {
				t.Fatalf("recovery: %+v gauge %v", res, officers.HQFaults())
			}
		})
	}
	r := newRig(t, 1, time.Minute)
	r.fleet.up[1], r.fleet.exists[1] = true, true
	r.hq.SetStatus(503)
	r.check(t)
	r.clk.Advance(time.Hour)
	if r.check(t); len(r.fleet.stops) != 0 {
		t.Fatal("a 5xx stopped instances")
	}
}

// TestOneHQRefusalThenSuccessChangesNothing: a single 404 (an HQ deploy
// mid-rollout) followed by a good answer neither scales anything down nor
// leaves a fault behind.
func TestOneHQRefusalThenSuccessChangesNothing(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	r.fleet.up[1], r.fleet.exists[1] = true, true
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.SetStatus(404)
	r.check(t)
	r.hq.SetStatus(0)
	r.clk.Advance(time.Hour)
	r.check(t)
	r.hq.SetStatus(404)
	r.clk.Advance(time.Hour)
	r.check(t) // a 404 again, but not CONSECUTIVE with the first
	if len(r.fleet.stops) != 0 || officers.HQFaults() != 0 || strings.Contains(r.logs.String(), "HQ refuses this officer") {
		t.Fatalf("non-consecutive single 404s: stops %v gauge %v\n%s", r.fleet.stops, officers.HQFaults(), r.logs.String())
	}
}

func itoa3(n int) string {
	return string(rune('0'+n/100)) + string(rune('0'+n/10%10)) + string(rune('0'+n%10))
}

// TestAFreshItemGetsOnePromptNotOnePerInstance: two up, idle instances and ONE
// claimable item. Instance 1 was prompted a minute ago (it is on its way to the
// queue); instance 2 was prompted long ago. The item is covered: instance 2 is
// not prompted too. A second item is not covered: then instance 2 is.
func TestAFreshItemGetsOnePromptNotOnePerInstance(t *testing.T) {
	r := newRig(t, 2, 30*time.Minute)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.check(t) // starts 1 and 2, both prompted now
	completeAfterClaim(t, r, "1")
	completeAfterClaim(t, r, "2")
	r.clk.Advance(officers.RepromptAfter * 2)
	r.hq.AddItem(relID, "release.evaluate", "svc:v3")
	if res := r.check(t); len(res.Decision.Reprompt) != 1 || res.Decision.Reprompt[0] != 1 {
		t.Fatalf("one fresh item, two idle instances: reprompt = %v, want [1]", res.Decision.Reprompt)
	}
	r.clk.Advance(time.Minute)
	if res := r.check(t); len(res.Decision.Reprompt) != 0 {
		t.Fatalf("the item is covered by instance 1's fresh prompt, yet reprompt = %v", res.Decision.Reprompt)
	}
	r.hq.AddItem(relID, "release.evaluate", "svc:v4")
	if res := r.check(t); len(res.Decision.Reprompt) != 1 || res.Decision.Reprompt[0] != 2 {
		t.Fatalf("a second item: reprompt = %v, want [2]", res.Decision.Reprompt)
	}
}

// TestAClaimHolderIsNotReprompted: max 1, instance 1 holds item A (mid-work),
// item B is open. Nothing can be started, and instance 1 is not prompted: it
// returns to the queue when it completes A.
func TestAClaimHolderIsNotReprompted(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	a := r.hq.AddItem(relID, "release.evaluate", "svc:a")
	r.check(t)
	r.hq.SetClaim(a, officers.InstanceName(relID, 1), r.clk.Now().Add(time.Hour))
	r.hq.AddItem(relID, "release.evaluate", "svc:b")
	r.clk.Advance(officers.RepromptAfter * 3)
	if res := r.check(t); len(res.Decision.Reprompt) != 0 {
		t.Fatalf("re-prompted the instance working item A: %v", res.Decision.Reprompt)
	}
}

// TestABusyInstanceIsRepromptedOnceItsTurnEnds: an up instance with no claim
// whose session a turn still holds (its first turn working the queue) answers
// a re-prompt with ErrInstanceBusy. That is not a prompt: nothing is recorded,
// nothing is logged as a failure, and the loop tries again on the next pass —
// without waiting out the rate limit — so the prompt lands as soon as the turn
// ends.
func TestABusyInstanceIsRepromptedOnceItsTurnEnds(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	first := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.check(t)
	completeAfterClaim(t, r, first)
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.fleet.busy[1] = true
	r.clk.Advance(officers.RepromptAfter)
	// 18 minutes of a turn still running — deliberately NOT a multiple of
	// RepromptAfter, so the pass the turn ends on is inside the rate-limit window
	// of any wrongly-recorded "prompt" and only a correct loop prompts at once.
	for i := 0; i < 18; i++ {
		if res := r.check(t); len(res.ActErrs) != 0 {
			t.Fatalf("a busy instance's re-prompt counted as a failure: %v", res.ActErrs)
		}
		r.clk.Advance(time.Minute)
	}
	if len(r.fleet.reprompts) != 0 || strings.Contains(r.logs.String(), "re-prompt instance 1:") {
		t.Fatalf("delivered %v or logged a failure while the turn ran:\n%s", r.fleet.reprompts, r.logs.String())
	}
	r.fleet.busy[1] = false // the turn ends
	if r.check(t); len(r.fleet.reprompts) != 1 {
		t.Fatalf("the turn ended: reprompts = %v, want one at once", r.fleet.reprompts)
	}
}
