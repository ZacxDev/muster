package mirrorhost

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestASessionsFileWriteSendsTheStatusBeforeTheRefresh: the operator answers
// a waiting Claude Code session in its own terminal, the CLI rewrites its
// sessions file, and muster hears "idle" within about a second — not at the
// next Refresh, which is an hour away here, so the tick cannot be what sent
// it. The config dir is the one the PANE names (ClaudePane.ConfigDir: the
// CLI's own CLAUDE_CONFIG_DIR), not one the backend states.
//
// This file uses only what predates the watch, so it runs against a tree
// without it: there it times out waiting for the sessions message.
func TestASessionsFileWriteSendsTheStatusBeforeTheRefresh(t *testing.T) {
	cfg := t.TempDir()
	sessions := filepath.Join(cfg, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sessions, "4242.json")
	writeStatus := func(st string, at int64) {
		t.Helper()
		body := `{"pid":4242,"sessionId":"g1","status":"` + st + `","statusUpdatedAt":` + strconv.FormatInt(at, 10) + `}`
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const waitingAt, idleAt = int64(1_792_000_000_100), int64(1_792_000_000_900)
	writeStatus(mirror.StatusWaiting, waitingAt)

	win := func(st string, at int64) WindowInfo {
		w := statusClaudeWindow(filepath.Join(cfg, "projects", "p", "g1.jsonl"), st, at)
		w.Claude.ConfigDir = cfg
		return w
	}
	b := &windowsBackend{windows: []WindowInfo{win(mirror.StatusWaiting, waitingAt)}}
	f := newFakeMuster(t)
	c, err := New(Config{URL: f.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: statusWindows{b},
		AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(f.of(mirror.TypeHello)) == 1 })
	if got := f.of(mirror.TypeHello)[0].Windows; len(got) != 1 || got[0].Status != mirror.StatusWaiting {
		t.Fatalf("hello windows %+v, want one waiting", got)
	}
	waitUntil(t, "the connection to be current", func() bool {
		c.curMu.Lock()
		defer c.curMu.Unlock()
		return c.cur != nil
	})
	// Let the watch take the first listing's directories before the write.
	time.Sleep(300 * time.Millisecond)

	// What the backend reads from now on, and the write that says so.
	b.set(win(mirror.StatusIdle, idleAt))
	start := time.Now()
	writeStatus(mirror.StatusIdle, idleAt)

	deadline := start.Add(2 * time.Second)
	for len(f.of(mirror.TypeSessions)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no sessions message %v after the sessions file was rewritten (Refresh is an hour): the write did not trigger a refresh", time.Since(start))
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := f.of(mirror.TypeSessions)[0].Windows
	if len(got) != 1 || got[0].Status != mirror.StatusIdle || got[0].StatusSince != idleAt {
		t.Fatalf("sent %+v, want the window idle since %d", got, idleAt)
	}
	t.Logf("idle reached muster %v after the write", time.Since(start))
}
