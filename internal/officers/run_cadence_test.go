package officers_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
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
}

func (f *slowFleet) Instances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	if o.ID == f.slow {
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
// intervals. The "control" case is the same with A's read not blocking:
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
