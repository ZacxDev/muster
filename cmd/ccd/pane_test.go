package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// 🔴 F1 (unit half): input goes to the pane the supervisor names, and with no
// such pane there is no paste at all — not_ready, never a fallback.
func TestInputGoesOnlyToTheSupervisorsPane(t *testing.T) {
	srv, cli := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	ts := gatewayServer(t, srv)
	if code, body := postResponses(t, ts.URL, pinnedDerivation, "list the workspace"); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if len(cli.panes) == 0 {
		t.Fatal("control: nothing was pasted at all")
	}
	for _, p := range cli.panes {
		if p != scriptedPane {
			t.Fatalf("input addressed %q, want only the CLI's pane %q (all: %q)", p, scriptedPane, cli.panes)
		}
	}

	cases := []struct {
		name string
		src  func() (paneRef, bool)
	}{
		{"no pane source (unsupervised)", nil},
		{"supervisor has no running CLI", func() (paneRef, bool) { return scriptedPane, false }},
	}
	for _, c := range cases {
		srv.inputPane = c.src
		before := len(cli.pasted)
		code, body := postResponses(t, ts.URL, pinnedDerivation, "hi")
		if code != http.StatusServiceUnavailable || errType(t, body) != failNotReady {
			t.Fatalf("%s: %d %s, want 503 not_ready", c.name, code, body)
		}
		if len(cli.pasted) != before {
			t.Fatalf("%s: pasted %q", c.name, cli.pasted[before:])
		}
	}
}

// deadPaneCLI is a terminal whose pane died after the supervisor last looked.
type deadPaneCLI struct{ *scriptedCLI }

func (deadPaneCLI) Paste(context.Context, paneRef, string) error {
	return fmt.Errorf("%w: pane %%7 reports pane_dead=\"1\"", errPaneNotLive)
}

// The terminal's own errPaneNotLive is not_ready, not a generic terminal failure.
func TestAPaneThatDiedSinceTheLastPollIsNotReady(t *testing.T) {
	srv, cli := newScripted(t, cliScript{})
	srv.term = deadPaneCLI{cli}
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "hi")
	if code != http.StatusServiceUnavailable || errType(t, body) != failNotReady {
		t.Fatalf("%d %s, want 503 not_ready", code, body)
	}
}

// The supervisor is the production pane source: it names its pane only while the
// CLI in it is running.
func TestTheSupervisorNamesItsPaneOnlyWhileTheCLIRuns(t *testing.T) {
	sup := newSupervisor(nil, "claude", t.TempDir(), "/w")
	ref := paneRef{Server: "9 9", Pane: "%3"}
	if p, ok := sup.inputPane(); ok {
		t.Fatalf("before any start: %v", p)
	}
	sup.started(ref, modeFresh)
	if p, ok := sup.inputPane(); !ok || p != ref {
		t.Fatalf("running: %v %v", p, ok)
	}
	sup.recordExit(paneExit{Dead: true, Known: true, Status: 1})
	if p, ok := sup.inputPane(); ok {
		t.Fatalf("restarting: %v", p)
	}
	sup.started(ref, modeContinue)
	for i := 0; i < sup.crashExits; i++ {
		sup.recordExit(paneExit{Dead: true, Known: true, Status: 1})
	}
	if p, ok := sup.inputPane(); ok {
		t.Fatalf("crash_loop: %v", p)
	}
	sup.started(ref, modeContinue)
	sup.lose(errTerminalLost)
	if p, ok := sup.inputPane(); ok {
		t.Fatalf("terminal_lost: %v", p)
	}
}

// tmuxTerminal refuses a non-pane-id target before running tmux at all: a session
// name is never an input target. bin is a path that cannot exec, so reaching tmux
// would be a different error.
func TestInputRefusesATargetThatIsNotAPaneID(t *testing.T) {
	term := tmuxTerminal{bin: "/nonexistent/tmux-must-not-run", target: "cc"}
	for _, target := range []paneRef{{Server: "1 1", Pane: "cc"}, {}, {Server: "1 1", Pane: "cc:0.0"},
		{Pane: "%1"}} { // the last: a pane id with no server identity
		if err := term.Paste(context.Background(), target, "x"); err == nil || !errors.Is(err, errPaneNotLive) ||
			strings.Contains(err.Error(), "tmux-must-not-run") {
			t.Fatalf("Paste(%v): %v, want errPaneNotLive without running tmux", target, err)
		}
		if err := term.Enter(context.Background(), target); err == nil || !errors.Is(err, errPaneNotLive) ||
			strings.Contains(err.Error(), "tmux-must-not-run") {
			t.Fatalf("Enter(%v): %v, want errPaneNotLive without running tmux", target, err)
		}
	}
}

// 🔴 F3: parsePaneExit through the REAL Exited, with tmux replaced by the test
// binary printing a canned display-message line (CCD_TEST_AS=tmuxprint).
func TestExitedReadsDeadStatusAndSignalAndKnowsWhenItDoesNotKnow(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const me = "4242 1791600000 %1 " // ref's server identity and pane id, as state reads them
	cases := []struct {
		out  string
		want paneExit
		err  bool
		lost bool // the error must be errTerminalLost
	}{
		{me + "0  \n", paneExit{}, false, false},
		{me + "1  \n", paneExit{Dead: true}, false, false},                                                        // dead, not yet reaped
		{me + "1 3 \n", paneExit{Dead: true, Known: true, Status: 3}, false, false},                               // exit 3
		{me + "1 0 \n", paneExit{Dead: true, Known: true, Status: 0}, false, false},                               // clean exit
		{me + "1  9\n", paneExit{Dead: true, Known: true, Status: 137, Signal: 9, SignalText: "9"}, false, false}, // SIGKILL
		{me + "1  15\n", paneExit{Dead: true, Known: true, Status: 143, Signal: 15, SignalText: "15"}, false, false},
		// A tmux built with sys_signame (BSD, macOS) prints the name: known, number not.
		{me + "1  KILL\n", paneExit{Dead: true, Known: true, Status: exitUnknown, SignalText: "KILL"}, false, false},
		{me + "1 \n", paneExit{}, true, false}, // the OLD two-field format: refused, not read as status 0
		{"garbage\n", paneExit{}, true, false},
		// 🔴 R2-F1: the same pane id on ANOTHER server (a different pid, or the same
		// pid with a different start time) is not ref's pane, live or not; and an id
		// that names no pane prints empty pane fields with exit 0 (measured on 3.3a
		// and 3.7c), which is not ref's pane either.
		{"5151 1791600000 %1 0  \n", paneExit{}, true, true},
		{"4242 1791600999 %1 0  \n", paneExit{}, true, true},
		{"4242 1791600000    \n", paneExit{}, true, true},
		{"4242 1791600000 %2 0  \n", paneExit{}, true, true},
	}
	t.Setenv("CCD_TEST_AS", "tmuxprint")
	t.Setenv("GORACE", "atexit_sleep_ms=0") // a -race child otherwise sleeps 1s at exit
	ref := paneRef{Server: "4242 1791600000", Pane: "%1"}
	for _, c := range cases {
		t.Setenv("CCD_TEST_TMUX_OUT", c.out)
		got, err := tmuxTerminal{bin: exe}.Exited(context.Background(), ref)
		if (err != nil) != c.err || got != c.want || errors.Is(err, errTerminalLost) != c.lost {
			t.Errorf("%q -> %+v, %v; want %+v (error %v, lost %v)", c.out, got, err, c.want, c.err, c.lost)
		}
	}
}
