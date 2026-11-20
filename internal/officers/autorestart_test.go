package officers_test

import (
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
)

// The bounded auto-restart of an instance in error (autorestart.go). Before
// it, an instance in error held its slot until an operator stopped and
// started it: on the base, every test below that expects a restart sees
// none (a mutant that turns planRestarts off reproduces that, and each test
// goes red on its own assertion).
//
// The fixture constants are pairwise distinct and distinct from the
// defaults (5m, 4/day), so a mutant that hardcodes a default cannot pass:
// After is 3m and the cap 2 or 3.

// restartNotices records Config.AutoRestartNotice's events.
type restartNotices struct {
	mu sync.Mutex
	ev []officers.AutoRestartEvent
}

func (n *restartNotices) add(_ officers.Officer, e officers.AutoRestartEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ev = append(n.ev, e)
}

func (n *restartNotices) all() []officers.AutoRestartEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]officers.AutoRestartEvent(nil), n.ev...)
}

func (n *restartNotices) count(f func(officers.AutoRestartEvent) bool) int {
	c := 0
	for _, e := range n.all() {
		if f(e) {
			c++
		}
	}
	return c
}

const restartAfter = 3 * time.Minute

// newRestartRig is newRig with the auto-restart on (after, perDay).
func newRestartRig(t *testing.T, max int, after time.Duration, perDay int) (*rig, *restartNotices) {
	t.Helper()
	return newRestartRigWith(t, max, after, perDay, nil)
}

// newRestartRigWith is newRestartRig over store (nil: the book in memory).
func newRestartRigWith(t *testing.T, max int, after time.Duration, perDay int, store officers.RestartStore) (*rig, *restartNotices) {
	t.Helper()
	r := newRig(t, max, 30*time.Minute)
	n := &restartNotices{}
	r.rec = r.restartLoop(t, after, perDay, store, n)
	return r, n
}

// restartLoop is a NEW reconciler over r's fleet, HQ and clock — a fresh
// muster process — with the auto-restart on, over store, telling n.
func (r *rig) restartLoop(t *testing.T, after time.Duration, perDay int, store officers.RestartStore, n *restartNotices) *officers.Reconciler {
	t.Helper()
	cfg := officers.Config{
		Registry:          officers.NewRegistry([]officers.Officer{r.o}),
		HQ:                officers.NewBackends(nil),
		Fleet:             r.fleet,
		Logger:            log.New(r.logs, "", 0),
		Now:               r.clk.Now,
		AutoRestart:       officers.AutoRestart{After: after, MaxPerDay: perDay},
		AutoRestartNotice: n.add,
	}
	if store != nil {
		// As a person, the way internal/api runs it as the officer's owner
		// (the PG store's row-level security needs one).
		cfg.Restarts, cfg.RestartContext = store, func(ctx context.Context, _ officers.Officer) context.Context { return authz.Operator(ctx) }
	}
	rec, err := officers.NewReconciler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// errorOut puts instance n in error of class c (it exists, and holds its slot).
func (r *rig) errorOut(n int, c officers.ErrorClass) {
	r.fleet.mu.Lock()
	defer r.fleet.mu.Unlock()
	r.fleet.errored[n], r.fleet.exists[n], r.fleet.class[n] = true, true, c
}

func (r *rig) startsStops() ([]int, []int) {
	r.fleet.mu.Lock()
	defer r.fleet.mu.Unlock()
	return append([]int(nil), r.fleet.starts...), append([]int(nil), r.fleet.stops...)
}

// TestAnErroredInstanceIsRestartedAfterItsBackoff: an instance in error of a
// restartable class, with an item waiting, holds its slot until it has been
// in error for After — then the loop stops and starts it, logs it and tells
// the notice. Not one poll earlier.
func TestAnErroredInstanceIsRestartedAfterItsBackoff(t *testing.T) {
	r, notices := newRestartRig(t, 1, restartAfter, 3)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.errorOut(1, officers.ErrorKickoffStuck)

	for _, at := range []time.Duration{0, time.Minute, 2 * time.Minute, restartAfter - time.Second} {
		r.clk.Advance(at - r.clk.Now().Sub(time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)))
		if res := r.check(t); len(res.Decision.Restart) != 0 {
			t.Fatalf("restarted after %s in error, before its %s backoff: %+v", at, restartAfter, res.Decision.Restart)
		}
	}
	if starts, stops := r.startsStops(); len(starts) != 0 || len(stops) != 0 {
		t.Fatalf("before the backoff: starts %v stops %v, want none (it holds its slot)", starts, stops)
	}
	r.clk.Advance(time.Second) // exactly After in error
	res := r.check(t)
	if len(res.Decision.Restart) != 1 || res.Decision.Restart[0].N != 1 || res.Decision.Restart[0].Attempt != 1 || res.Decision.Restart[0].Max != 3 {
		t.Fatalf("after %s in error: Restart = %+v, want instance 1, attempt 1 of 3", restartAfter, res.Decision.Restart)
	}
	if len(res.Decision.Start) != 0 {
		t.Fatalf("the restart's slot was also filled by a start: Start = %v", res.Decision.Start)
	}
	starts, stops := r.startsStops()
	if !slices.Equal(stops, []int{1}) || !slices.Equal(starts, []int{1}) {
		t.Fatalf("restart: stops %v starts %v, want [1] and [1] (stop, then start)", stops, starts)
	}
	if !strings.Contains(r.logs.String(), "AUTO-RESTARTED instance 1 from error (kickoff_stuck, in error 3m0s): auto-restart 1 of 3 today") {
		t.Fatalf("the restart was not logged:\n%s", r.logs.String())
	}
	if got := notices.count(func(e officers.AutoRestartEvent) bool {
		return e.Restarted && e.Attempt == 1 && e.Max == 3 && e.Name == officers.InstanceName(relID, 1)
	}); got != 1 {
		t.Fatalf("restart notices = %d, want 1: %+v", got, notices.all())
	}
}

// TestTheAutoRestartBackoffDoubles: each later restart of the same instance
// that day waits twice as long in its new error as the one before: After,
// 2×After, 4×After.
func TestTheAutoRestartBackoffDoubles(t *testing.T) {
	r, _ := newRestartRig(t, 1, restartAfter, 3)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	for attempt, wait := range []time.Duration{restartAfter, 2 * restartAfter, 4 * restartAfter} {
		r.errorOut(1, officers.ErrorCrashLoop)
		r.check(t) // this pass sees the new error episode begin
		r.clk.Advance(wait - time.Second)
		if res := r.check(t); len(res.Decision.Restart) != 0 {
			t.Fatalf("restart %d came after %s in error, before its %s backoff", attempt+1, wait-time.Second, wait)
		}
		r.clk.Advance(time.Second)
		res := r.check(t)
		if len(res.Decision.Restart) != 1 || res.Decision.Restart[0].Attempt != attempt+1 {
			t.Fatalf("restart %d: after %s in error, Restart = %+v, want attempt %d", attempt+1, wait, res.Decision.Restart, attempt+1)
		}
		// Back in error at once: its next wait is timed from this restart.
	}
	if starts, _ := r.startsStops(); len(starts) != 3 {
		t.Fatalf("three backed-off restarts: %d starts, want 3", len(starts))
	}
}

// TestTheDailyAutoRestartCapHoldsTheInstance: past MaxPerDay restarts for the
// officer in a UTC day, an instance in error holds its slot again — logged
// and told ONCE for the episode, however many passes follow — and the next
// UTC day it is restarted again.
func TestTheDailyAutoRestartCapHoldsTheInstance(t *testing.T) {
	r, notices := newRestartRig(t, 1, restartAfter, 2)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	for i := 0; i < 2; i++ {
		r.errorOut(1, officers.ErrorKickoffStuck)
		r.check(t)
		r.clk.Advance(time.Hour) // overshoots every backoff
		if res := r.check(t); len(res.Decision.Restart) != 1 {
			t.Fatalf("restart %d within the cap: Restart = %+v", i+1, res.Decision.Restart)
		}
	}
	r.errorOut(1, officers.ErrorKickoffStuck)
	r.check(t)
	for i := 0; i < 5; i++ {
		r.clk.Advance(time.Hour)
		if res := r.check(t); len(res.Decision.Restart) != 0 {
			t.Fatalf("a third restart in one UTC day with a cap of 2: %+v", res.Decision.Restart)
		}
	}
	if starts, _ := r.startsStops(); len(starts) != 2 {
		t.Fatalf("cap 2: %d starts, want 2", len(starts))
	}
	if got := strings.Count(r.logs.String(), "auto-restarts for today (UTC) are spent"); got != 1 {
		t.Fatalf("held-by-cap log lines over 5 passes = %d, want exactly 1:\n%s", got, r.logs.String())
	}
	if got := notices.count(func(e officers.AutoRestartEvent) bool {
		return e.Held && strings.Contains(e.HeldWhy, "2 auto-restarts")
	}); got != 1 {
		t.Fatalf("held notices = %d, want 1: %+v", got, notices.all())
	}
	// The next UTC day: the count starts again.
	now := r.clk.Now()
	r.clk.Advance(time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 1, 0, time.UTC).Sub(now))
	if res := r.check(t); len(res.Decision.Restart) != 1 || res.Decision.Restart[0].Attempt != 1 {
		t.Fatalf("a new UTC day: Restart = %+v, want attempt 1", res.Decision.Restart)
	}
	if got := notices.count(func(e officers.AutoRestartEvent) bool { return e.Recovered }); got != 0 {
		t.Fatalf("recovered told before the instance left error: %+v", notices.all())
	}
	r.check(t) // it left error: the held notice is withdrawn
	if got := notices.count(func(e officers.AutoRestartEvent) bool { return e.Recovered }); got != 1 {
		t.Fatalf("recovered notices = %d, want 1: %+v", got, notices.all())
	}
}

// TestANonRestartableErrorClassIsHeld: an instance in error that a restart
// cannot fix — a bad configuration, a credential problem, a grant nobody
// approved, or a cause muster does not recognise — is never restarted, with
// work waiting and long past every backoff; it is told once. A restartable
// class under the same fixture IS restarted (the positive control).
func TestANonRestartableErrorClassIsHeld(t *testing.T) {
	for _, c := range []officers.ErrorClass{officers.ErrorConfig, officers.ErrorAuth, officers.ErrorCheckoutWait, officers.ErrorRateLimited, officers.ErrorSidecar, officers.ErrorUnknown, "a-future-class"} {
		t.Run(c.String(), func(t *testing.T) {
			r, notices := newRestartRig(t, 1, restartAfter, 3)
			r.hq.AddItem(relID, "release.evaluate", "svc:v1")
			r.errorOut(1, c)
			for i := 0; i < 6; i++ {
				r.check(t)
				r.clk.Advance(time.Hour)
			}
			if starts, stops := r.startsStops(); len(starts) != 0 || len(stops) != 0 {
				t.Fatalf("class %s: starts %v stops %v, want none", c, starts, stops)
			}
			if got := notices.count(func(e officers.AutoRestartEvent) bool {
				return e.Held && e.Class == c && strings.Contains(e.HeldWhy, "not one a restart fixes")
			}); got != 1 {
				t.Fatalf("class %s: held notices = %d, want 1: %+v", c, got, notices.all())
			}
			if notices.count(func(e officers.AutoRestartEvent) bool { return e.Restarted }) != 0 {
				t.Fatalf("class %s: a restart was told: %+v", c, notices.all())
			}
		})
	}
	for _, c := range []officers.ErrorClass{officers.ErrorKickoffStuck, officers.ErrorCrashLoop, officers.ErrorApplyFailed} {
		t.Run("control "+c.String(), func(t *testing.T) {
			r, _ := newRestartRig(t, 1, restartAfter, 3)
			r.hq.AddItem(relID, "release.evaluate", "svc:v1")
			r.errorOut(1, c)
			r.check(t)
			r.clk.Advance(time.Hour)
			r.check(t)
			if starts, _ := r.startsStops(); !slices.Equal(starts, []int{1}) {
				t.Fatalf("positive control: class %s was not restarted (starts %v)", c, starts)
			}
		})
	}
}

// TestAnErroredInstanceWithNoWorkIsNotRestarted: with nothing claimable and
// no claim of its own, a restart has nothing to do and is not made (nor told
// as held).
func TestAnErroredInstanceWithNoWorkIsNotRestarted(t *testing.T) {
	r, notices := newRestartRig(t, 1, restartAfter, 3)
	r.errorOut(1, officers.ErrorKickoffStuck)
	for i := 0; i < 4; i++ {
		if res := r.check(t); len(res.Decision.Restart) != 0 {
			t.Fatalf("no work: Restart = %+v", res.Decision.Restart)
		}
		r.clk.Advance(5 * time.Minute)
	}
	if len(notices.all()) != 0 {
		t.Fatalf("no work: notices %+v, want none", notices.all())
	}
}

// TestAnAutoRestartNeitherDoubleCountsASlotNorAClaim: max_instances 2;
// instance 1 is in error holding a live claim on item A, item B is open and
// instance 2 down. The pass that restarts 1 also starts 2 for B — and
// nothing more: 1 is stopped and started in place (never a third instance),
// A is released exactly once (after 1's stop) and is not ALSO a down
// holder's release, and B is not released. The next pass starts nothing.
func TestAnAutoRestartNeitherDoubleCountsASlotNorAClaim(t *testing.T) {
	r, _ := newRestartRig(t, 2, restartAfter, 3)
	a := r.hq.AddItem(relID, "release.evaluate", "svc:a")
	b := r.hq.AddItem(relID, "release.evaluate", "svc:b")
	r.errorOut(1, officers.ErrorCrashLoop)
	holder := officers.InstanceName(relID, 1)
	r.hq.SetClaim(a, holder, r.clk.Now().Add(time.Hour))

	res := r.check(t)
	if len(res.Decision.Restart) != 0 || !slices.Equal(res.Decision.Start, []int{2}) {
		t.Fatalf("first pass: Restart %+v Start %v, want none and [2]", res.Decision.Restart, res.Decision.Start)
	}
	r.clk.Advance(restartAfter)
	r.hq.SetClaim(a, holder, r.clk.Now().Add(time.Hour)) // still held, by the errored instance's name
	r.hq.ResetRequests()
	res = r.check(t)
	if len(res.Decision.Restart) != 1 || res.Decision.Restart[0].N != 1 {
		t.Fatalf("restart pass: Restart = %+v, want instance 1", res.Decision.Restart)
	}
	if len(res.Decision.Start) != 0 || len(res.Decision.Release) != 0 {
		t.Fatalf("restart pass: Start %v Release %v, want none (2 is up; 1's claim is the restart's)", res.Decision.Start, res.Decision.Release)
	}
	if got := res.Decision.Restart[0].Release; len(got) != 1 || string(got[0].ID) != a {
		t.Fatalf("restart pass: the restart releases %+v, want item %s", got, a)
	}
	starts, stops := r.startsStops()
	if !slices.Equal(starts, []int{2, 1}) || !slices.Equal(stops, []int{1}) {
		t.Fatalf("starts %v stops %v, want [2 1] and [1]", starts, stops)
	}
	if got := r.fleet.upCount(); got != 2 {
		t.Fatalf("%d instances up, want 2 (max_instances)", got)
	}
	releases := map[string]int{}
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/release") {
			releases[q.Path]++
		}
	}
	if len(releases) != 1 {
		t.Fatalf("release requests %v, want exactly one, for item %s", releases, a)
	}
	for path, n := range releases {
		if n != 1 || !strings.Contains(path, "/items/"+a+"/") {
			t.Fatalf("release requests %v, want one for item %s", releases, a)
		}
	}
	if it := r.hq.Item(a); it.Status != "open" {
		t.Fatalf("item %s after the restart: %s, want open (released once)", a, it.Status)
	}
	if it := r.hq.Item(b); it.Status != "open" {
		t.Fatalf("item %s: %s, want untouched", b, it.Status)
	}
	res = r.check(t)
	if len(res.Decision.Start) != 0 || len(res.Decision.Restart) != 0 {
		t.Fatalf("the pass after: Start %v Restart %+v, want none (both instances up)", res.Decision.Start, res.Decision.Restart)
	}
	if got := r.fleet.upCount(); got != 2 {
		t.Fatalf("the pass after: %d up, want 2", got)
	}
}

// memRestartStore is a RestartStore in memory, shared by the reconcilers of a
// test the way the database is shared by muster processes. fail makes every
// call fail.
type memRestartStore struct {
	mu    sync.Mutex
	rows  map[string]map[string]officers.RestartRecord // officer|day -> instance
	fail  bool
	reads int
}

func newMemRestartStore() *memRestartStore {
	return &memRestartStore{rows: map[string]map[string]officers.RestartRecord{}}
}

var errStoreDown = errors.New("restart store unreachable")

func (m *memRestartStore) RestartDay(_ context.Context, officer, day string) (map[string]officers.RestartRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	if m.fail {
		return nil, errStoreDown
	}
	out := map[string]officers.RestartRecord{}
	for k, v := range m.rows[officer+"|"+day] {
		out[k] = v
	}
	return out, nil
}

func (m *memRestartStore) row(officer, day, instance string) officers.RestartRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[officer+"|"+day][instance]
}

func (m *memRestartStore) put(officer, day, instance string, f func(*officers.RestartRecord)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errStoreDown
	}
	k := officer + "|" + day
	if m.rows[k] == nil {
		m.rows[k] = map[string]officers.RestartRecord{}
	}
	rec := m.rows[k][instance]
	f(&rec)
	m.rows[k][instance] = rec
	return nil
}

func (m *memRestartStore) RecordRestart(_ context.Context, officer, day, instance string, at time.Time, errText string) error {
	return m.put(officer, day, instance, func(r *officers.RestartRecord) { r.Restarts++; r.LastRestartAt = at; r.LastError = errText })
}

func (m *memRestartStore) SetRestartHeld(_ context.Context, officer, day, instance string, held bool, errText string) error {
	return m.put(officer, day, instance, func(r *officers.RestartRecord) {
		r.HeldTold = held
		if held {
			r.LastError = errText
		}
	})
}

const restartDay = "2000-01-01" // the rig's clock's UTC day

// TestAFreshLoopHonoursThePersistedCap: two restarts made by one muster
// process are counted by the NEXT process (a rollout) through the store: at
// a cap of 2 it does not restart a third time that UTC day, tells the held
// notice once, and a third process tells it no second time.
func TestAFreshLoopHonoursThePersistedCap(t *testing.T) {
	store := newMemRestartStore()
	r, notices := newRestartRigWith(t, 1, restartAfter, 2, store)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	for i := 0; i < 2; i++ {
		r.errorOut(1, officers.ErrorKickoffStuck)
		r.check(t)
		r.clk.Advance(time.Hour)
		if res := r.check(t); len(res.Decision.Restart) != 1 {
			t.Fatalf("restart %d: %+v", i+1, res.Decision.Restart)
		}
	}
	if got := store.row(relID, restartDay, officers.InstanceName(relID, 1)).Restarts; got != 2 {
		t.Fatalf("store holds %d restarts, want 2", got)
	}
	for proc := 2; proc <= 3; proc++ {
		r.rec = r.restartLoop(t, restartAfter, 2, store, notices) // a fresh process
		r.errorOut(1, officers.ErrorKickoffStuck)
		for i := 0; i < 3; i++ {
			if res := r.check(t); len(res.Decision.Restart) != 0 {
				t.Fatalf("process %d restarted past the persisted cap: %+v", proc, res.Decision.Restart)
			}
			r.clk.Advance(time.Hour)
		}
	}
	if starts, _ := r.startsStops(); len(starts) != 2 {
		t.Fatalf("%d starts over three processes, want 2", len(starts))
	}
	if got := notices.count(func(e officers.AutoRestartEvent) bool { return e.Held }); got != 1 {
		t.Fatalf("held notices over two fresh processes = %d, want 1 (the record says it was told): %+v", got, notices.all())
	}
	if !store.row(relID, restartDay, officers.InstanceName(relID, 1)).HeldTold {
		t.Fatalf("the held notice is not recorded")
	}
}

// TestAFreshLoopHonoursThePersistedBackoff: one restart of the instance by
// one process makes the next process's restart of it wait 2×After in error,
// not After.
func TestAFreshLoopHonoursThePersistedBackoff(t *testing.T) {
	store := newMemRestartStore()
	r, notices := newRestartRigWith(t, 1, restartAfter, 3, store)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.errorOut(1, officers.ErrorCrashLoop)
	r.check(t)
	r.clk.Advance(restartAfter)
	if res := r.check(t); len(res.Decision.Restart) != 1 {
		t.Fatalf("first restart: %+v", res.Decision.Restart)
	}
	r.rec = r.restartLoop(t, restartAfter, 3, store, notices) // a fresh process
	r.errorOut(1, officers.ErrorCrashLoop)
	r.check(t) // first sight of this error by the new process
	r.clk.Advance(2*restartAfter - time.Second)
	if res := r.check(t); len(res.Decision.Restart) != 0 {
		t.Fatalf("the fresh process restarted after %s in error, before the persisted %s backoff", 2*restartAfter-time.Second, 2*restartAfter)
	}
	r.clk.Advance(time.Second)
	res := r.check(t)
	if len(res.Decision.Restart) != 1 || res.Decision.Restart[0].Attempt != 2 {
		t.Fatalf("after %s: Restart = %+v, want attempt 2 (the store's first counted)", 2*restartAfter, res.Decision.Restart)
	}
}

// TestAnUnreadableRestartRecordRestartsNothing: a store that cannot be read
// is not "no restarts today": nothing is restarted or told held until it
// reads again (fail closed), and that is logged once.
func TestAnUnreadableRestartRecordRestartsNothing(t *testing.T) {
	store := newMemRestartStore()
	r, notices := newRestartRigWith(t, 1, restartAfter, 3, store)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.errorOut(1, officers.ErrorKickoffStuck)
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	for i := 0; i < 4; i++ {
		if res := r.check(t); len(res.Decision.Restart) != 0 {
			t.Fatalf("restarted with the record unreadable: %+v", res.Decision.Restart)
		}
		r.clk.Advance(time.Hour)
	}
	if len(notices.all()) != 0 {
		t.Fatalf("told %+v with the record unreadable", notices.all())
	}
	if got := strings.Count(r.logs.String(), "the auto-restart record cannot be read"); got != 1 {
		t.Fatalf("unreadable log lines = %d, want 1:\n%s", got, r.logs.String())
	}
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	if res := r.check(t); len(res.Decision.Restart) != 1 {
		t.Fatalf("the record reads again: Restart = %+v, want one", res.Decision.Restart)
	}
}

// TestARestartAndAHeldRestartKeepTheErrorText: the restart's Stop clears
// the agent row's error, so the restart's log line and record carry it, and
// so does a held restart's.
func TestARestartAndAHeldRestartKeepTheErrorText(t *testing.T) {
	store := newMemRestartStore()
	r, _ := newRestartRigWith(t, 2, restartAfter, 1, store)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.AddItem(relID, "release.evaluate", "svc:v2")
	r.errorOut(1, officers.ErrorKickoffStuck)
	r.errorOut(2, officers.ErrorConfig)
	r.fleet.mu.Lock()
	r.fleet.errText[1] = "kickoff never delivered: the agent dwelt more than 15m0s (fixture 4471)"
	r.fleet.errText[2] = "build spec: the account has no setup-token (fixture 4483)"
	r.fleet.mu.Unlock()
	r.check(t)
	r.clk.Advance(restartAfter)
	r.check(t)
	logs := r.logs.String()
	if !strings.Contains(logs, "AUTO-RESTARTED instance 1") || !strings.Contains(logs, "its error was: kickoff never delivered: the agent dwelt more than 15m0s (fixture 4471)") {
		t.Fatalf("the restart's log line does not carry its error:\n%s", logs)
	}
	if !strings.Contains(logs, "instance 2 is in error (config) for 3m0s with work waiting and is NOT auto-restarted") ||
		!strings.Contains(logs, "its error: build spec: the account has no setup-token (fixture 4483)") {
		t.Fatalf("the held line does not carry its error:\n%s", logs)
	}
	if got := store.row(relID, restartDay, officers.InstanceName(relID, 1)).LastError; !strings.Contains(got, "fixture 4471") {
		t.Fatalf("restart record's last error = %q", got)
	}
	if got := store.row(relID, restartDay, officers.InstanceName(relID, 2)).LastError; !strings.Contains(got, "fixture 4483") {
		t.Fatalf("held record's last error = %q", got)
	}
}
