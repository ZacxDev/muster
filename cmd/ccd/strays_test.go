package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// writeProcTree writes a fixture /proc entry: pid's stat (with ppid and
// state), its cmdline (argv), and its exe link (exe; "" for none).
func writeProcTree(t *testing.T, root string, pid, ppid int, state, comm, exe string, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := strconv.Itoa(pid) + " (" + comm + ") " + state + " " + strconv.Itoa(ppid) + " 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if exe != "" {
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	fxCCD   = "/usr/local/bin/ccd"
	fxTmux  = "/usr/bin/tmux"
	fxGit   = "/usr/bin/git"
	fxHQ    = "/usr/local/bin/hq"
	fxCLI   = "/usr/local/bin/claude"
	fxShell = "/bin/sh"
)

func fixtureInputs(self, scope int, clis ...int) strayInputs {
	return strayInputs{self: self, scope: scope, clis: clis,
		helperExes: []string{fxTmux, fxGit, fxCCD}, hookExe: fxCCD,
		mcp: []stdioServer{{exe: fxHQ, args: []string{"mcp"}}}}
}

func strayPIDs(s []strayProc) []int {
	var out []int
	for _, p := range s {
		out = append(out, p.PID)
	}
	return out
}

// TestTheStrayScanCountsEverythingCcdCannotAccountFor is the allowlist as a
// ledger, on the Kubernetes layout (ccd is PID 1): ccd, its tmux server and the
// two CLIs it started; a CLI's `hq mcp` and `ccd hook`; ccd's own tmux client
// and git — and EVERY other live process is a stray, exactly: an orphaned
// loop reparented to ccd, a CLI's background shell, another window under the
// tmux server, hq run with other arguments, a child of the MCP server, curl
// renamed to git, git's own child, the ccd binary run by a stray. A zombie is
// not counted. Fails if the set grows or shrinks.
func TestTheStrayScanCountsEverythingCcdCannotAccountFor(t *testing.T) {
	root := t.TempDir()
	writeProcTree(t, root, 1, 0, "S", "ccd", fxCCD, "ccd", "serve")
	writeProcTree(t, root, 10, 1, "S", "tmux: server", fxTmux, "tmux", "-L", "cc")
	writeProcTree(t, root, 20, 10, "S", "claude", fxCLI, "claude", "--resume", "x")
	writeProcTree(t, root, 21, 10, "S", "claude", fxCLI, "claude", "--session-id", "y")
	writeProcTree(t, root, 30, 20, "S", "hq", fxHQ, "hq", "mcp")                  // allowed: ccd's MCP server
	writeProcTree(t, root, 31, 21, "S", "ccd", fxCCD, "ccd", "hook")              // allowed: a hook
	writeProcTree(t, root, 40, 1, "S", "tmux", fxTmux, "tmux", "display-message") // allowed: ccd's tmux client
	writeProcTree(t, root, 41, 1, "S", "git", fxGit, "git", "clone")              // allowed: the checkout's git
	writeProcTree(t, root, 46, 1, "Z", "sh", "", "")                              // a zombie: not counted
	// Strays:
	writeProcTree(t, root, 32, 20, "S", "hq", fxHQ, "hq", "tasks", "list")
	writeProcTree(t, root, 33, 30, "S", "sh", fxShell, "sh", "-c", "x")
	writeProcTree(t, root, 42, 1, "S", "git", "/tmp/git", "git", "-sS", "http://muster/mcp")
	writeProcTree(t, root, 43, 1, "S", "sh", fxShell, "sh", "-c", "until false; do sleep 2; done")
	writeProcTree(t, root, 44, 20, "S", "bash", "/bin/bash", "bash", "-c", "sleep 600")
	writeProcTree(t, root, 45, 10, "S", "sh", fxShell, "sh")
	writeProcTree(t, root, 50, 41, "S", "git-remote-http", "/usr/lib/git-core/git-remote-https", "git-remote-https")
	writeProcTree(t, root, 60, 43, "S", "ccd", fxCCD, "ccd", "hook")

	got, err := strayProcesses(root, fixtureInputs(1, 1, 20, 21))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{32, 33, 42, 43, 44, 45, 50, 60}; !reflect.DeepEqual(strayPIDs(got), want) {
		t.Fatalf("strays = %v, want exactly %v", strayPIDs(got), want)
	}
	if got[3] != (strayProc{PID: 43, PPID: 1, Comm: "sh"}) {
		t.Fatalf("a stray is reported as %+v", got[3])
	}
	// Control: the CLIs are allowed BY PID, never by name — a CLI ccd did not
	// start (21 not handed in) is a stray, and so is the hook under it.
	got, err = strayProcesses(root, fixtureInputs(1, 1, 20))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{21, 31, 32, 33, 42, 43, 44, 45, 50, 60}; !reflect.DeepEqual(strayPIDs(got), want) {
		t.Fatalf("with one CLI unknown, strays = %v, want exactly %v", strayPIDs(got), want)
	}
}

// TestTheStrayScanOnTheComposeLayout: under Compose, `ccd compose` is PID 1
// and ccd serve its child. compose is ccd's ancestor (allowed), its ccd helper
// and the tmux server it runs are allowed; an orphan reparented to it is a
// stray.
func TestTheStrayScanOnTheComposeLayout(t *testing.T) {
	root := t.TempDir()
	writeProcTree(t, root, 1, 0, "S", "ccd", "", "ccd", "compose") // root-owned: exe unreadable
	writeProcTree(t, root, 5, 1, "S", "ccd", fxCCD, "ccd", "serve")
	writeProcTree(t, root, 6, 1, "S", "ccd", fxCCD, "ccd", "seed")
	writeProcTree(t, root, 10, 1, "S", "tmux: server", fxTmux, "tmux")
	writeProcTree(t, root, 20, 10, "S", "claude", fxCLI, "claude")
	writeProcTree(t, root, 70, 1, "S", "sh", fxShell, "sh", "-c", "loop")
	got, err := strayProcesses(root, fixtureInputs(5, scopeRoot(root, 5), 20))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{70}; !reflect.DeepEqual(strayPIDs(got), want) {
		t.Fatalf("strays = %v, want exactly %v", strayPIDs(got), want)
	}
}

// TestTheStrayScanFailsRatherThanGuessing: a /proc that cannot be read, or one
// without ccd's own process, is an error — never an empty, clean-looking list.
func TestTheStrayScanFailsRatherThanGuessing(t *testing.T) {
	if _, err := strayProcesses(filepath.Join(t.TempDir(), "absent"), fixtureInputs(1, 1)); err == nil {
		t.Error("an unreadable /proc scanned clean")
	}
	root := t.TempDir()
	writeProcTree(t, root, 20, 1, "S", "sh", fxShell, "sh")
	if _, err := strayProcesses(root, fixtureInputs(1, 1)); err == nil {
		t.Error("a /proc without ccd's own process scanned clean")
	}
}

// TestTheProcessReportFailsClosedWhenACLICannotBeNamed: the report a GET
// /v1/clis carries is NOT scanned (and so refused by muster) when a CLI ccd
// started cannot be named; a CLI not started yet is simply absent.
func TestTheProcessReportFailsClosedWhenACLICannotBeNamed(t *testing.T) {
	root := t.TempDir()
	writeProcTree(t, root, 1, 0, "S", "ccd", fxCCD, "ccd", "serve")
	old, oldSelf := procRoot, procSelf
	procRoot, procSelf = root, func() int { return 1 }
	t.Cleanup(func() { procRoot, procSelf = old, oldSelf })
	srv, _ := newScripted(t, cliScript{})

	srv.cliPID = func(context.Context) (int, error) { return 0, errors.New("tmux: no server running") }
	if rep := srv.processReport(context.Background()); rep.Scanned || !strings.Contains(rep.Error, "no server running") {
		t.Fatalf("a CLI that cannot be named: %+v, want a failed scan", rep)
	}
	srv.cliPID = func(context.Context) (int, error) { return 0, errNotStarted }
	if rep := srv.processReport(context.Background()); !rep.Scanned || len(rep.Strays) != 0 {
		t.Fatalf("a CLI not started yet: %+v, want a clean scan", rep)
	}
}
