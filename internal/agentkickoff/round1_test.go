package agentkickoff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// PR #37 review round 1: F1 (resolve before the stamp; a dial failure is
// recorded as never-connected), F4 (card changes are broadcast), and the scrubbed
// failStuck log line.
// ---------------------------------------------------------------------------

// withNotify rebuilds the harness's deliverer with an OnChange that writes into
// the shared transcript, so its ORDER against store writes is observable.
func (h *harness) withNotify(t *testing.T) {
	t.Helper()
	d, err := New(Config{
		Store: h.store, Instances: h.insts, Gateway: h.gw, NamespacePrefix: testPrefix,
		Owner: ownerID, Now: func() time.Time { return fixedNow },
		OnChange: func(name string) { h.log.add("OnChange(" + name + ")") },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.d = d
}

// TestAnUnresolvableAgentIsRetriedNotStamped (F1): finding the agent's address (a
// Kubernetes API read for the k8s driver) sends nothing and costs nothing, so its
// failure must be retried like the other pre-send failures, not land after the stamp
// as a permanent "kickoff failed".
func TestAnUnresolvableAgentIsRetriedNotStamped(t *testing.T) {
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, ownedRow())
	h.gw.resolveErr = errors.New("get deployment: etcdserver: request timed out vx40")
	h.tick(t)
	log := h.tick(t)

	if countPrefix(log, "Resolve(lively-newt)") == 0 {
		t.Fatalf("instrument check FAILED: Resolve never ran, so the failure under test was never "+
			"reached.%s", transcript(log))
	}
	if countPrefix(log, "SetKickedOff(") != 0 || h.gw.callCount() != 0 {
		t.Errorf("an agent whose address could not be resolved was STAMPED (or sent a turn): the "+
			"stamp makes the failure permanent although nothing was sent.%s", transcript(log))
	}
	if n := countPrefix(log, "Resolve(lively-newt)"); n != 2 {
		t.Errorf("Resolve ran %d time(s) over two ticks, want 2 (it is retried).%s", n, transcript(log))
	}
	if n := countPrefix(log, "SetKickoffError(7301,kickoff not sent: could not resolve the agent's gateway: "+
		"get deployment: etcdserver: request timed out vx40)"); n != 2 {
		t.Errorf("the resolve failure was recorded %d time(s) over two ticks, want 2 (one per retry).%s",
			n, transcript(log))
	}
	if r := h.store.row(7301); !agents.KickoffOwed(r) || agents.KickoffFailed(r) {
		t.Errorf("after a resolve failure: KickoffOwed=%t KickoffFailed=%t, want true/false",
			agents.KickoffOwed(r), agents.KickoffFailed(r))
	}
}

// dialError is the error chain a refused connection produces through the real
// transport: agents.streamResponses wraps client.Do's *url.Error with %w.
func dialError() error {
	return fmt.Errorf("request: %w", &url.Error{Op: "Post", URL: "http://192.0.2.44:18789/v1/responses",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}})
}

// TestAFailedTurnIsClassifiedByWhetherAnythingWasSent pins turnFailure's four
// causes and the one of them KickoffResendSafe accepts (the other it accepts,
// KickoffNotAcceptedReason, is written by deliver, not turnFailure). Each error is distinct, and the
// non-dial ones include a *net.OpError whose Op is NOT "dial" (a read on an open
// connection), which is the case a looser check would misclassify.
func TestAFailedTurnIsClassifiedByWhetherAnythingWasSent(t *testing.T) {
	readErr := fmt.Errorf("request: %w", &url.Error{Op: "Post", URL: "http://192.0.2.44:18789/v1/responses",
		Err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel2()
	live := context.Background()

	for _, c := range []struct {
		label        string
		parent, turn context.Context
		err          error
		wantPrefix   string
		wantSafe     bool
	}{
		{"dial", live, live, dialError(), agents.KickoffNeverConnectedReason, true},
		{"dial while shutting down", cancelled, cancelled, dialError(), agents.KickoffNeverConnectedReason, true},
		{"read after the request was written", live, live, readErr, "kickoff turn failed after it was handed", false},
		{"shutdown", cancelled, cancelled, errors.New("post responses: context canceled"), ShutdownCancelledReason, false},
		{"timeout", live, expired, errors.New("post responses: context deadline exceeded"), "kickoff turn exceeded its", false},
	} {
		got := turnFailure(c.parent, c.turn, 15*time.Minute, c.err)
		if !strings.HasPrefix(got, c.wantPrefix) {
			t.Errorf("%s: kickoff_error = %q, want it to open with %q", c.label, got, c.wantPrefix)
		}
		row := agents.Agent{KickedOff: true, KickoffError: got}
		if safe := agents.KickoffResendSafe(row); safe != c.wantSafe {
			t.Errorf("%s: KickoffResendSafe = %t, want %t — the remedy would say %s", c.label, safe,
				c.wantSafe, map[bool]string{true: "re-sending is safe", false: "check first"}[safe])
		}
	}
	// An empty reply is NOT safe: the runtime may have retried and done the work.
	if agents.KickoffResendSafe(agents.Agent{KickedOff: true, KickoffError: EmptyReplyReason}) {
		t.Errorf("an EMPTY reply reads as safe to re-send")
	}
}

// staticResolver is an EndpointResolver that always answers one address.
type staticResolver struct{ ep provision.Endpoint }

func (s staticResolver) Endpoint(context.Context, provision.Ref) (provision.Endpoint, error) {
	return s.ep, nil
}

// TestARealRefusedConnectionIsRecordedAsNeverConnected drives the REAL
// agentgateway.Gateway, so the error chain the classification reads is the one
// net/http actually builds, not one this file constructed.
//
// PAIRED CONTROL: a runtime that accepts the connection, reads the request and then
// hangs up has RECEIVED the turn; it must NOT read as never-connected.
func TestARealRefusedConnectionIsRecordedAsNeverConnected(t *testing.T) {
	// A port that was listening and is now closed: connecting to it is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	hangup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("hijack unsupported")
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer hangup.Close()
	hu, _ := url.Parse(hangup.URL)
	hangupPort, _ := strconv.Atoi(hu.Port())

	for _, c := range []struct {
		label    string
		port     int
		wantSafe bool
	}{
		{"refused", closedPort, true},
		{"received, then hung up", hangupPort, false},
	} {
		gw, err := agentgateway.New(agentgateway.Config{
			Driver:  staticResolver{provision.Endpoint{Scheme: "http", Host: "127.0.0.1", Port: c.port}},
			Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel",
		})
		if err != nil {
			t.Fatalf("gateway: %v", err)
		}
		log := &recorder{}
		store := newFakeStore(log, ownedRow())
		d, err := New(Config{
			Store: store, Gateway: gw, NamespacePrefix: testPrefix, Owner: ownerID,
			Now:       func() time.Time { return fixedNow },
			Instances: &fakeInstances{insts: []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := d.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		d.Wait()
		r := store.row(7301)
		if !agents.KickoffFailed(r) {
			t.Fatalf("%s: instrument check FAILED: the turn did not fail after the stamp (kicked_off=%t "+
				"kickoff_error=%q).%s", c.label, r.KickedOff, r.KickoffError, transcript(log.snapshot()))
		}
		if got := agents.KickoffResendSafe(r); got != c.wantSafe {
			t.Errorf("%s: KickoffResendSafe = %t, want %t; kickoff_error = %q", c.label, got, c.wantSafe, r.KickoffError)
		}
	}
}

// TestEveryCardChangeIsBroadcastAndNothingElseIs (F4): the Agents list re-renders
// only on agents:changed, so a change the deliverer makes in the background must
// call OnChange, AFTER the write it announces — and a pre-send failure, which changes
// nothing a card shows and recurs every tick, must not.
func TestEveryCardChangeIsBroadcastAndNothingElseIs(t *testing.T) {
	ready := []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}

	t.Run("a delivered turn: once, after the stamp", func(t *testing.T) {
		h := newHarness(t, ready, ownedRow())
		h.withNotify(t)
		log := h.tick(t)
		if n := countPrefix(log, "OnChange(lively-newt)"); n != 1 {
			t.Fatalf("OnChange ran %d time(s), want 1.%s", n, transcript(log))
		}
		if !(indexOf(log, "SetKickedOff(7301,true)") < indexOf(log, "OnChange(lively-newt)")) {
			t.Errorf("OnChange ran before the stamp it announces.%s", transcript(log))
		}
	})

	t.Run("a failed turn: again, after the failure is recorded", func(t *testing.T) {
		h := newHarness(t, ready, ownedRow())
		h.withNotify(t)
		h.gw.err = errors.New("unexpected EOF from runtime pq27")
		log := h.tick(t)
		if n := countPrefix(log, "OnChange(lively-newt)"); n != 2 {
			t.Fatalf("OnChange ran %d time(s), want 2 (stamp, failure).%s", n, transcript(log))
		}
		last := -1
		for i, l := range log {
			if l == "OnChange(lively-newt)" {
				last = i
			}
		}
		if !(indexOf(log, "SetKickoffError(7301,kickoff turn failed") < last) {
			t.Errorf("the last OnChange ran before the failure it announces was written.%s", transcript(log))
		}
	})

	t.Run("an empty reply: again, after the failure is recorded", func(t *testing.T) {
		h := newHarness(t, ready, ownedRow())
		h.withNotify(t)
		h.gw.reply = "  "
		log := h.tick(t)
		if n := countPrefix(log, "OnChange(lively-newt)"); n != 2 {
			t.Fatalf("OnChange ran %d time(s), want 2 (stamp, empty reply).%s", n, transcript(log))
		}
	})

	t.Run("a stuck verdict: once, after the write", func(t *testing.T) {
		row := ownedRow()
		row.UpdatedAt = fixedNow.Add(-agents.ProvisioningStuckTimeout - time.Second)
		h := newHarness(t, nil, row)
		h.withNotify(t)
		log := h.tick(t)
		if n := countPrefix(log, "OnChange(lively-newt)"); n != 1 {
			t.Fatalf("OnChange ran %d time(s) for a stuck verdict, want 1.%s", n, transcript(log))
		}
		if !(indexOf(log, "MarkKickoffStuck(7301,") < indexOf(log, "OnChange(lively-newt)")) {
			t.Errorf("OnChange ran before the stuck write.%s", transcript(log))
		}
	})

	t.Run("a refused stuck verdict: not at all", func(t *testing.T) {
		row := ownedRow()
		row.UpdatedAt = fixedNow.Add(-agents.ProvisioningStuckTimeout - time.Second)
		h := newHarness(t, nil, row)
		h.withNotify(t)
		h.store.listSkew = time.Second // the row moved after the list read
		log := h.tick(t)
		if indexOf(log, "MarkKickoffStuck-REFUSED(7301)") < 0 {
			t.Fatalf("instrument check FAILED: the stuck write was not refused.%s", transcript(log))
		}
		if n := countPrefix(log, "OnChange("); n != 0 {
			t.Errorf("OnChange ran for a stuck write that did not apply.%s", transcript(log))
		}
	})

	t.Run("a pre-send failure: not at all", func(t *testing.T) {
		row := ownedRow()
		row.HooksToken = ""
		h := newHarness(t, ready, row)
		h.withNotify(t)
		log := h.tick(t)
		if countPrefix(log, "SetKickoffError(7301,kickoff not sent") != 1 {
			t.Fatalf("instrument check FAILED: no pre-send failure was recorded.%s", transcript(log))
		}
		if n := countPrefix(log, "OnChange("); n != 0 {
			t.Errorf("OnChange ran %d time(s) for a pre-send failure, which changes no card and "+
				"recurs every tick.%s", n, transcript(log))
		}
	})
}

// TestTheStuckLogLineDoesNotCarryTheNote: failStuck's message appends the last
// kickoff_error, which can quote a runtime body that echoed the note. Every log line
// it writes must be scrubbed, as recordError's is.
func TestTheStuckLogLineDoesNotCarryTheNote(t *testing.T) {
	row := ownedRow()
	row.UpdatedAt = fixedNow.Add(-agents.ProvisioningStuckTimeout - time.Second)
	row.KickoffError = "responses HTTP 400: echoed request: " + row.PendingNote + " (end mz12)"
	h := newHarness(t, nil, row)
	var buf bytes.Buffer
	d, err := New(Config{
		Store: h.store, Instances: h.insts, Gateway: h.gw, NamespacePrefix: testPrefix, Owner: ownerID,
		Now: func() time.Time { return fixedNow }, Logger: log.New(&buf, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	d.Wait()
	out := buf.String()
	if !strings.Contains(out, "end mz12") {
		t.Fatalf("instrument check FAILED: the stuck verdict's log line was not written:\n%s", out)
	}
	if strings.Contains(out, row.PendingNote) {
		t.Errorf("failStuck logged the pending note:\n%s", out)
	}
}
