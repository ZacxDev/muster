package officers_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

const (
	cadenceSlowID  = "cadence:a-slow"
	cadenceSlowTok = "tok-cadence-a-1c2d" // fixtures: values nobody's
	cadenceChatID  = "cadence:b-chat"
	cadenceChatTok = "tok-cadence-b-3e4f"
)

// slowFleet is a poolFleet whose Instances for officer slow signals the first
// time it is called and, when block is set, then holds until release closes
// or the pass's context ends: one officer's pass stuck on a slow fleet read.
type slowFleet struct {
	*poolFleet
	slow    string
	block   bool
	called  chan struct{}
	once    sync.Once
	release chan struct{}
	// slowCalls counts officer slow's Instances calls: one per pass of it.
	slowCalls atomic.Int32
}

func (f *slowFleet) Instances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	if o.ID == f.slow {
		f.slowCalls.Add(1)
		f.once.Do(func() { close(f.called) })
		if f.block {
			select {
			case <-f.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return f.poolFleet.Instances(ctx, o)
}

// TestADuePassDoesNotWaitBehindAnotherOfficersSlowPass: Run used to pass
// every due officer in turn on its one goroutine, so one officer's slow pass
// (each bounded only by the 2m pass timeout) held every other officer's —
// and a chat item is claimed only at the end of its officer's pass, so a
// member's chat waited minutes with poll_interval 1m. Officer A's fleet read
// blocks; officer B has an up instance and a chat lane; a chat enqueued for B
// once A is stuck must be claimed as B's chat holder within two of B's poll
// intervals — and no second pass of A may start beside its stuck one. The
// "control" case is the same with A's read not blocking:
// green whether passes run serially or not, so a red "slow" case is the
// stuck pass and not the fixture.
func TestADuePassDoesNotWaitBehindAnotherOfficersSlowPass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block bool
	}{{"slow", true}, {"control", false}} {
		t.Run(tc.name, func(t *testing.T) {
			saved := officers.RunTick
			officers.RunTick = time.Millisecond
			t.Cleanup(func() { officers.RunTick = saved })

			hq := hqtest.New(map[string]string{cadenceSlowTok: cadenceSlowID, cadenceChatTok: cadenceChatID})
			t.Cleanup(hq.Close)
			const poll = time.Second
			a := officers.Officer{ID: cadenceSlowID, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: poll, Owner: 1}.WithToken(cadenceSlowTok)
			b := officers.Officer{ID: cadenceChatID, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: poll, Owner: 1}.WithToken(cadenceChatTok)
			if a.Key() >= b.Key() {
				t.Fatalf("fixture: A (%q) must sort before B (%q), so a serial loop reaches A's pass first", a.Key(), b.Key())
			}
			fleet := &slowFleet{poolFleet: &poolFleet{up: map[string]bool{b.InstanceName(1): true}, kickoff: map[string]string{}},
				slow: a.ID, block: tc.block, called: make(chan struct{}), release: make(chan struct{})}
			logs := &syncBuffer{}
			backends := officers.NewBackends(nil)
			runner, err := officers.NewChatRunner(officers.ChatRunnerConfig{Lanes: newLanes(), Backend: backends.Backend, Logger: log.New(logs, "", 0),
				HeartbeatEvery: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry([]officers.Officer{a, b}), HQ: backends, Fleet: fleet,
				Logger: log.New(logs, "", 0), Chats: runner})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			go rec.Run(ctx)
			t.Cleanup(func() {
				close(fleet.release)
				cancel()
				select {
				case <-rec.Done():
				case <-time.After(10 * time.Second):
					t.Error("Run did not return after its context ended")
				}
				runner.Wait()
			})

			select {
			case <-fleet.called:
			case <-time.After(5 * time.Second):
				t.Fatalf("fixture: officer A's pass never read its fleet; log:\n%s", logs)
			}
			hq.AddChat(b.ID, "c-1", "triage")
			_, id := hq.MemberSays("c-1", "member1", "Member One", "are you there?")
			holder := officers.ChatHolder(b.InstanceName(1))
			deadline := time.Now().Add(2 * poll)
			for {
				if claimedAs(hq, id) == holder {
					// One officer never has two passes at once: while A's
					// pass is stuck, no second pass of A has read its fleet.
					if n := fleet.slowCalls.Load(); tc.block && n != 1 {
						t.Fatalf("officer A's fleet was read by %d passes while its first pass was stuck, want 1: a second pass of one officer ran beside its first", n)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("B's chat item %s was not claimed as %s within %s (2 x poll_interval) after A's pass started (A blocking: %v); claims seen: %q; log:\n%s",
						id, holder, 2*poll, tc.block, claimedAs(hq, id), logs)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

// TestRunClosesDoneOnlyAfterItsPassesEnd: passes run on their own goroutines,
// so Run's shutdown must wait for them — Done is what the shutdown drain
// joins on before it waits for re-prompt sends, so no pass may still be
// running once it closes. A's fleet read here ignores its context and ends
// only when released: Done must stay open after ctx ends until it is; the
// control is that it closes once the pass is released. An INVARIANT guard:
// the serial loop held it by construction (its passes ran on Run's own
// goroutine); it reddens when Run's wait for its passes is removed.
func TestRunClosesDoneOnlyAfterItsPassesEnd(t *testing.T) {
	saved := officers.RunTick
	officers.RunTick = time.Millisecond
	t.Cleanup(func() { officers.RunTick = saved })
	hq := hqtest.New(map[string]string{cadenceSlowTok: cadenceSlowID})
	t.Cleanup(hq.Close)
	o := officers.Officer{ID: cadenceSlowID, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(cadenceSlowTok)
	fleet := &stuckFleet{poolFleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}}, called: make(chan struct{}), release: make(chan struct{})}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry([]officers.Officer{o}), HQ: officers.NewBackends(nil), Fleet: fleet,
		Logger: log.New(&syncBuffer{}, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go rec.Run(ctx)
	released := false
	t.Cleanup(func() {
		if !released {
			close(fleet.release)
		}
		cancel()
		<-rec.Done()
	})
	select {
	case <-fleet.called:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture: the pass never read its fleet")
	}
	cancel()
	select {
	case <-rec.Done():
		t.Fatal("Done closed while a pass was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(fleet.release)
	released = true
	select {
	case <-rec.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("control: Done did not close once the pass ended")
	}
}

// stuckFleet is a poolFleet whose Instances signals and then holds until
// release closes, whatever its context says.
type stuckFleet struct {
	*poolFleet
	called  chan struct{}
	once    sync.Once
	release chan struct{}
}

func (f *stuckFleet) Instances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	f.once.Do(func() { close(f.called) })
	<-f.release
	return f.poolFleet.Instances(ctx, o)
}

// countingFleet is a poolFleet that signals each Instances call: one per pass
// of an untiered officer.
type countingFleet struct {
	*poolFleet
	calls chan struct{}
}

func (f *countingFleet) Instances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	f.calls <- struct{}{}
	return f.poolFleet.Instances(ctx, o)
}

// TestALatePassIsLoggedOncePerEpisode: a pass that starts more than the
// officer's poll interval after it was due is logged — once while the lag
// lasts, again only after a pass has started on time. Run's clock is a test
// clock moved between passes: +5m (4m late: logged), +5m (still late: not
// again), +1m (on time: the episode ends), +5m (late again: logged).
func TestALatePassIsLoggedOncePerEpisode(t *testing.T) {
	saved := officers.RunTick
	officers.RunTick = time.Millisecond
	t.Cleanup(func() { officers.RunTick = saved })
	hq := hqtest.New(map[string]string{cadenceSlowTok: cadenceSlowID})
	t.Cleanup(hq.Close)
	o := officers.Officer{ID: cadenceSlowID, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(cadenceSlowTok)
	clk := &clock{t: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
	hq.Now = clk.Now
	fleet := &countingFleet{poolFleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}}, calls: make(chan struct{}, 16)}
	logs := &syncBuffer{}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry([]officers.Officer{o}), HQ: officers.NewBackends(nil), Fleet: fleet,
		Logger: log.New(logs, "", 0), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go rec.Run(ctx)
	t.Cleanup(func() { cancel(); <-rec.Done() })
	// passed waits for the next pass to read the fleet and then to end (the
	// in-flight gauge back at 0), so the clock moves only between passes.
	passed := func(step string) {
		t.Helper()
		select {
		case <-fleet.calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no pass ran; log:\n%s", step, logs)
		}
		for end := time.Now().Add(5 * time.Second); testutil.ToFloat64(metrics.OfficerPassesInFlight) != 0; {
			if time.Now().After(end) {
				t.Fatalf("%s: the pass never ended", step)
			}
			time.Sleep(time.Millisecond)
		}
	}
	const line = "officers: " + cadenceSlowID + ": pass started 240s late (poll_interval 1m0s); logged once until a pass starts on time"
	lateLines := func() int { return strings.Count(logs.String(), "pass started") }
	passed("first pass")
	for _, step := range []struct {
		advance time.Duration
		want    int
	}{{5 * time.Minute, 1}, {5 * time.Minute, 1}, {time.Minute, 1}, {5 * time.Minute, 2}} {
		clk.Advance(step.advance)
		passed("after +" + step.advance.String())
		if got := lateLines(); got != step.want {
			t.Fatalf("after +%s: %d late-pass line(s), want %d; log:\n%s", step.advance, got, step.want, logs)
		}
	}
	if got := strings.Count(logs.String(), line+"\n"); got != 2 {
		t.Fatalf("the late-pass line %q appears %d time(s), want 2; log:\n%s", line, got, logs)
	}
}

// passFleet is a poolFleet that counts each officer's Instances calls (one per
// pass of an untiered officer) and holds each for hold, as a pass's slow fleet
// read would — or, for an officer in block, until release closes or the
// pass's context ends.
type passFleet struct {
	*poolFleet
	hold    time.Duration
	block   map[string]bool
	release chan struct{}
	mu      sync.Mutex
	passes  map[string]int
}

func newPassFleet(hold time.Duration, block map[string]bool) *passFleet {
	return &passFleet{poolFleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}}, hold: hold, block: block,
		release: make(chan struct{}), passes: map[string]int{}}
}

func (f *passFleet) Instances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	f.mu.Lock()
	f.passes[o.ID]++
	f.mu.Unlock()
	var wait <-chan time.Time
	if !f.block[o.ID] {
		wait = time.After(f.hold)
	}
	select {
	case <-wait:
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.poolFleet.Instances(ctx, o)
}

// count is how many passes of officer id have read the fleet.
func (f *passFleet) count(id string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.passes[id] }

// cadenceRoster is n officers cadence:<prefix>00.. polling every poll, sorted
// by key in index order, and the HQ that knows them.
func cadenceRoster(t *testing.T, prefix string, n int, poll time.Duration) ([]officers.Officer, *hqtest.Server) {
	t.Helper()
	toks := map[string]string{}
	for i := range n {
		toks[fmt.Sprintf("tok-cadence-%s%02d-5a6b", prefix, i)] = fmt.Sprintf("cadence:%s%02d", prefix, i)
	}
	hq := hqtest.New(toks)
	t.Cleanup(hq.Close)
	var list []officers.Officer
	for i := range n {
		id := fmt.Sprintf("cadence:%s%02d", prefix, i)
		list = append(list, officers.Officer{ID: id, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: poll, Owner: 1}.
			WithToken(fmt.Sprintf("tok-cadence-%s%02d-5a6b", prefix, i)))
	}
	return list, hq
}

// gateFleet is a poolFleet whose Instances (one call per pass of an untiered
// officer) announces each pass on entered and then holds until the test
// releases it or the pass's context ends: the test decides when each pass
// ends, so slots free one at a time and in a known order.
type gateFleet struct {
	*poolFleet
	entered chan gatePass
}

// gatePass is one pass held on its fleet read.
type gatePass struct {
	id      string
	release chan struct{}
}

func newGateFleet() *gateFleet {
	return &gateFleet{poolFleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}}, entered: make(chan gatePass)}
}

func (f *gateFleet) Instances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	g := gatePass{id: o.ID, release: make(chan struct{})}
	select {
	case f.entered <- g:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.poolFleet.Instances(ctx, o)
}

// cadenceSlots is the loop's pass-slot cap (maxConcurrentPasses).
const cadenceSlots = 4

// stepPasses runs Run over list on a test clock, in steps, and returns how
// many passes each officer had (by index in list). It first waits for every
// slot to hold a pass blocked on its fleet read; then, steps times: between
// (if set) is called, the clock moves step, and the OLDEST running pass is
// released — its slot is the only one free, so the loop starts exactly one
// pass, which the step waits for, calling between again and again while it
// waits (as a user tapping "run now" would). No wall-clock window: the test
// clock stands still within a step, so which officer gets the freed slot is
// decided by the clock and the order of releases — under a correct loop
// whenever within the step between's calls land. A step whose freed slot
// finds no officer due fails the test.
func stepPasses(t *testing.T, list []officers.Officer, hq *hqtest.Server, steps int, step time.Duration, between func(*officers.Reconciler)) []int {
	t.Helper()
	saved := officers.RunTick
	officers.RunTick = time.Millisecond
	t.Cleanup(func() { officers.RunTick = saved })
	clk := &clock{t: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
	hq.Now = clk.Now
	fleet := newGateFleet()
	logs := &syncBuffer{}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry(list), HQ: officers.NewBackends(nil), Fleet: fleet,
		Logger: log.New(logs, "", 0), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go rec.Run(ctx)
	t.Cleanup(func() { cancel(); <-rec.Done() })
	index := map[string]int{}
	for i, o := range list {
		index[o.ID] = i
	}
	got := make([]int, len(list))
	var running []gatePass
	// enter waits for the next pass to start — calling between over and
	// over meanwhile, when set, so it also lands after the released pass
	// has set its next poll time and before the tick that fills the slot.
	enter := func(what string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; {
			select {
			case g := <-fleet.entered:
				got[index[g.id]]++
				running = append(running, g)
				return
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no pass started (passes so far in key order: %v); log:\n%s", what, got, logs)
			}
			if between != nil {
				between(rec)
			}
			runtime.Gosched()
		}
	}
	for i := range cadenceSlots {
		enter(fmt.Sprintf("slot %d of the first passes", i+1))
	}
	for i := range steps {
		if between != nil {
			between(rec)
		}
		clk.Advance(step)
		close(running[0].release)
		running = running[1:]
		enter(fmt.Sprintf("step %d", i+1))
	}
	return got
}

// TestEveryDueOfficerGetsAPassSlot: Run gave each free pass slot to the first
// due officer in KEY order, and a pass longer than poll_interval leaves its
// officer due again the moment it ends — so with more officers due than
// slots, the lowest keys took every freed slot and the highest-key officers
// NEVER passed (and, their pass never starting, never showed a late start
// either). Ten officers polling every 1m on a test clock; each step moves it
// 30s and ends the oldest of the 4 running passes, so every pass lasts 2m
// (4 steps), longer than poll_interval. Over 60 steps every officer must
// pass, and the busiest no more than three times as often as the idlest.
// Key-order assignment gives the four lowest keys every pass and the rest none.
func TestEveryDueOfficerGetsAPassSlot(t *testing.T) {
	const (
		n     = 10
		poll  = time.Minute
		step  = 30 * time.Second
		steps = 60
	)
	list, hq := cadenceRoster(t, "f", n, poll)
	got := stepPasses(t, list, hq, steps, step, nil)
	t.Logf("passes per officer in key order: %v", got)
	lo, hi := slices.Min(got), slices.Max(got)
	if lo == 0 {
		var starved []string
		for i, c := range got {
			if c == 0 {
				starved = append(starved, list[i].ID)
			}
		}
		t.Fatalf("%v had no pass in %d steps of %s (%d officers due every %s, each pass lasting %s): starved of a pass slot; passes per officer in key order: %v",
			starved, steps, step, n, poll, cadenceSlots*step, got)
	}
	if hi > 3*lo {
		t.Fatalf("passes per officer in key order %v: the busiest had %d, more than 3x the idlest's %d — slots are not shared by wait", got, hi, lo)
	}
}

// TestRunNowSpamDoesNotStarveOtherOfficers: Run ordered a zero nextAt — a
// first pass, or one Due asked for — ahead of every other wait, and Due is
// asked on every "run now" tap (a dedupe "already-queued" included) and every
// pause/resume. So a user tapping run-now on 4 officers took all 4 slots at
// every tick, and every other officer had no pass at all (the auditor's run:
// passes [38 37 38 37 0 0]). Six officers polling every 1m on a test clock;
// before every step Due is asked for the four lowest keys — those running a
// pass and those waiting — and again and again until the freed slot is
// filled; the clock moves 15s and the oldest pass ends. Over 60 steps each
// of the two others must pass, and the busiest officer no more than twice
// as often as the idlest.
func TestRunNowSpamDoesNotStarveOtherOfficers(t *testing.T) {
	const (
		n     = 6
		spam  = 4
		poll  = time.Minute
		step  = 15 * time.Second
		steps = 60
	)
	list, hq := cadenceRoster(t, "s", n, poll)
	got := stepPasses(t, list, hq, steps, step, func(rec *officers.Reconciler) {
		for _, o := range list[:spam] {
			rec.Due(o.Key())
		}
	})
	t.Logf("passes per officer in key order (Due asked for the first %d throughout): %v", spam, got)
	for i, o := range list[spam:] {
		if got[spam+i] == 0 {
			t.Fatalf("%s had no pass in %d steps while Due was asked for %d other officers throughout: run-now starved it; passes per officer in key order: %v",
				o.ID, steps, spam, got)
		}
	}
	if lo, hi := slices.Min(got), slices.Max(got); hi > 2*lo {
		t.Fatalf("passes per officer in key order %v: the busiest had %d, more than 2x the idlest's %d — Due lets an officer jump the wait", got, hi, lo)
	}
}

// TestADueAskedForDuringAPassRunsAfterIt: a Due that landed while the
// officer's own pass ran was overwritten when that pass set its next poll
// time, so the run-now was lost until the next poll_interval. One officer
// polling every 1h on a test clock that never moves: Due during its first
// pass must produce exactly one more pass once the first ends — nothing else
// can make it due.
func TestADueAskedForDuringAPassRunsAfterIt(t *testing.T) {
	saved := officers.RunTick
	officers.RunTick = time.Millisecond
	t.Cleanup(func() { officers.RunTick = saved })
	list, hq := cadenceRoster(t, "r", 1, time.Hour)
	clk := &clock{t: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
	hq.Now = clk.Now
	fleet := newGateFleet()
	logs := &syncBuffer{}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry(list), HQ: officers.NewBackends(nil), Fleet: fleet,
		Logger: log.New(logs, "", 0), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go rec.Run(ctx)
	t.Cleanup(func() { cancel(); <-rec.Done() })
	next := func(within time.Duration) (gatePass, bool) {
		select {
		case g := <-fleet.entered:
			return g, true
		case <-time.After(within):
			return gatePass{}, false
		}
	}
	first, ok := next(5 * time.Second)
	if !ok {
		t.Fatalf("fixture: the first pass never started; log:\n%s", logs)
	}
	rec.Due(list[0].Key())
	close(first.release)
	second, ok := next(5 * time.Second)
	if !ok {
		t.Fatalf("Due asked for during %s's pass produced no pass after it (poll_interval 1h, clock still): the run-now was dropped; log:\n%s", list[0].ID, logs)
	}
	close(second.release)
	if g, ok := next(200 * time.Millisecond); ok {
		close(g.release)
		t.Fatalf("a third pass of %s started with no Due and the clock still: one Due during a pass must produce one pass after it, not more", list[0].ID)
	}
}

// TestAnOfficerWaitingForASlotIsLoggedAndMeasured: an officer that is due but
// cannot start (every slot held) produces no pass, so the start-lag histogram
// and the late-pass line — both written when a pass starts — cannot see it.
// Four officers' passes block on their fleet read; a fifth (last by key) is
// due and waits. On a test clock moved 2m (poll_interval 1m) it must be logged
// as waiting, once, and muster_officer_pass_oldest_wait_seconds must read its
// wait; moved another 1m, the line is not repeated and the gauge reads 180.
// Released, its pass starts 3m late inside the same episode, so no
// "pass started … late" line follows, and the gauge drops to 0.
func TestAnOfficerWaitingForASlotIsLoggedAndMeasured(t *testing.T) {
	saved := officers.RunTick
	officers.RunTick = time.Millisecond
	t.Cleanup(func() { officers.RunTick = saved })
	list, hq := cadenceRoster(t, "w", 5, time.Minute)
	clk := &clock{t: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
	hq.Now = clk.Now
	block := map[string]bool{}
	for _, o := range list[:4] {
		block[o.ID] = true
	}
	waiter := list[4]
	fleet := newPassFleet(0, block)
	logs := &syncBuffer{}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry(list), HQ: officers.NewBackends(nil), Fleet: fleet,
		Logger: log.New(logs, "", 0), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go rec.Run(ctx)
	released := false
	t.Cleanup(func() {
		if !released {
			close(fleet.release)
		}
		cancel()
		<-rec.Done()
	})
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("%s; log:\n%s", what, logs)
			}
		}
	}
	waitFor("fixture: the four blocking officers' passes never all read their fleet", func() bool {
		for _, o := range list[:4] {
			if fleet.count(o.ID) != 1 {
				return false
			}
		}
		return true
	})
	if n := fleet.count(waiter.ID); n != 0 {
		t.Fatalf("fixture: %s passed %d time(s) with every slot held, want 0", waiter.ID, n)
	}
	waitLine := func(secs int) string {
		return fmt.Sprintf("officers: %s: due %ds ago and still waiting for a pass slot (4 of 4 running, poll_interval 1m0s); logged once until a pass starts on time\n", waiter.ID, secs)
	}
	clk.Advance(2 * time.Minute)
	waitFor("no waiting-for-a-slot line for "+waiter.ID+" after it was due 2m (poll_interval 1m) with every slot held", func() bool {
		return strings.Contains(logs.String(), waitLine(120))
	})
	gauge := func() float64 { return testutil.ToFloat64(metrics.OfficerPassOldestWait) }
	if g := gauge(); g != 120 {
		t.Fatalf("muster_officer_pass_oldest_wait_seconds = %v after %s waited 2m, want 120", g, waiter.ID)
	}
	clk.Advance(time.Minute)
	waitFor("muster_officer_pass_oldest_wait_seconds never reached 180 after another 1m", func() bool { return gauge() == 180 })
	if c := strings.Count(logs.String(), "still waiting for a pass slot"); c != 1 {
		t.Fatalf("%d waiting-for-a-slot line(s) in one episode, want 1; log:\n%s", c, logs)
	}
	close(fleet.release)
	released = true
	waitFor(waiter.ID+" never passed once the slots were released", func() bool { return fleet.count(waiter.ID) >= 1 })
	if strings.Contains(logs.String(), "officers: "+waiter.ID+": pass started") {
		t.Fatalf("%s's late start was logged again inside the episode its wait already logged; log:\n%s", waiter.ID, logs)
	}
	waitFor("muster_officer_pass_oldest_wait_seconds did not return to 0 once no officer waited", func() bool { return gauge() == 0 })
}

// TestAPassDueAskedForEndsALateEpisode: a pass Due asks for (after a pause
// or a resume) has no next-check time to be late against, and was neither
// measured nor allowed to end a late episode — so after it, the officer's
// next late start went unlogged, though the docs say the next pass that
// starts on time ends the episode. On a test clock: +5m (4m late: logged),
// then Due (starts at once: on time, the episode ends), then +5m (late again:
// logged a second time).
func TestAPassDueAskedForEndsALateEpisode(t *testing.T) {
	saved := officers.RunTick
	officers.RunTick = time.Millisecond
	t.Cleanup(func() { officers.RunTick = saved })
	hq := hqtest.New(map[string]string{cadenceSlowTok: cadenceSlowID})
	t.Cleanup(hq.Close)
	o := officers.Officer{ID: cadenceSlowID, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(cadenceSlowTok)
	clk := &clock{t: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
	hq.Now = clk.Now
	fleet := &countingFleet{poolFleet: &poolFleet{up: map[string]bool{}, kickoff: map[string]string{}}, calls: make(chan struct{}, 16)}
	logs := &syncBuffer{}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry([]officers.Officer{o}), HQ: officers.NewBackends(nil), Fleet: fleet,
		Logger: log.New(logs, "", 0), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go rec.Run(ctx)
	t.Cleanup(func() { cancel(); <-rec.Done() })
	passed := func(step string) {
		t.Helper()
		select {
		case <-fleet.calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no pass ran; log:\n%s", step, logs)
		}
		for end := time.Now().Add(5 * time.Second); testutil.ToFloat64(metrics.OfficerPassesInFlight) != 0; {
			if time.Now().After(end) {
				t.Fatalf("%s: the pass never ended", step)
			}
			time.Sleep(time.Millisecond)
		}
	}
	lateLines := func() int { return strings.Count(logs.String(), "pass started") }
	passed("first pass")
	clk.Advance(5 * time.Minute)
	passed("after +5m")
	if got := lateLines(); got != 1 {
		t.Fatalf("after +5m: %d late-pass line(s), want 1; log:\n%s", got, logs)
	}
	rec.Due(o.Key())
	passed("after Due")
	clk.Advance(5 * time.Minute)
	passed("after Due, +5m")
	if got := lateLines(); got != 2 {
		t.Fatalf("after Due then +5m: %d late-pass line(s), want 2 — the pass Due asked for started on time and must have ended the first episode; log:\n%s", got, logs)
	}
}

// claimedAs is the instance the last claim of item id named ("" if none).
func claimedAs(hq *hqtest.Server, id string) string {
	got := ""
	for _, q := range hq.Requests() {
		if strings.HasSuffix(q.Path, "/items/"+id+"/claim") {
			got, _ = q.Body["instance"].(string)
		}
	}
	return got
}

// syncBuffer is a bytes.Buffer safe for the loop's concurrent log writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
