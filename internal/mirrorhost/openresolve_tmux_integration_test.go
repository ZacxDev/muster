//go:build tmuxit

package mirrorhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// AN OPEN LISTS THE SERVER ONCE, AND OPENS THE RIGHT PANE (openresolve.go),
// on real tmux.
//
// The host connects and reads the whole server once (the refresh loop is
// held off, so the list it last SENT goes stale). Then the server changes
// under it — a window renamed, its active pane split, the window
// moved to another session or closed, its pane respawned with another CLI,
// the CLI starting another conversation (/clear) — and only then is the
// window opened. Each open must show exactly what a fresh read would: the
// window's CURRENT Claude Code conversation, or a refusal. Each transcript
// carries a marker of its own, so the batch says which one was served.

// cacheRig is a perfRig whose windows have marker transcripts.
type cacheRig struct {
	*perfRig
	m *perfMuster
}

// newCacheRig is two sessions, "a" (windows a0 a1) and "b" (b0), each window
// a Claude Code pane whose transcript says marker-<its name>.
func newCacheRig(t *testing.T) *cacheRig {
	t.Helper()
	r := newPerfRigOf(t, map[string]int{"a": 2, "b": 1})
	for i, k := range r.windows {
		writeMarkerSession(t, r.claudeDir, r.panes[k].cli, fmt.Sprintf("00000000-0000-4000-8000-%012d", i), "marker-"+r.names[k])
	}
	// Every runtime a deployed host runs, so an open asks all three (a host
	// without them asks only Claude Code).
	be := r.backend
	be.Hermes = NewHermesSource(t.TempDir(), t.TempDir())
	be.OpenCode = NewOpenCodeSource(context.Background(), nil, "not paired (rig)", log.New(io.Discard, "", 0))
	c := &cacheRig{perfRig: r, m: newPerfMuster(t)}
	c.m.connect(t, be)
	return c
}

// writeMarkerSession is cli's sessions file naming a new transcript that says
// marker.
func writeMarkerSession(t testing.TB, home string, cli int, sessionID, marker string) {
	t.Helper()
	dir := filepath.Join(home, "projects", ProjectSlug("/src/project"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(home, "sessions"), 0o700)
	row, _ := json.Marshal(map[string]any{"type": "user", "promptId": "p", "message": map[string]any{"content": marker}})
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), append(row, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	sf, _ := json.Marshal(map[string]any{"pid": cli, "sessionId": sessionID, "cwd": "/src/project", "status": "idle", "statusUpdatedAt": 946684800000})
	if err := os.WriteFile(filepath.Join(home, "sessions", strconv.Itoa(cli)+".json"), sf, 0o600); err != nil {
		t.Fatal(err)
	}
}

// open watches k and returns the first batch's text, or the refusal.
func (c *cacheRig) open(t *testing.T, id uint32, k windowKey) (batch string, refused string, listAll int) {
	t.Helper()
	mark := len(c.execs(0))
	msg, err := c.m.watchOrRefusal(t, id, k)
	for _, e := range c.execs(mark) {
		if strings.Contains(e, "list-panes -a") {
			listAll++
		}
	}
	if err != "" {
		return "", err, listAll
	}
	b, _ := json.Marshal(msg)
	return string(b), "", listAll
}

func TestTmuxAnOpenListsTheServerOnceAndOpensTheRightPane(t *testing.T) {
	t.Run("unchanged: one server-wide listing for every runtime the open asks", func(t *testing.T) {
		c := newCacheRig(t)
		k := c.byName["a1"]
		batch, refused, listAll := c.open(t, 1, k)
		if refused != "" || !strings.Contains(batch, "marker-a1") {
			t.Fatalf("the open served %q (refused %q), want a1's conversation", batch, refused)
		}
		// 🔴 THE ASSERTION THAT IS RED BEFORE openresolve.go: an open listed the
		// whole server (`list-panes -a`) once per runtime that could claim the
		// window — three on this host (opencode, Hermes, Claude Code).
		if listAll != 1 {
			t.Fatalf("the open ran %d server-wide listing(s), want exactly 1, shared by its runtimes", listAll)
		}
	})
	t.Run("renamed: still the same window", func(t *testing.T) {
		c := newCacheRig(t)
		k := c.byName["a1"]
		c.run("rename-window", "-t", "="+k.session+":"+k.window, "renamed")
		batch, refused, _ := c.open(t, 1, k)
		if refused != "" || !strings.Contains(batch, "marker-a1") {
			t.Fatalf("served %q (refused %q), want a1's conversation", batch, refused)
		}
	})
	t.Run("split: the window's active pane is now another pane", func(t *testing.T) {
		c := newCacheRig(t)
		k := c.byName["a1"]
		// The new pane becomes the window's active pane and runs no CLI: the
		// old pane — still alive, still running its CLI — is NOT what the
		// window shows any more.
		c.run("split-window", "-t", "="+k.session+":"+k.window, "sleep infinity")
		batch, refused, _ := c.open(t, 1, k)
		if refused == "" || strings.Contains(batch, "marker-a1") {
			t.Fatalf("served %q: the old pane's conversation for a window whose active pane runs no CLI", batch)
		}
	})
	t.Run("moved: the window left the session it was listed in", func(t *testing.T) {
		c := newCacheRig(t)
		k := c.byName["a1"]
		c.run("move-window", "-s", "="+k.session+":"+k.window, "-t", "=b:")
		_, refused, _ := c.open(t, 1, k)
		if refused == "" {
			t.Fatal("a window moved to session b was opened as a's")
		}
	})
	t.Run("closed", func(t *testing.T) {
		c := newCacheRig(t)
		k := c.byName["a1"]
		c.run("kill-window", "-t", "="+k.session+":"+k.window)
		_, refused, _ := c.open(t, 1, k)
		if refused == "" {
			t.Fatal("a closed window was opened")
		}
	})
	t.Run("respawned: the pane runs another CLI", func(t *testing.T) {
		c := newCacheRig(t)
		k := c.byName["a1"]
		old := c.panes[k]
		c.run("respawn-pane", "-k", "-t", old.pane, perfCLI)
		root := paneRoot(t, c.perfRig, old.pane)
		if root == old.root {
			t.Fatal("CONTROL: the respawned pane kept its process")
		}
		writeMarkerSession(t, c.claudeDir, childOf(t, root), "00000000-0000-4000-8000-00000000aaaa", "marker-respawned")
		batch, refused, _ := c.open(t, 1, k)
		if refused != "" || !strings.Contains(batch, "marker-respawned") || strings.Contains(batch, "marker-a1") {
			t.Fatalf("served %q (refused %q), want the new CLI's conversation", batch, refused)
		}
	})
	t.Run("cleared: the same CLI started another conversation", func(t *testing.T) {
		c := newCacheRig(t)
		k := c.byName["a1"]
		writeMarkerSession(t, c.claudeDir, c.panes[k].cli, "00000000-0000-4000-8000-00000000bbbb", "marker-cleared")
		batch, refused, _ := c.open(t, 1, k)
		if refused != "" || !strings.Contains(batch, "marker-cleared") || strings.Contains(batch, "marker-a1") {
			t.Fatalf("served %q (refused %q), want the conversation the CLI shows now", batch, refused)
		}
	})
	t.Run("two windows swapped: each id is still its own window", func(t *testing.T) {
		c := newCacheRig(t)
		a0, a1 := c.byName["a0"], c.byName["a1"]
		c.run("swap-window", "-s", "="+a0.session+":"+a0.window, "-t", "="+a1.session+":"+a1.window)
		batch, refused, _ := c.open(t, 1, a0)
		if refused != "" || !strings.Contains(batch, "marker-a0") {
			t.Fatalf("served %q (refused %q), want a0's conversation", batch, refused)
		}
	})
}

// paneRoot is pane's root process now.
func paneRoot(t *testing.T, r *perfRig, pane string) int {
	t.Helper()
	out := strings.TrimSpace(r.run("display-message", "-p", "-t", pane, "#{pane_pid}"))
	pid, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("pane %s: %q", pane, out)
	}
	return pid
}

var _ = mirror.TypeWatch
