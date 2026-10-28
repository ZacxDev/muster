//go:build darwin

package mirrorhost

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// hostProcs is this host's process source. macOS has no /proc, so it asks the
// system, as the user muster-mirror runs as, with no root:
//
//   - the table: ONE `ps` (psArgs) for pid, parent, state, start time and
//     name; then, for each of this user's own processes, kern.procargs2 for
//     its exact argv (another user's is refused by the kernel, and is never
//     Claude Code this host can read anyway);
//   - a process's environment: kern.procargs2 again, for that one pid;
//   - working directories: ONE `lsof` for every pid asked.
//
// ⚠ Written against the documented formats and Claude Code's own macOS code
// path, and tested on Linux with fixtures of those formats — not measured on a
// real Mac (see the PR that added it).
func hostProcs() procSource { return darwinProcs{} }

type darwinProcs struct{}

// The absolute paths: a launchd agent's PATH is minimal, and ps and lsof live
// at these paths on every macOS.
const (
	darwinPS   = "/bin/ps"
	darwinLsof = "/usr/sbin/lsof"
)

// darwinExecTimeout bounds each exec, so a wedged ps or lsof cannot stall a
// refresh.
const darwinExecTimeout = 5 * time.Second

func darwinRun(bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), darwinExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	// Only the locale and zone (psEnv): both tools are run by absolute path
	// and read nothing else from the environment.
	cmd.Env = psEnv
	return cmd.Output()
}

func (darwinProcs) table() (map[int]procInfo, map[int][]int, error) {
	out, err := darwinRun(darwinPS, psArgs...)
	if err != nil {
		return map[int]procInfo{}, map[int][]int{}, err
	}
	me := os.Getuid()
	procs, children := psTable(parsePS(out), func(r psRow) (procArgs, bool) {
		if r.uid != me {
			return procArgs{}, false
		}
		return procArgsOf(r.pid)
	})
	return procs, children, nil
}

// procArgsOf is pid's kern.procargs2, parsed.
func procArgsOf(pid int) (procArgs, bool) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return procArgs{}, false
	}
	return parseProcArgs2(b)
}

func (darwinProcs) cwds(pids []int) map[int]string {
	var list []string
	for _, p := range pids {
		if p > 0 {
			list = append(list, strconv.Itoa(p))
		}
	}
	if len(list) == 0 {
		return map[int]string{}
	}
	// lsof exits 1 when ANY pid is gone or unreadable, and still prints the
	// rest: the output is read whatever the status.
	out, _ := darwinRun(darwinLsof, append(lsofCwdArgs, strings.Join(list, ","))...)
	return parseLsofCwds(out)
}

func (darwinProcs) environ(pid int) ([]string, error) {
	return environOf(procArgsOf(pid))
}
