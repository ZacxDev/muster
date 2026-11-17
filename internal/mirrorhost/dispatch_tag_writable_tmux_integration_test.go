//go:build tmuxit

package mirrorhost

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTmuxASpawnedWindowCanClearItsOwnDispatchTag pins the PREMISE the
// server's owner-authority rule is built on (internal/api owner_session.go,
// spawnedByRecord): a window's @muster-dispatch tag is the window's OWN
// option, so a process running in a spawned window can clear it with plain
// tmux, and the host's listing (Tmux.Windows) and dispatchTagOf then both read
// 0. That is why the server decides "a session muster started for the chief"
// from its own host_dispatches record, never from the tag.
//
// ⚠ An INVARIANT guard, not regression coverage: it was green before the
// change too. Should tmux ever make the option unwritable from inside the
// window, this fails, and the premise above is worth re-reading.
//
// Against a REAL tmux server on a private -L socket the test starts and
// kills. The control: an identically tagged window whose process does nothing
// keeps its tag, so the read can see a tag at all.
func TestTmuxASpawnedWindowCanClearItsOwnDispatchTag(t *testing.T) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is not on PATH (this suite needs it)")
	}
	sock := fmt.Sprintf("tagwritable-%d", os.Getpid())
	tm := Tmux{Bin: bin, Socket: sock}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		out, err := tm.run(ctx, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(out)
	}
	run("new-session", "-d", "-s", mirror.AgentSpawnSession, "-n", "keep", "sleep 60")
	t.Cleanup(func() { _, _ = tm.run(context.Background(), "kill-server") }) // only this test's -L server
	keep := run("display-message", "-p", "-t", "="+mirror.AgentSpawnSession+":keep", "#{window_id}")
	run("set-option", "-w", "-t", keep, mirror.DispatchOption, "42")
	// The agent: waits until it is tagged, then clears its OWN window's tag.
	agent := run("new-window", "-d", "-P", "-F", "#{window_id}", "-t", "="+mirror.AgentSpawnSession, "-n", "agent",
		"sh -c 'while [ -z \"$(tmux show-options -w -t \"$TMUX_PANE\" -qv "+mirror.DispatchOption+")\" ]; do sleep 0.05; done; tmux set-option -w -t \"$TMUX_PANE\" -u "+mirror.DispatchOption+"; exec sleep 60'")
	run("set-option", "-w", "-t", agent, mirror.DispatchOption, "42")

	var got map[string]int64
	for i := 0; i < 60; i++ {
		ws, err := tm.Windows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = map[string]int64{}
		for _, w := range ws {
			got[w.ID] = w.Dispatch
		}
		if got[agent] == 0 && i > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	ownAgent, _ := tm.dispatchTagOf(ctx, agent)
	ownKeep, _ := tm.dispatchTagOf(ctx, keep)
	if got[keep] != 42 || ownKeep != 42 {
		t.Fatalf("control: the untouched window lost its tag (the read is broken): listing %v, own %d", got, ownKeep)
	}
	if got[agent] != 0 || ownAgent != 0 {
		t.Fatalf("the agent's window still reads its tag (listing %v, own %d): the premise changed", got, ownAgent)
	}
}
