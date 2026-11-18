package mirrorhost

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// REGRESSION TESTS for "an opencode question or permission request reaches
// muster" (docs/design/opencode.md, "Asks"). Each drives the REAL event
// frames a real `opencode serve` sent (opencodetest's fixture) through the
// real client and OpenCodeSource, and reads the window as the listing does.
// They use nothing this change added outside the fake, so they run — and
// fail — against the code before it: there an opencode window whose session
// asks is never waiting and carries no Pending.

// askSource is a fake server holding the fixture's two root sessions, a
// source reading it with its event stream open, and the runtime resolving a
// pane titled title.
func askSource(t *testing.T, title string) (*opencodetest.Server, *OpenCodeSource, func() AgentPane) {
	t.Helper()
	srv, src := ocServer(t, map[string]string{
		opencodetest.FixtureFormSession:       "Fixture session",
		opencodetest.FixturePermissionSession: "Ask session",
	})
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" && srv.Subscribers() > 0 })
	procs, children, root := ocPaneTable(t, "opencode")
	rt := openCodeRuntime{proc: root, src: src, title: titled(title)}
	pane := func() AgentPane { return rt.LocatePanes(ocPane, procs, children)["%1"] }
	// The window's first listing reads its session's pending lists; wait for
	// that read to finish, so what a test then raises arrives by the STREAM.
	sid := pane().Gen
	waitUntil(t, "the session's pending lists read", func() bool {
		n := 0
		for _, r := range srv.Requests() {
			if r.Path == "/api/session/"+sid+"/form" {
				n++
			}
		}
		return sid != "" && n == 1
	})
	time.Sleep(20 * time.Millisecond)
	return srv, src, pane
}

// TestAnOpenCodeQuestionMakesItsWindowWaitOnIt: the question tool's form
// (form.created, its session NESTED under data.form) makes the window
// WAITING, its Pending the question; the form cancelled in the terminal
// (form.cancelled) takes it away again.
func TestAnOpenCodeQuestionMakesItsWindowWaitOnIt(t *testing.T) {
	srv, _, pane := askSource(t, "OC | Fixture session")
	if ap := pane(); ap.Gen != opencodetest.FixtureFormSession || ap.Status == mirror.StatusWaiting || ap.Pending != nil {
		t.Fatalf("instrument check: before any ask: %+v", ap)
	}
	srv.EmitFrame(opencodetest.FixtureFrame("form.created", 1)) // "Proceed?": Yes, No
	waitUntil(t, "the window to wait on the question", func() bool { return pane().Status == mirror.StatusWaiting })
	ap := pane()
	if p := ap.Pending; p == nil || p.Kind != "question" || p.Summary != "Proceed?" || p.Tool != "question" || p.PromptID != opencodetest.FixtureProceedForm {
		t.Fatalf("the question's Pending: %+v", p)
	}
	if ap.StatusSince == 0 || ap.Busy {
		t.Fatalf("a waiting window with no time, or busy: %+v", ap)
	}
	srv.EmitFrame(opencodetest.FixtureFrame("form.cancelled", 0))
	waitUntil(t, "the question to leave the window", func() bool { ap := pane(); return ap.Status != mirror.StatusWaiting && ap.Pending == nil })
}

// TestAnOpenCodePermissionRequestMakesItsWindowWaitOnIt: a permission request
// (permission.asked) makes the window WAITING on the tool and what it acts
// on, offering once and reject; replied in the terminal (permission.replied)
// it is gone.
func TestAnOpenCodePermissionRequestMakesItsWindowWaitOnIt(t *testing.T) {
	srv, _, pane := askSource(t, "OC | Ask session")
	srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 0)) // bash: rm -rf build
	waitUntil(t, "the window to wait on the request", func() bool { return pane().Status == mirror.StatusWaiting })
	if p := pane().Pending; p == nil || p.Kind != "permission" || p.Summary != "bash: rm -rf build" || p.Tool != "bash" ||
		p.PromptID != opencodetest.FixtureBashRequest || p.Choices != "once reject" {
		t.Fatalf("the request's Pending: %+v", p)
	}
	srv.EmitFrame(opencodetest.FixtureFrame("permission.replied", 0))
	waitUntil(t, "the request to leave the window", func() bool { ap := pane(); return ap.Status != mirror.StatusWaiting && ap.Pending == nil })
}

// TestASubagentsPermissionRequestWaitsOnItsRootsWindow: a child session's
// request (a subagent's) waits on the window of the root session the stream
// saw it created under (session.created's parentID).
func TestASubagentsPermissionRequestWaitsOnItsRootsWindow(t *testing.T) {
	srv, _, pane := askSource(t, "OC | Ask session")
	srv.EmitFrame(opencodetest.FixtureFrame("session.created", 2))  // the child, under the Ask session
	srv.EmitFrame(opencodetest.FixtureFrame("permission.asked", 1)) // edit src/main.go, in the child
	waitUntil(t, "the root's window to wait on its subagent's request", func() bool { return pane().Status == mirror.StatusWaiting })
	if p := pane().Pending; p == nil || p.Kind != "permission" || p.Summary != "edit: src/main.go" || p.PromptID != opencodetest.FixtureEditRequest {
		t.Fatalf("the subagent's request: %+v", p)
	}
}
