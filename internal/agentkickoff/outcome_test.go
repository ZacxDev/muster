package agentkickoff

import (
	"context"

	"fmt"
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
	if got := strings.Join(rep.snapshot(), " "); got != "started(7301) answered(7301,READY — 412 tests)" {
		t.Errorf("a good first turn reported %q", got)
	}

	h = newHarness(t, inst, ownedRow())
	rep = withReporter(t, h)
	h.gw.err = &agents.StreamError{Message: "FailoverError: Unknown model: openai/Acme v4.1 flash"}
	h.tick(t)
	ev := rep.snapshot()
	if len(ev) != 2 || ev[0] != "started(7301)" || !strings.HasPrefix(ev[1], "failed(7301,first turn failed: ") ||
		!strings.Contains(ev[1], "Unknown model: openai/Acme v4.1 flash") {
		t.Errorf("a failed first turn reported %q; want started then failed with the runtime's words", ev)
	}

	h = newHarness(t, inst, ownedRow())
	rep = withReporter(t, h)
	h.gw.reply = "  "
	h.tick(t)
	if ev := rep.snapshot(); len(ev) != 2 || !strings.HasPrefix(ev[1], "failed(7301,first turn failed: ") {
		t.Errorf("an empty reply reported %q; want failed", ev)
	}

	// Shutdown mid-turn: started, and nothing after.
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
	if ev := rep.snapshot(); len(ev) != 1 || ev[0] != "started(7301)" {
		t.Errorf("a shutdown-cancelled turn reported %q; want only started (not failed)", ev)
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
