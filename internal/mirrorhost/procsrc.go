package mirrorhost

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// procSource is how this host reads its processes: the table the Claude
// Code locator walks, a process's working directory, and a process's
// environment. Two implementations:
//
//   - procFS reads a Linux-style proc filesystem under a root ("/proc", or a
//     test's fixture tree). It is the Linux backend, unchanged from before
//     the seam: the same files read the same way.
//   - darwinProcs (procsrc_darwin.go) asks macOS, which has no /proc: `ps` for
//     the table, the kern.procargs2 sysctl for argv and environment, `lsof`
//     for working directories. Its parsers (procsrc_parse.go) are plain Go
//     with no build tag, so they are tested on Linux.
//
// Only the Claude Code locator and what reads through it (Tmux.Windows,
// resumeLive, the catalog's cwd) go through this. The Hermes and opencode
// runtimes and the background-shell count (internal/cctasks) still read a
// proc root of their own, so on macOS they find nothing, as before.
type procSource interface {
	// table reads every process once: parent, name, argv and start time, and
	// each process's children (sorted). The error is the source's own (no proc
	// root, `ps` failed); a process that cannot be read is left out.
	table() (map[int]procInfo, map[int][]int, error)
	// cwd is process pid's working directory.
	cwd(pid int) (string, error)
	// cwds is each of pids' working directory; a pid whose cwd cannot be read
	// is absent. One call per listing, so a backend that execs (lsof) execs
	// once.
	cwds(pids []int) map[int]string
	// environ is process pid's environment as it was exec'd, KEY=VALUE
	// strings in order; the error says it could not be read.
	environ(pid int) ([]string, error)
}

// procs is the process source l reads: its test source when set, the proc
// root it names when that is set (a fixture tree), else this host's own
// (hostProcs: /proc on Linux, ps/sysctl/lsof on macOS).
func (l ClaudeLocator) procs() procSource {
	if l.src != nil {
		return l.src
	}
	if l.Proc != "" {
		return procFS{root: l.Proc}
	}
	return hostProcs()
}

// procFS is a procSource over a proc filesystem rooted at root.
type procFS struct{ root string }

func (p procFS) table() (map[int]procInfo, map[int][]int, error) { return readProcTable(p.root) }

func (p procFS) cwd(pid int) (string, error) {
	return os.Readlink(filepath.Join(p.root, strconv.Itoa(pid), "cwd"))
}

func (p procFS) cwds(pids []int) map[int]string {
	out := map[int]string{}
	for _, pid := range pids {
		if d, err := p.cwd(pid); err == nil {
			out[pid] = d
		}
	}
	return out
}

// environ reads <root>/<pid>/environ: NUL-separated KEY=VALUE.
func (p procFS) environ(pid int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(p.root, strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil, err
	}
	var env []string
	for _, kv := range bytes.Split(b, []byte{0}) {
		if len(kv) > 0 {
			env = append(env, string(kv))
		}
	}
	return env, nil
}
