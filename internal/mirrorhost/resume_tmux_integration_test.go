//go:build tmuxit

package mirrorhost

import (
	"context"
	"os"
	"os/exec"
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
//   - runs the argv as separate arguments (through the login shell's constant
//     `exec "$@"`): an argument holding shell syntax reaches the program as ONE
//     literal argument, and nothing it spells runs.
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

// TestTmuxAResumeWindowRunsWithTheUsersLoginPath: muster-mirror runs as a
// service whose PATH holds tmux alone, and tmux gives the window it starts the
// PATH of the client that asked — so a resumed session got that PATH, and
// every shell tool and hook it ran was "command not found". The window must get
// the user's login PATH, as an ordinary window does, in both of the ways the
// host can be found:
//   - the operator's tmux server is running (started from their login
//     session): the window gets that server's PATH;
//   - no server is running, so the resume starts it, from the service's own
//     environment: the user's login shell builds the PATH.
//
// Either way bash and git resolve inside the window although the asking
// process could find neither.
//
// ⚠ The second case tells the login shell apart only on a host whose /bin/sh
// has no built-in PATH that finds git (NixOS: measured red with loginArgv
// removed). On an FHS host — CI's runner — a window with NO PATH still finds
// /usr/bin/git through that default, so there it guards only the start.
func TestTmuxAResumeWindowRunsWithTheUsersLoginPath(t *testing.T) {
	tm, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	if tm, err = filepath.EvalSymlinks(tm); err != nil {
		t.Fatal(err)
	}
	narrow := filepath.Dir(tm)
	for _, tc := range []struct {
		name string
		// operator: the server is started first, from this test's own
		// environment; otherwise the resume starts it.
		operator bool
	}{
		{"the operator's server is running", true},
		{"the resume starts the server", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTmuxRig(t)
			if tc.operator {
				r.session("main", "M")
			}
			unitEnv(t, narrow)
			if _, err := exec.LookPath("git"); err == nil {
				t.Fatalf("the control is void: git resolves on the narrowed PATH %s", narrow)
			}
			b := Tmux{Socket: r.sock, Bin: tm}
			out := filepath.Join(t.TempDir(), "found")
			argv := []string{"/bin/sh", "-c", `{ command -v bash; command -v git; echo end; } > "$0" 2>&1; exec sleep 60`, out}
			if _, err := b.NewResumeWindow(context.Background(), "muster-resume", "resume-0b5e55ef", t.TempDir(), argv); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the window's program to look up bash and git", func() bool {
				b, _ := os.ReadFile(out)
				return strings.Contains(string(b), "end\n")
			})
			got, _ := os.ReadFile(out)
			lines := strings.Split(strings.TrimSpace(string(got)), "\n")
			if len(lines) != 3 || !filepath.IsAbs(lines[0]) || filepath.Base(lines[0]) != "bash" ||
				!filepath.IsAbs(lines[1]) || filepath.Base(lines[1]) != "git" {
				t.Fatalf("inside the resumed window, command -v bash git found %q; want both, by absolute path", got)
			}
		})
	}
}

// unitEnv makes this process's environment muster-mirror's systemd unit's for
// the rest of the test: PATH narrow alone, plus the identity a user manager
// gives a service (HOME, USER, LOGNAME, SHELL; TMUX_TMPDIR as the unit sets
// it) — none of what a login session exports. The rig's own tmux commands
// still find tmux on it.
func unitEnv(t *testing.T, narrow string) {
	t.Helper()
	saved := os.Environ()
	keep := map[string]string{}
	for _, k := range []string{"HOME", "USER", "LOGNAME", "SHELL", "TMUX_TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			keep[k] = v
		}
	}
	os.Clearenv()
	for k, v := range keep {
		_ = os.Setenv(k, v)
	}
	_ = os.Setenv("PATH", narrow)
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			k, v, _ := strings.Cut(kv, "=")
			_ = os.Setenv(k, v)
		}
	})
}
