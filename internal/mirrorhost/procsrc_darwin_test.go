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
	wantDir, _ := filepath.EvalSymlinks(dir)
	if got, err := src.cwd(child); err != nil || got != wantDir {
		t.Errorf("cwd(child) = %q, %v; want %q", got, err, wantDir)
	}
	if got := src.cwds([]int{child, me}); got[child] != wantDir || got[me] == "" {
		t.Errorf("cwds = %v", got)
	}
}
