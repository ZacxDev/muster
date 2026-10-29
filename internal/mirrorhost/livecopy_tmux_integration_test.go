//go:build tmuxit

package mirrorhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTmuxAWindowWhoseConversationRunsElsewhereNamesItsLiveCopy, on a private
// tmux server: a window's Claude Code shows session lcOwn, and lcCopy — the
// same conversation, its transcript beginning with the same origin row — is
// named by the sessions file of ANOTHER running process (this test's own, a
// stand-in for the client the person resumed from). Tmux.Windows reports the
// live copy on the window when its LiveCopies is set, and none when it is not:
// the seam from the listing to the scanner (livecopy.go).
func TestTmuxAWindowWhoseConversationRunsElsewhereNamesItsLiveCopy(t *testing.T) {
	r := newTmuxRig(t)
	home, cwd := t.TempDir(), t.TempDir()
	project := filepath.Join(home, "projects", ProjectSlug(cwd))
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, id := range []string{lcOwn, lcCopy} {
		p := filepath.Join(project, id+".jsonl")
		if err := os.WriteFile(p, []byte(lcRows(id, lcOrigin, lcOrigTS, "", old)), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
	}
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": lcCopy, "cwd": cwd, "status": "idle"})
	if err := os.WriteFile(filepath.Join(home, "sessions", strconv.Itoa(os.Getpid())+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	r.run(append([]string{"new-session", "-d", "-s", "scratch", "-x", "120", "-y", "40", "-c", cwd}, fakeClaude(t, t.TempDir(), home, lcOwn)...)...)
	w := strings.TrimSpace(r.run("display-message", "-p", "-t", "=scratch:", "#{window_id}"))
	waitFor(t, "the fake CLI to start", func() bool { return strings.Contains(r.windowPane(w), "FAKE-CLAUDE "+lcOwn) })

	ctx := context.Background()
	pane := func(b Tmux) *ClaudePane {
		var c *ClaudePane
		waitFor(t, "the window to resolve", func() bool {
			ws, _ := b.Windows(ctx)
			got, _ := FindWindow(ws, "scratch", w)
			c = got.Claude
			return c != nil
		})
		return c
	}
	if c := pane(Tmux{Socket: r.sock, Claude: ClaudeLocator{Home: home}}); c.Gen != lcOwn || c.LiveCopy != "" {
		t.Fatalf("control, no LiveCopies: %+v, want %s with no live copy", c, lcOwn)
	}
	on := Tmux{Socket: r.sock, Claude: ClaudeLocator{Home: home}, LiveCopies: &LiveCopies{}}
	if c := pane(on); c.Gen != lcOwn || c.LiveCopy != lcCopy {
		t.Fatalf("window = %+v, want %s with live copy %s", c, lcOwn, lcCopy)
	}
}
