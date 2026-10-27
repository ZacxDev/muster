//go:build tmuxit

package mirrorhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTmuxAResumeWindowStartsDetachedInItsDirectoryWithNoShell: the resume's
// tmux half (resume.go's NewResumeWindow) against REAL tmux. The window:
//   - starts in the resume session, which is CREATED when absent and REUSED
//     after, with the operator's session's current window untouched (-d);
//   - runs in the directory given;
//   - runs the argv as tmux's own arguments: an argument holding shell syntax
//     reaches the program as ONE literal argument, and nothing it spells runs.
func TestTmuxAResumeWindowStartsDetachedInItsDirectoryWithNoShell(t *testing.T) {
	r := newTmuxRig(t)
	// The operator's session, started by the rig so the server reads no config.
	r.session("main", "M")
	before := strings.TrimSpace(r.run("display-message", "-p", "-t", "=main:", "#{window_id}"))
	b := Tmux{Socket: r.sock}
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	marker := filepath.Join(t.TempDir(), "pwned")
	argv := []string{"sh", "-c", `printf '%s\n' "$1" > "$0"; pwd >> "$0"; exec sleep 60`, out, "x; touch " + marker}
	id, err := b.NewResumeWindow(context.Background(), "muster-resume", "resume-0b5e55ed", dir, argv)
	if err != nil || mirror.ValidWindowID(id) != nil {
		t.Fatalf("the first window: %q, %v", id, err)
	}
	waitFor(t, "the window's program to write its arguments", func() bool {
		b, _ := os.ReadFile(out)
		return strings.Count(string(b), "\n") >= 2
	})
	got, _ := os.ReadFile(out)
	if want := "x; touch " + marker + "\n" + dir + "\n"; string(got) != want {
		t.Fatalf("the window's program saw %q, want %q (its argument literally, then its directory)", got, want)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the shell syntax in an argument RAN: %v", err)
	}
	names := r.run("list-windows", "-t", "=muster-resume", "-F", "#{window_id} #{window_name}")
	if !strings.Contains(names, id+" resume-0b5e55ed") {
		t.Fatalf("the resume session's windows: %q; want %s named resume-0b5e55ed", names, id)
	}
	// Again: the session exists now, so this is a second window in it.
	id2, err := b.NewResumeWindow(context.Background(), "muster-resume", "resume-0b5e55ee", dir, []string{"sleep", "60"})
	if err != nil || id2 == id {
		t.Fatalf("the second window: %q, %v", id2, err)
	}
	if n := len(strings.Fields(r.run("list-windows", "-t", "=muster-resume", "-F", "#{window_id}"))); n != 2 {
		t.Fatalf("the resume session has %d windows, want 2", n)
	}
	if after := strings.TrimSpace(r.run("display-message", "-p", "-t", "=main:", "#{window_id}")); after != before {
		t.Fatalf("the operator's current window moved from %s to %s", before, after)
	}
	// It is listed like any other window, so muster mirrors it.
	ws, err := b.Windows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, w := range ws {
		if w.Session == "muster-resume" {
			listed++
		}
	}
	if listed != 2 {
		t.Fatalf("the backend lists %d of the resume session's windows, want 2", listed)
	}
}
