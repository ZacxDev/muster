//go:build darwin

package mirrorhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The macOS process source against the REAL system: runs only on a Mac
// (`go test ./internal/mirrorhost -run Darwin -v`). Every check is on this
// test's own processes: itself, and a child it starts and kills by pid.
func TestDarwinSourceReadsThisHostsOwnProcesses(t *testing.T) {
	dir := t.TempDir()
	// A child named "claude" (a symlink to sleep), in its own cwd, with its
	// own CLAUDE_CONFIG_DIR: what the locator looks for.
	link := filepath.Join(dir, "claude")
	if err := os.Symlink("/bin/sleep", link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link, "60")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "CLAUDE_CONFIG_DIR=/opt/dev/.claude-probe"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	src := hostProcs()
	procs, children, err := src.table()
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	me := os.Getpid()
	self, ok := procs[me]
	if !ok {
		t.Fatalf("this test's own pid %d is not in the table (%d processes)", me, len(procs))
	}
	if self.ppid != os.Getppid() || self.argvUnread || len(self.argv) == 0 || self.argv[0] != os.Args[0] {
		t.Errorf("own process = %+v; want ppid %d and argv[0] %q", self, os.Getppid(), os.Args[0])
	}
	// The start token, byte for byte what Claude Code writes as procStart.
	out, err := exec.Command("/bin/sh", "-c", "LC_ALL=C TZ=UTC /bin/ps -o lstart= -p "+strconv.Itoa(me)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(out)); self.start != want {
		t.Errorf("start = %q, want ps's own %q", self.start, want)
	}
	if got := findClaude(me, procs, children); got != child {
		t.Errorf("findClaude(self) = %d, want the child %d (%+v)", got, child, procs[child])
	}
	env, err := src.environ(child)
	if v, ok := envLookup(env, "CLAUDE_CONFIG_DIR"); err != nil || !ok || v != "/opt/dev/.claude-probe" {
		t.Errorf("environ(child) = %q, %v", env, err)
	}
	// The real kern.procargs2 layout: a child exec'd with an EMPTY argv[0]
	// keeps it, and its environment is not shifted into argv.
	empty := exec.Command("/bin/sleep")
	empty.Args = []string{"", "61"}
	empty.Env = []string{"MUSTER_PROBE=1"}
	if err := empty.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = empty.Process.Kill(); _ = empty.Wait() })
	pa, ok := procArgsOf(empty.Process.Pid)
	if !ok || len(pa.argv) != 2 || pa.argv[0] != "" || pa.argv[1] != "61" {
		t.Errorf("empty argv[0] child: procargs2 = %+v, %v; want argv [\"\" 61]", pa, ok)
	}
	if env, err := environOf(pa, ok); err != nil || len(env) == 0 || env[0] != "MUSTER_PROBE=1" {
		t.Errorf("empty argv[0] child: environ = %q, %v; want [MUSTER_PROBE=1 …]", env, err)
	}

	wantDir, _ := filepath.EvalSymlinks(dir)
	if got, ok := cwdOf(src, child); !ok || got != wantDir {
		t.Errorf("cwdOf(child) = %q, %v; want %q", got, ok, wantDir)
	}
	if got := src.cwds([]int{child, me}); got[child] != wantDir || got[me] == "" {
		t.Errorf("cwds = %v", got)
	}
}
