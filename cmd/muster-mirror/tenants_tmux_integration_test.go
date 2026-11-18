//go:build tmuxit

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hook record's socket against REAL tmux servers (tmux 3.7c measured):
// the daemon DERIVES the socket path from its flags, before its tmux server
// exists, and it must equal the first field of the $TMUX that server gives its
// panes. Every server here lives under a private TMUX_TMPDIR; none is the
// operator's.

// startTmux starts a tmux server (serverArgs: none, -L name or -S path) under
// TMUX_TMPDIR=tmpdir with one pane, and returns the $TMUX that pane got. The
// server is killed when the test ends.
func startTmux(t *testing.T, tmpdir string, serverArgs ...string) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux is not installed; this suite fails rather than skips: %v", err)
	}
	out := filepath.Join(t.TempDir(), "tmux-env")
	cmd := func(args ...string) *exec.Cmd {
		c := exec.Command("tmux", append(append([]string{"-f", "/dev/null"}, serverArgs...), args...)...)
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMUX_TMPDIR=" + tmpdir}
		return c
	}
	if b, err := cmd("new-session", "-d", "-s", "t", "sh", "-c", `printf %s "$TMUX" > `+out+`; exec sleep 300`).CombinedOutput(); err != nil {
		t.Fatalf("start tmux %v: %v: %s", serverArgs, err, b)
	}
	t.Cleanup(func() { _ = cmd("kill-server").Run() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the pane of tmux %v never wrote its $TMUX", serverArgs)
	return ""
}

// shortTmp is a directory short enough for a unix socket path under it
// (sun_path is 108 bytes; a test's TempDir can be longer).
func shortTmp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "mt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// askFrom is the hook run in a pane whose environment is tmux (its $TMUX).
func askFrom(t *testing.T, h *tenantHost, tmux string) {
	t.Helper()
	h.ask(t, tmuxEnvSocket(tmux), nil)
}

// TestTmuxADaemonStartedBeforeItsServerStillGetsItsQuestions: each daemon
// records BEFORE its tmux server exists; the server is started afterwards,
// and a question from its pane reaches that tenant only — for the default
// server, a -L server and a -S server.
func TestTmuxADaemonStartedBeforeItsServerStillGetsItsQuestions(t *testing.T) {
	h := newTenantHost(t)
	h.tmpdir = shortTmp(t)
	h.real, _ = filepath.EvalSymlinks(h.tmpdir)
	t.Setenv("TMUX_TMPDIR", h.tmpdir)
	a, b, c := newTenantServer(t), newTenantServer(t), newTenantServer(t)
	sPath := filepath.Join(h.tmpdir, "s.sock")
	byPath := overlay(h.tenantEnv("path", c), map[string]string{envSocketPath: sPath})
	delete(byPath, envSocket)
	runDaemons(t, h.defaultEnv(a), h.tenantEnv("w", b), byPath)

	// Only now do the servers exist.
	askFrom(t, h, startTmux(t, h.tmpdir))
	counts(t, a, b, 1, 0, "a question from the real default server")
	askFrom(t, h, startTmux(t, h.tmpdir, "-L", "w"))
	counts(t, a, b, 1, 1, "a question from the real -L w server")
	askFrom(t, h, startTmux(t, h.tmpdir, "-S", sPath))
	counts(t, a, b, 1, 1, "a question from the real -S server")
	if c.asks.Load() != 1 {
		t.Fatalf("the -S tenant has %d question(s), want 1", c.asks.Load())
	}
	// A third real server: fails open.
	askFrom(t, h, startTmux(t, h.tmpdir, "-L", "third"))
	counts(t, a, b, 1, 1, "a question from a real third server")
	if c.asks.Load() != 1 {
		t.Fatalf("the -S tenant has %d question(s) after the third server's, want 1", c.asks.Load())
	}
}

// TestTmuxASymlinkedTMUX_TMPDIRMatches: TMUX_TMPDIR is a symlink; tmux puts
// the RESOLVED directory in $TMUX. The recorded socket matches it, and so
// does an old legacy record with no socket (the EvalSymlinks fallback) — the
// Linux stand-in for macOS's /tmp → /private/tmp.
func TestTmuxASymlinkedTMUX_TMPDIRMatches(t *testing.T) {
	h := newTenantHost(t)
	base := shortTmp(t)
	realDir := filepath.Join(base, "r")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "l")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	h.tmpdir, h.real = link, realDir
	t.Setenv("TMUX_TMPDIR", link)
	a, b := newTenantServer(t), newTenantServer(t)
	runDaemons(t, h.defaultEnv(a), h.tenantEnv("w", b))

	def := startTmux(t, link)
	if !strings.HasPrefix(def, realDir+"/") {
		t.Fatalf("fixture: tmux reported %q, not under the resolved %s", def, realDir)
	}
	askFrom(t, h, def)
	counts(t, a, b, 1, 0, "the default server, new record")
	askFrom(t, h, startTmux(t, link, "-L", "w"))
	counts(t, a, b, 1, 1, "the -L w server")

	// An older daemon's legacy record: no socket.
	if err := writeHookConfig(h.legacy(), hookConfig{URL: a.URL, AllowHTTP: true, TokenFile: h.token("default")}); err != nil {
		t.Fatal(err)
	}
	askFrom(t, h, def)
	counts(t, a, b, 2, 1, "the default server, old record (EvalSymlinks fallback)")
}
