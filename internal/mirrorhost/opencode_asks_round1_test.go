package mirrorhost

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// sampler polls pane every few ms until stopped, counting the samples that
// are not waiting on an ask: a listing's gap.
type sampler struct {
	mu        sync.Mutex
	n, gaps   int
	stop, end chan struct{}
}

func sample(pane func() AgentPane) *sampler {
	s := &sampler{stop: make(chan struct{}), end: make(chan struct{})}
	go func() {
		defer close(s.end)
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			ap := pane()
			s.mu.Lock()
			s.n++
			if ap.Status != mirror.StatusWaiting || ap.Pending == nil {
				s.gaps++
			}
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *sampler) done() (n, gaps int) {
	close(s.stop)
	<-s.end
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n, s.gaps
}

// permissionReads counts the host's reads of session sid's pending requests.
func permissionReads(srv *opencodetest.Server, sid string) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Path == "/api/session/"+sid+"/permission" {
			n++
		}
	}
	return n
}

// TestADroppedStreamHoldsItsAskUntilTheReReadSettlesIt (🟡-1): the stream
// drops while the window waits on a request. Still pending after the
// reconnect: the listing never once stops waiting — not during the drop, not
// between the reconnect and its re-read — so muster withdraws nothing. Settled
// DURING the gap (no event can say so): the window keeps waiting, held, until
// the reconnect's re-read lands, and then stops.
func TestADroppedStreamHoldsItsAskUntilTheReReadSettlesIt(t *testing.T) {
	sid := opencodetest.FixturePermissionSession
	srv, _, pane := askSource(t, "OC | Ask session")
	srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0))
	waitUntil(t, "waiting", func() bool { return pane().Status == mirror.StatusWaiting })

	// Still pending across the drop.
	reads := permissionReads(srv, sid)
	smp := sample(pane)
	srv.DropStreams()
	waitUntil(t, "the drop", func() bool { return srv.Subscribers() == 0 })
	waitUntil(t, "the reconnect and its re-read", func() bool { return srv.Subscribers() > 0 && permissionReads(srv, sid) > reads })
	time.Sleep(50 * time.Millisecond)
	if n, gaps := smp.done(); n < 20 || gaps != 0 {
		t.Fatalf("across a drop with the request still pending: %d of %d listings were not waiting (want 0; at least 20 samples)", gaps, n)
	}

	// Settled while the stream was down: held until the re-read, then gone.
	reads = permissionReads(srv, sid)
	srv.DropStreams()
	waitUntil(t, "the drop", func() bool { return srv.Subscribers() == 0 })
	srv.EmitFrame(opencodetest.FixtureFrame("permission.replied", 0)) // nobody hears it
	if ap := pane(); ap.Status != mirror.StatusWaiting || ap.Pending == nil {
		t.Fatalf("during the gap the ask was not held: %+v", ap)
	}
	waitUntil(t, "the re-read to drop the settled request", func() bool {
		ap := pane()
		return permissionReads(srv, sid) > reads && ap.Status != mirror.StatusWaiting && ap.Pending == nil
	})
}

// TestAnInterruptedTurnTakesItsPermissionRequestWithIt (🟡-2): the REAL
// frames of a turn interrupted while it waited on a permission request —
// permission.asked, then session.execution.interrupted and nothing that names
// the request (upstream drops it silently: permission.ts:251-262). The window
// stops waiting and its status is idle. Another session's request is not
// touched by this session's turn ending; and an interrupted question is
// cancelled by its own frame (form.cancelled).
func TestAnInterruptedTurnTakesItsPermissionRequestWithIt(t *testing.T) {
	srv, src := ocServer(t, map[string]string{opencodetest.AbortPermissionSession: "Abort session", opencodetest.FixturePermissionSession: "Ask session"})
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" && srv.Subscribers() > 0 })
	procs, children, root := ocPaneTable(t, "opencode")
	pane := func(title string) AgentPane {
		return openCodeRuntime{proc: root, src: src, title: titled(title)}.LocatePanes(ocPane, procs, children)["%1"]
	}
	srv.EmitFrame(opencodetest.AbortFrame("session.execution.started", 0))
	srv.EmitFrame(opencodetest.AbortFrame("permission.asked", 0))
	srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0)) // another session's
	waitUntil(t, "waiting", func() bool { return pane("OC | Abort session").Status == mirror.StatusWaiting })
	if p := pane("OC | Abort session").Pending; p == nil || p.PromptID != opencodetest.AbortShellRequest || !strings.HasPrefix(p.Summary, "shell: ls -la .") {
		t.Fatalf("the real request: %+v", p)
	}
	srv.EmitFrame(opencodetest.AbortFrame("session.execution.interrupted", 0))
	waitUntil(t, "the interrupted turn to leave the window", func() bool {
		ap := pane("OC | Abort session")
		return ap.Status == mirror.StatusIdle && ap.Pending == nil
	})
	if p := pane("OC | Ask session").Pending; p == nil || p.PromptID != opencodetest.FixtureBashRequest {
		t.Fatalf("another session's request went with this turn: %+v", p)
	}
}

// TestTheTurnsLifecycleIsTheSessionsStatus: the v2 server publishes no
// session.status (measured); the REAL lifecycle frames move the status —
// execution.started busy, execution.interrupted idle — with its time.
func TestTheTurnsLifecycleIsTheSessionsStatus(t *testing.T) {
	sid := opencodetest.AbortQuestionSession
	srv, src := ocServer(t, map[string]string{sid: "Question session"})
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" && srv.Subscribers() > 0 })
	srv.EmitFrame(opencodetest.AbortFrame("session.execution.started", 1))
	waitUntil(t, "busy", func() bool { st, _ := src.status(sid); return st == mirror.StatusBusy })
	_, busyAt := src.status(sid)
	srv.EmitFrame(opencodetest.AbortFrame("form.created", 0))
	srv.EmitFrame(opencodetest.AbortFrame("form.cancelled", 0))
	srv.EmitFrame(opencodetest.AbortFrame("session.execution.interrupted", 1))
	waitUntil(t, "idle", func() bool { st, at := src.status(sid); return st == mirror.StatusIdle && at >= busyAt })
}

// A 400 from opencode (the field cannot take the value) is said as what it
// is — opencode refused the answer, still pending — never as "answered in the
// terminal".
func TestARefusedAnswerIsNotSaidToBeGone(t *testing.T) {
	srv, src, _ := askSource(t, "OC | Fixture session")
	srv.EmitFrame(opencodetest.FixtureFrame("form.created", 1))
	d := runOCHost(t, src, "OC | Fixture session", true)
	waitUntil(t, "the question on the wire", func() bool { p := lastPending(d); return p != nil && p.Options == "Yes\nNo" })
	srv.RefuseReplies(400)
	d.send(t, ocAnswer(1, opencodetest.FixtureProceedForm, "Yes"))
	if m := answeredOf(t, d, 1); m.Refusal != mirror.AskRefusalOpenCodeRefused {
		t.Fatalf("a refused answer: %+v", m)
	}
	if lastPending(d) == nil {
		t.Fatal("a refused answer took the question off the window")
	}
}
