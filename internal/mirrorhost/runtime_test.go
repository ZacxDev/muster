package mirrorhost

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RUNTIME SEAM's host half (runtime.go). All fixtures are synthetic: a
// fake proc tree, no tmux server.

// stdin gives fake process pid a stdin (fd 0) that is tty.
func (f *fakeProc) stdin(pid int, tty string) {
	f.t.Helper()
	dir := filepath.Join(f.root, strconv.Itoa(pid), "fd")
	must(f.t, os.MkdirAll(dir, 0o755))
	must(f.t, os.Symlink(tty, filepath.Join(dir, "0")))
}

// TestTheLargestWindowListWithAgentWindowsFitsOneFrame is the opencode twin
// of TestTheLargestWindowListFitsOneFrame: an opencode window carries Runtime
// and an AgentSession of up to 128 bytes in place of a Claude window's flag
// and 64-byte id, so a list of them is the worst case. Every window that does
// not carry what it waits on (Pending is Claude Code's, MaxPendingWindows of
// them) is an opencode window here, every field at its bound. An INVARIANT
// GUARD over the constants, with the same control: twice the count must not
// fit.
func TestTheLargestWindowListWithAgentWindowsFitsOneFrame(t *testing.T) {
	largestAgentList(t, mirror.RuntimeOpenCode)
}

// largestAgentList is the frame-budget check with every non-pending window
// one of runtime rt's.
func largestAgentList(t *testing.T, rt string) {
	worst := func(n int) []WindowInfo {
		out := make([]WindowInfo, 0, n)
		for i := 0; i < n; i++ {
			name := strings.Repeat("<", 128-len(itoa(i))) + itoa(i)
			w := WindowInfo{Session: name, ID: "@" + strings.Repeat("9", 10), Index: 1 << 20, Active: true,
				Pane: "%1", PanePID: 1, Activity: 1791903960, Command: strings.Repeat(">", mirror.MaxCommand),
				Others: otherClaudeAtBound(i)}
			if i < mirror.MaxPendingWindows {
				w.Claude = &ClaudePane{Status: mirror.StatusWaiting, StatusSince: math.MaxInt64, Gen: strings.Repeat("f", 64),
					Pending: &mirror.Pending{Summary: strings.Repeat("<", mirror.MaxPendingSummary), Tool: strings.Repeat("&", mirror.MaxPendingTool),
						Last: strings.Repeat(">", mirror.MaxPendingLast)}}
			} else {
				id := itoa(i) + "h"
				w.Agent = &AgentPane{Runtime: rt, Status: mirror.StatusWaiting, StatusSince: math.MaxInt64,
					Gen: id + strings.Repeat("a", 128-len(id)), Note: strings.Repeat("&", mirror.MaxAgentNote)}
			}
			out = append(out, w)
		}
		return out
	}
	codename := strings.Repeat("&", mirror.MaxLabel-len(":1048576"))
	encode := func(n int) int {
		raw := worst(n)
		codenames := map[string]string{}
		for _, w := range raw {
			codenames[w.Session] = codename
		}
		h := &hostSide{c: &Client{cfg: Config{Backend: &windowsBackend{windows: raw}, Codenames: codenames}, log: log.New(io.Discard, "", 0)},
			ctx: context.Background(), version: mirror.ProtocolVersion}
		l, ok := h.read()
		if !ok {
			t.Fatal("the read failed")
		}
		agents, notes := 0, 0
		for _, w := range l.windows {
			if w.AgentNote != "" {
				notes++
			}
			if w.Runtime == rt {
				if len(w.AgentSession) != 128 || w.Status != mirror.StatusWaiting || w.StatusSince != math.MaxInt64 || w.Label == "" {
					t.Fatalf("instrument check: a worst-case %s field was dropped by normalisation: %+v", rt, w)
				}
				agents++
			}
		}
		// At the bound every window but the pending ones is the runtime's. Past
		// it the list is cut in session-name order, which keeps the longest
		// indexes — all the runtime's windows — so it is a list of nothing else.
		want := mirror.MaxWindows - mirror.MaxPendingWindows
		if n > mirror.MaxWindows {
			want = mirror.MaxWindows
		}
		if notes != mirror.MaxAgentNoteWindows {
			t.Fatalf("instrument check: %d windows carry a note, want the bound %d", notes, mirror.MaxAgentNoteWindows)
		}
		if agents != want || len(l.windows) != min(n, mirror.MaxWindows) {
			t.Fatalf("instrument check: %d %s windows of %d on the wire, want %d", agents, rt, len(l.windows), want)
		}
		m := l.msg(mirror.TypeHello)
		m.Version, m.Host, m.Interactive, m.PanePaste, m.StatusReport, m.OpenCodeAnswer = mirror.ProtocolVersion, strings.Repeat("h", 63), true, true, true, true
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	got := encode(mirror.MaxWindows)
	if got > mirror.MaxFrame {
		t.Fatalf("the largest window list with %s windows encodes to %d bytes, over MaxFrame (%d)", rt, got, mirror.MaxFrame)
	}
	if over := encode(2 * mirror.MaxWindows); over > mirror.MaxFrame {
		t.Fatalf("a host with twice MaxWindows encodes to %d bytes, over MaxFrame (%d): the bound did not hold", over, mirror.MaxFrame)
	}
	if perWindow := got / mirror.MaxWindows; 2*mirror.MaxWindows*perWindow <= mirror.MaxFrame {
		t.Fatalf("instrument check: at %d bytes a window, even twice MaxWindows fits a frame — this test could not see an overrun", perWindow)
	}
	t.Logf("largest list with %s windows: %d windows in %d bytes (%.1f%% of MaxFrame)", rt, mirror.MaxWindows, got, 100*float64(got)/float64(mirror.MaxFrame))
}
