package mirrorhost

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// procSource is how this host reads its processes: the table the Claude
// Code locator walks, processes' working directories, and a process's
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
	// cwds is each of pids' working directory; a pid whose cwd cannot be read
	// is absent. A listing asks for every pid it may need through one cwdMemo,
	// so a backend that execs (lsof) execs once; one pid is cwds([]int{pid}).
	cwds(pids []int) map[int]string
	// environ is process pid's environment as it was exec'd, KEY=VALUE
	// strings in order; the error says it could not be read (on macOS, also
	// that the kernel returned no environment at all: errEnvAbsent).
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

// cwdOf is pid's working directory from src, and false when it cannot be read.
func cwdOf(src procSource, pid int) (string, bool) {
	d, ok := src.cwds([]int{pid})[pid]
	return d, ok
}

// procFS is a procSource over a proc filesystem rooted at root.
type procFS struct{ root string }

func (p procFS) table() (map[int]procInfo, map[int][]int, error) { return readProcTable(p.root) }

func (p procFS) cwds(pids []int) map[int]string {
	out := map[int]string{}
	for _, pid := range pids {
		if d, err := os.Readlink(filepath.Join(p.root, strconv.Itoa(pid), "cwd")); err == nil {
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

// cwdMemo is a procSource whose cwds answers a known set of pids from ONE
// read of the source it wraps, made on first use; a pid outside the set is
// read when asked, and kept. What makes one Tmux.Windows listing — the
// locator's cwd guesses, then every window's cwd — one lsof on macOS rather
// than one per pane. Not safe for concurrent use: one listing's own.
type cwdMemo struct {
	procSource
	want  map[int]bool
	got   map[int]string
	read  bool
	reads int // calls made to the wrapped cwds (a test's counter)
}

func newCwdMemo(src procSource, pids []int) *cwdMemo {
	m := &cwdMemo{procSource: src, want: map[int]bool{}, got: map[int]string{}}
	for _, p := range pids {
		if p > 0 {
			m.want[p] = true
		}
	}
	return m
}

func (m *cwdMemo) cwds(pids []int) map[int]string {
	if !m.read {
		m.read = true
		all := make([]int, 0, len(m.want))
		for p := range m.want {
			all = append(all, p)
		}
		sort.Ints(all)
		if len(all) > 0 {
			m.reads++
			m.got = m.procSource.cwds(all)
		}
	}
	out := map[int]string{}
	var miss []int
	for _, p := range pids {
		if d, ok := m.got[p]; ok {
			out[p] = d
		} else if !m.want[p] && p > 0 {
			miss = append(miss, p)
		}
	}
	if len(miss) > 0 {
		m.reads++
		for p, d := range m.procSource.cwds(miss) {
			out[p], m.got[p] = d, d
		}
		for _, p := range miss {
			m.want[p] = true
		}
	}
	return out
}
