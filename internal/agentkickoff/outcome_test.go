package agentkickoff

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// recReporter records what the deliverer told the outcome reporter.
type recReporter struct {
	mu     sync.Mutex
	events []string
}

func (r *recReporter) add(s string) { r.mu.Lock(); r.events = append(r.events, s); r.mu.Unlock() }
func (r *recReporter) TurnStarted(_ context.Context, a agents.Agent) {
	r.add(fmt.Sprintf("started(%d)", a.ID))
}
func (r *recReporter) TurnEnded(_ context.Context, a agents.Agent) {
	r.add(fmt.Sprintf("ended(%d)", a.ID))
}
func (r *recReporter) TurnActivity(_ context.Context, a agents.Agent) {
	r.add(fmt.Sprintf("activity(%d)", a.ID))
}
func (r *recReporter) TurnAnswered(_ context.Context, a agents.Agent, reply string) {
	r.add(fmt.Sprintf("answered(%d,%s)", a.ID, reply))
}
func (r *recReporter) TurnFailed(_ context.Context, a agents.Agent, reason string) {
	r.add(fmt.Sprintf("failed(%d,%s)", a.ID, reason))
}
func (r *recReporter) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func withReporter(t *testing.T, h *harness) *recReporter {
	t.Helper()
	rep := &recReporter{}
	d, err := New(Config{Store: h.store, Instances: h.insts, Gateway: h.gw, NamespacePrefix: testPrefix, Owner: ownerID,
		Now: func() time.Time { return fixedNow }, Outcome: rep})
	if err != nil {
		t.Fatal(err)
	}
	h.d = d
	return rep
}

// TestTheOutcomeReporterHearsHowAFirstTurnWent: started then answered with the
// reply; started then FAILED with the runtime's own words (the incident: a model
// the runtime does not know, reported mid-stream); an empty reply failed; and a
// turn cut off by THIS process shutting down is NOT reported as failed (the
// runtime may still be running it).
//
// 🔴 RED BEFORE THIS CHANGE: the deliverer told nobody — a failed first turn
// lived only in kickoff_error, and the agent read `running`.
func TestTheOutcomeReporterHearsHowAFirstTurnWent(t *testing.T) {
	inst := []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}

	h := newHarness(t, inst, ownedRow())
	rep := withReporter(t, h)
	h.tick(t)
	if got := strings.Join(rep.snapshot(), " "); got != "started(7301) ended(7301) answered(7301,READY — 412 tests)" {
		t.Errorf("a good first turn reported %q", got)
	}

	h = newHarness(t, inst, ownedRow())
	rep = withReporter(t, h)
	h.gw.err = &agents.StreamError{Message: "FailoverError: Unknown model: openai/Acme v4.1 flash"}
	h.tick(t)
	ev := rep.snapshot()
	if len(ev) != 3 || ev[0] != "started(7301)" || ev[1] != "ended(7301)" || !strings.HasPrefix(ev[2], "failed(7301,first turn failed: ") ||
		!strings.Contains(ev[2], "Unknown model: openai/Acme v4.1 flash") {
		t.Errorf("a failed first turn reported %q; want started, ended, then failed with the runtime's words", ev)
	}

	h = newHarness(t, inst, ownedRow())
	rep = withReporter(t, h)
	h.gw.reply = "  "
	h.tick(t)
	if ev := rep.snapshot(); len(ev) != 3 || ev[1] != "ended(7301)" || !strings.HasPrefix(ev[2], "failed(7301,first turn failed: ") {
		t.Errorf("an empty reply reported %q; want failed", ev)
	}

	// Shutdown mid-turn: started and closed, never failed.
	h = newHarness(t, inst, ownedRow())
	rep = withReporter(t, h)
	h.gw.hold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.gw.hold:
	case <-time.After(5 * time.Second):
		t.Fatal("instrument check FAILED: the turn never reached the gateway")
	}
	cancel()
	h.d.Wait()
	if got := strings.Join(rep.snapshot(), " "); got != "started(7301) ended(7301)" {
		t.Errorf("a shutdown-cancelled turn reported %q; want started and ended (closed, not failed)", got)
	}
}

// TestANeverDeliveredFirstTurnIsReportedFailed: the dwell bound's verdict (the
// row marked `error`: no ready instance ever accepted the first turn) is a
// failure the reporter hears — before this change it changed the status and told
// no one.
func TestANeverDeliveredFirstTurnIsReportedFailed(t *testing.T) {
	row := ownedRow()
	row.UpdatedAt = fixedNow.Add(-agents.ProvisioningStuckTimeout - time.Minute)
	h := newHarness(t, nil, row)
	rep := withReporter(t, h)
	h.tick(t)
	ev := rep.snapshot()
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "failed(7301,kickoff never delivered") {
		t.Fatalf("a never-delivered first turn reported %q; want one failure", ev)
	}
	// Control: the same row inside the bound reports nothing.
	row.UpdatedAt = fixedNow.Add(-time.Minute)
	h = newHarness(t, nil, row)
	rep = withReporter(t, h)
	h.tick(t)
	if ev := rep.snapshot(); len(ev) != 0 {
		t.Errorf("a young owed row reported %q", ev)
	}

}

// TestATurnThatOutlivesItsBudgetIsNotReportedFailed (round 1, 🟡2; round 2,
// 🔴1): the runtime may still be running a turn muster stopped waiting for, so
// it is not reported failed — but it IS closed (ended): left open it read as
// "working" to the watchdog for the life of the process. The api test
// TestAnAbandonedFirstTurnIsClosedAndASilentAgentGoesStuck follows it to stuck.
//
// 🔴 RED AT 4cca02e3: the reporter heard only "started".
func TestATurnThatOutlivesItsBudgetIsNotReportedFailed(t *testing.T) {
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, ownedRow())
	rep := withReporter(t, h)
	h.d.turn = 50 * time.Millisecond
	h.gw.hold = make(chan struct{})
	h.tick(t)
	if got := strings.Join(rep.snapshot(), " "); got != "started(7301) ended(7301)" {
		t.Fatalf("a turn past its budget reported %q; want started and ended (closed, not failed)", got)
	}
}

// TestEveryStartedFirstTurnEndsExactlyOnce: whatever path a delivery takes after
// the stamp — an answer, the runtime's error, an empty reply, the budget, a
// connection never opened, a not_ready past its bound —
// the reporter hears "ended" exactly once, after "started".
func TestEveryStartedFirstTurnEndsExactlyOnce(t *testing.T) {
	inst := []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}
	for _, c := range []struct {
		name string
		set  func(h *harness)
	}{
		{"answered", func(*harness) {}},
		{"runtime error", func(h *harness) { h.gw.err = &agents.StreamError{Message: "boom"} }},
		{"empty reply", func(h *harness) { h.gw.reply = " " }},
		{"budget", func(h *harness) { h.d.turn = 50 * time.Millisecond; h.gw.hold = make(chan struct{}) }},
		{"never connected", func(h *harness) { h.gw.err = &net.OpError{Op: "dial", Err: errors.New("connection refused")} }},
		{"not ready past its bound", func(h *harness) {
			h.d.notReadyWait, h.d.notReadyPoll = 20*time.Millisecond, 5*time.Millisecond
			h.gw.err = &agents.RuntimeError{Status: 503, Type: "not_ready"}
		}},
	} {
		h := newHarness(t, inst, ownedRow())
		rep := withReporter(t, h)
		c.set(h)
		h.tick(t)
		ev := rep.snapshot()
		ended := 0
		for _, e := range ev {
			if e == "ended(7301)" {
				ended++
			}
		}
		if len(ev) == 0 || ev[0] != "started(7301)" || ended != 1 {
			t.Errorf("%s: reported %q; want started then exactly one ended", c.name, ev)
		}
	}
}
