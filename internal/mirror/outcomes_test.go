package mirror_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// An idle opencode window the chief started (dispatch 41), and the windows an
// outcome must never ride: a window nobody dispatched, a Claude dispatch
// window, a BUSY opencode dispatch window, one with no session. Every id,
// session and mark is distinct.
func outcomeWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "agents", ID: "@41", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_aaa41", Dispatch: 41, Status: mirror.StatusIdle},
		{Session: "main", ID: "@5", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_bbb05", Status: mirror.StatusIdle},
		{Session: "agents", ID: "@42", Claude: true, ClaudeSession: "c0ffee42-0000-4000-8000-000000000042", Dispatch: 42, Status: mirror.StatusIdle},
		{Session: "agents", ID: "@43", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_ccc43", Dispatch: 43, Status: mirror.StatusBusy},
		{Session: "agents", ID: "@44", Runtime: mirror.RuntimeOpenCode, Dispatch: 44, Status: mirror.StatusIdle},
	}
}

// TestNormalizeOutcomesKeepsOnlyIdleOpenCodeDispatchWindows: of a map naming
// every window above and one not listed, only the idle opencode window the chief
// started keeps its outcome; a mark the wire does not carry drops it; its lines
// are its last three non-empty ones, control characters made spaces, each cut
// to MaxOutcomeLine bytes.
func TestNormalizeOutcomesKeepsOnlyIdleOpenCodeDispatchWindows(t *testing.T) {
	ws := mirror.NormalizeWindows(outcomeWindows(), 0)
	in := map[string]mirror.WindowOutcome{}
	for _, id := range []string{"@41", "@5", "@42", "@43", "@44", "@99"} {
		in[id] = mirror.WindowOutcome{Mark: "msg_" + strings.TrimPrefix(id, "@"), Lines: []string{"x"}}
	}
	long := strings.Repeat("é", mirror.MaxOutcomeLine) // 2 bytes each: cut on a rune boundary
	in["@41"] = mirror.WindowOutcome{Mark: "msg_m41", Lines: []string{"first", "", "second\x1b[31m", "  third  ", long}}
	got := mirror.NormalizeOutcomes(in, ws)
	if len(got) != 1 {
		t.Fatalf("kept %v, want only @41's", got)
	}
	o := got["@41"]
	if o.Mark != "msg_m41" {
		t.Fatalf("mark %q", o.Mark)
	}
	want := []string{"second [31m", "third", strings.Repeat("é", mirror.MaxOutcomeLine/2)}
	if fmt.Sprint(o.Lines) != fmt.Sprint(want) {
		t.Fatalf("lines %q, want %q", o.Lines, want)
	}
	// A mark the wire does not carry.
	in["@41"] = mirror.WindowOutcome{Mark: "msg with spaces", Lines: []string{"x"}}
	if got := mirror.NormalizeOutcomes(in, ws); len(got) != 0 {
		t.Fatalf("a bad mark was kept: %v", got)
	}
}

// TestTheLargestOutcomesMessageFitsOneFrame: MaxWindows idle opencode dispatch
// windows each with an outcome at every bound, every byte one JSON escapes
// six-fold: the list-wide bound keeps MaxOutcomeWindows and the message fits a
// frame.
func TestTheLargestOutcomesMessageFitsOneFrame(t *testing.T) {
	var windows []mirror.Window
	in := map[string]mirror.WindowOutcome{}
	for i := 0; i < mirror.MaxWindows; i++ {
		id := "@" + fmt.Sprint(9_999_999_999-i)
		windows = append(windows, mirror.Window{Session: "s", ID: id, Index: i, Runtime: mirror.RuntimeOpenCode,
			AgentSession: "ses_" + strings.Repeat("a", 124), Dispatch: int64(i + 1), Status: mirror.StatusIdle})
		line := strings.Repeat("<", mirror.MaxOutcomeLine)
		in[id] = mirror.WindowOutcome{Mark: "m" + strings.Repeat("b", 127), Lines: []string{line, line, line}}
	}
	nw := mirror.NormalizeWindows(windows, 0)
	kept := mirror.NormalizeOutcomes(in, nw)
	if len(kept) != mirror.MaxOutcomeWindows {
		t.Fatalf("instrument check: kept %d, want the bound %d", len(kept), mirror.MaxOutcomeWindows)
	}
	for _, o := range kept {
		if len(o.Lines) != mirror.MaxOutcomeLines || len(o.Lines[0]) != mirror.MaxOutcomeLine || len(o.Mark) != 128 {
			t.Fatalf("instrument check: an outcome was cut, so this is not the worst case: %d lines", len(o.Lines))
		}
	}
	b, err := json.Marshal(mirror.Msg{Type: mirror.TypeOutcomes, Outcomes: kept})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > mirror.MaxFrame {
		t.Fatalf("the largest outcomes message is %d bytes, over MaxFrame", len(b))
	}
	t.Logf("largest outcomes message: %d entries in %d bytes (%.1f%% of MaxFrame)", len(kept), len(b), 100*float64(len(b))/float64(mirror.MaxFrame))
}

// TestAnOutcomesMessageReachesTheHookOnlyForItsIdleDispatchWindow: the hub
// hands its outcomes hook the host's windows and the outcomes restricted to
// them — after the hello (none yet), after an outcomes message (the idle
// dispatch window's only), and after a listing in which that window went BUSY
// (none: an outcome rides an idle window only) — each with a larger snapshot
// number. An older muster has no such case (the message is dropped).
func TestAnOutcomesMessageReachesTheHookOnlyForItsIdleDispatchWindow(t *testing.T) {
	r := newRig(t)
	type call struct {
		owner    int64
		host     string
		seq      uint64
		outcomes map[string]mirror.WindowOutcome
	}
	var mu sync.Mutex
	var calls []call
	r.svc.SetOutcomesNotify(func(owner int64, host string, seq uint64, _ []mirror.Window, oc map[string]mirror.WindowOutcome) {
		mu.Lock()
		calls = append(calls, call{owner, host, seq, oc})
		mu.Unlock()
	})
	snapshot := func() []call { mu.Lock(); defer mu.Unlock(); return append([]call(nil), calls...) }
	tok, _ := r.store.mint(t, 7, "laptop")
	h := hello("laptop", false)
	h.StatusReport = true
	h.Windows = outcomeWindows()
	host, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the hello's snapshot", func() bool { return len(snapshot()) == 1 })
	if c := snapshot()[0]; c.owner != 7 || c.host != "laptop" || len(c.outcomes) != 0 {
		t.Fatalf("the hello's snapshot: %+v", c)
	}
	send := func(m mirror.Msg) {
		b, _ := json.Marshal(m)
		if err := host.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	send(mirror.Msg{Type: mirror.TypeOutcomes, Outcomes: map[string]mirror.WindowOutcome{
		"@41": {Mark: "msg_m41", Lines: []string{"MUSTER-DONE: shipped it"}},
		"@43": {Mark: "msg_m43", Lines: []string{"busy, must not ride"}},
		"@5":  {Mark: "msg_m05", Lines: []string{"nobody dispatched it"}},
	}})
	eventually(t, "the outcomes snapshot", func() bool { return len(snapshot()) == 2 })
	c := snapshot()[1]
	if len(c.outcomes) != 1 || c.outcomes["@41"].Mark != "msg_m41" || c.seq <= snapshot()[0].seq {
		t.Fatalf("the outcomes snapshot: %+v", c)
	}
	// The window goes busy: the next listing's snapshot carries no outcome.
	ws := outcomeWindows()
	ws[0].Status = mirror.StatusBusy
	send(mirror.Msg{Type: mirror.TypeSessions, Windows: ws})
	eventually(t, "the listing's snapshot", func() bool { return len(snapshot()) >= 3 })
	time.Sleep(20 * time.Millisecond)
	last := snapshot()[len(snapshot())-1]
	if len(last.outcomes) != 0 || last.seq <= c.seq {
		t.Fatalf("after the window went busy: %+v", last)
	}
}
