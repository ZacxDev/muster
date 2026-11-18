package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// A SPAWNED WINDOW AT A STARTUP SCREEN (dispatchWindowAlive,
// host_dispatch_reconcile.go): Claude Code writes no sessions file and no
// transcript until it is past its startup screens, so the host lists such a
// window with its id and its tag but NOT as Claude. Its dispatch is tracked by
// the window id the spawn reply recorded, so it stays running (its card says it
// is waiting at a prompt), and ends only when that id is really gone.

// startScreenWindow is host-bravo's listing of dispatch id's window as the host
// sends it while Claude Code sits at a startup screen: the window, its tag and
// its pane's command, no Claude flag, no session, no status.
func startScreenWindow(id int64) mirror.Window {
	return mirror.Window{Session: "agents", ID: "@" + strconv.FormatInt(id, 10), Command: "claude", Dispatch: id}
}

// TestASpawnedWindowAtAStartScreenStaysRunningUntilItReallyCloses: the
// production defect. A spawn whose reply named its window, listed for well past
// the 180 s countdown only as a window carrying its tag (Claude Code waiting at
// a startup screen), stays RUNNING with no missing listings — never "window
// gone" — and its live card reads waiting at a prompt, naming the host. Then
// Claude Code starts (the host lists it as Claude): working. Then the window
// really closes: two listings without it end the row "window gone".
func TestASpawnedWindowAtAStartScreenStaysRunningUntilItReallyCloses(t *testing.T) {
	f := newHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Fix the README typo mirrored on host-bravo.")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "screen-1", true)))
	win := startScreenWindow(id)
	if r := f.row(t, id); r.Window != win.ID || r.Status != plugins.HostDispatchPending {
		t.Fatalf("the spawn reply did not record the window: %+v", r)
	}
	f.setBravoWindows(win)
	// Five listings, each well past the pending countdown (the production
	// timeline: "window gone" ~3 minutes after the spawn).
	for i := 1; i <= 5; i++ {
		at := time.Now().Add(hostPendingCountdown + time.Duration(i)*time.Minute)
		f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{win}, at)
		if r := f.row(t, id); r.Status != plugins.HostDispatchRunning || r.MissingListings != 0 || r.Window != win.ID {
			t.Fatalf("listing %d: a window that is still open lost its dispatch: %+v", i, r)
		}
	}
	c, err := f.srv.hostDispatchCardFromStore(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Waiting at a prompt: its window is open, but Claude Code has not started its first turn — " +
		"a startup screen may be waiting for an answer. Open the window on host-bravo to see it."; c.State != ui.DispatchWaiting || c.Detail != want {
		t.Fatalf("the card at a startup screen: state %q detail %q", c.State, c.Detail)
	}
	// Past the screen: the host now lists it as Claude, busy.
	started := win
	started.Claude, started.ClaudeSession, started.Status = true, "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b", mirror.StatusBusy
	f.setBravoWindows(started)
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{started}, time.Now())
	if c, _ := f.srv.hostDispatchCardFromStore(f.asOwner, id); c.State != ui.DispatchWorking || strings.Contains(c.Detail, "Waiting at a prompt") {
		t.Fatalf("the card once Claude Code started: state %q detail %q", c.State, c.Detail)
	}
	// The window really closes.
	f.setBravoWindows()
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, time.Now())
	if r := f.row(t, id); r.Status != plugins.HostDispatchRunning || r.MissingListings != 1 {
		t.Fatalf("one missing listing: %+v", r)
	}
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", nil, time.Now())
	if r := f.row(t, id); r.Status != plugins.HostDispatchEnded || r.EndedWhy != "window gone" {
		t.Fatalf("the window closed, two listings: %+v", r)
	}
}

// TestAClosedStartScreenWindowEndsEvenBesideAForgedTag: the window id is what
// keeps a row alive, never the tag. The recorded window closes while ANOTHER
// window — not Claude, a different id — carries the dispatch's tag: two
// listings end the row "window gone" (the forger keeps nothing alive).
func TestAClosedStartScreenWindowEndsEvenBesideAForgedTag(t *testing.T) {
	f := newHostFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Fix the README typo mirrored on host-bravo.")
	id := hostDispatchIDOf(t, f.callRaw(f.chief, agentDispatchTool, hostCall("host/host-bravo/claude", "screen-2", true)))
	win := startScreenWindow(id)
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{win}, time.Now())
	if r := f.row(t, id); r.Status != plugins.HostDispatchRunning {
		t.Fatalf("the start-screen window did not make it running: %+v", r)
	}
	forged := mirror.Window{Session: "main", ID: "@907", Command: "zsh", Dispatch: id}
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{forged}, time.Now())
	f.srv.reconcileListing(context.Background(), f.owner, "host-bravo", []mirror.Window{forged}, time.Now())
	if r := f.row(t, id); r.Status != plugins.HostDispatchEnded || r.EndedWhy != "window gone" || r.Window != win.ID {
		t.Fatalf("a forged tag kept a closed window's dispatch alive: %+v", r)
	}
}

// TestDispatchWindowAliveIsTheRecordedWindowWhateverRunsInIt: the liveness pick
// (no database). With a window recorded, THAT id keeps the row alive whether
// or not the host lists it as the row's runtime; another id carrying the tag
// never does. With none recorded, only dispatchWindow's runtime-checked pick
// (adoption still needs the runtime).
func TestDispatchWindowAliveIsTheRecordedWindowWhateverRunsInIt(t *testing.T) {
	screen := mirror.Window{ID: "@5", Command: "claude", Dispatch: 9}
	other := mirror.Window{ID: "@6", Command: "zsh", Dispatch: 9}
	running := mirror.Window{ID: "@5", Claude: true, ClaudeSession: doneAgentSID, Dispatch: 9}
	rec := plugins.HostDispatch{Runtime: mirror.SpawnRuntimeClaude, Window: "@5"}
	none := plugins.HostDispatch{Runtime: mirror.SpawnRuntimeClaude}
	for _, c := range []struct {
		why    string
		row    plugins.HostDispatch
		cands  []mirror.Window
		want   string
		wantOK bool
	}{
		{"recorded, at a startup screen", rec, []mirror.Window{screen}, "@5", true},
		{"recorded, at a startup screen, listed after another", rec, []mirror.Window{other, screen}, "@5", true},
		{"recorded, running", rec, []mirror.Window{running}, "@5", true},
		{"recorded, gone; another id tagged", rec, []mirror.Window{other}, "", false},
		{"recorded, gone", rec, nil, "", false},
		{"none recorded, not the runtime", none, []mirror.Window{screen}, "", false},
		{"none recorded, the runtime", none, []mirror.Window{running}, "@5", true},
	} {
		w, ok := dispatchWindowAlive(c.row, c.cands)
		if ok != c.wantOK || w.ID != c.want {
			t.Errorf("%s: got %q %v, want %q %v", c.why, w.ID, ok, c.want, c.wantOK)
		}
	}
}
