package previewhost

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Report is one listener offered to the owner: an allowlisted port this user
// listens on from a process in a mirrored tmux window. These four fields are
// ALL it carries (TestAReportCarriesExactlyItsFourKeys): never the argv,
// working directory, environment or pid.
type Report struct {
	Port      int       `json:"port"`
	Process   string    `json:"process"` // the listening process's name, at most MaxProcessName bytes
	Window    string    `json:"window"`  // the tmux window id ("@12")
	FirstSeen time.Time `json:"first_seen"`
}

// MaxProcessName is the longest process name a report carries: the kernel's
// own comm length (TASK_COMM_LEN 16, less its NUL).
const MaxProcessName = 15

// Proc is one process as the attribution walk needs it.
type Proc struct {
	PPID int
	Comm string
}

// ProcTable reads this host's process table: every process's parent and
// name. muster-mirror passes mirrorhost's (mirrorhost.ProcTable), the source
// the Claude Code locator walks.
type ProcTable func() (map[int]Proc, error)

// maxDepth bounds the walk from a listener up to a pane: a cycle in a torn
// read of the table must end it.
const maxDepth = 64

// Scanner finds the listener reports. Each Scan is one poll; the scanner keeps
// first_seen across polls. Safe for concurrent use.
type Scanner struct {
	// Proc is the proc root for net/tcp{,6} and <pid>/fd ("" = "/proc").
	Proc string
	// UID is this user's: only its listeners are reported.
	UID int
	// Procs reads the process table (required).
	Procs ProcTable
	// Now is the clock (nil = time.Now).
	Now func() time.Time

	mu   sync.Mutex
	seen map[reportKey]time.Time
}

type reportKey struct {
	window, process string
	port            int
}

func (s *Scanner) root() string {
	if s.Proc == "" {
		return "/proc"
	}
	return s.Proc
}

// Scan is one poll. panes maps each mirrored pane's root process (its
// #{pane_pid}) to its tmux window id. A listener is reported when its port is
// allowed (Config.Allows, so never an always-refused one), it listens where a
// loopback dial reaches it, it is this user's, and its process is a pane's
// root or descends from one; a listener in no pane has no session to show it
// in and is not reported. Sorted by window, then port. With offers off
// (Config.Offer false) it reads nothing and reports nothing.
func (s *Scanner) Scan(c Config, refused AlwaysRefused, panes map[int]string) ([]Report, error) {
	if !c.Offer {
		return nil, nil
	}
	read, err := ReadListeners(s.root())
	if err != nil {
		return nil, err
	}
	socks := read.Sockets
	// Only the sockets that could be reported get their owner looked up.
	want := map[uint64]int{} // inode → port
	for _, k := range socks {
		if k.Local() && k.UID == s.UID && c.Allows(k.Port, refused) == nil {
			want[k.Inode] = k.Port
		}
	}
	var out []Report
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	live := map[reportKey]bool{}
	if len(want) > 0 {
		owners := s.socketOwners(want)
		procs, err := s.Procs()
		if err != nil {
			return nil, err
		}
		dup := map[reportKey]bool{}
		for inode, pid := range owners {
			win, ok := paneWindow(pid, procs, panes)
			if !ok {
				continue
			}
			k := reportKey{window: win, process: processName(procs[pid].Comm), port: want[inode]}
			if dup[k] {
				continue // the same server on 127.0.0.1 and ::1
			}
			dup[k] = true
			live[k] = true
			first, ok := s.seen[k]
			if !ok {
				first = now
			}
			out = append(out, Report{Port: k.port, Process: k.process, Window: k.window, FirstSeen: first})
		}
	}
	// A listener that went away is forgotten: when it comes back it is new.
	s.seen = map[reportKey]time.Time{}
	for _, r := range out {
		s.seen[reportKey{window: r.Window, process: r.Process, port: r.Port}] = r.FirstSeen
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Window != out[j].Window {
			return out[i].Window < out[j].Window
		}
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Process < out[j].Process
	})
	return out, nil
}

func (s *Scanner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// socketOwners finds, for each inode in want, a process holding it open: the
// lowest pid whose /proc/<pid>/fd/* links to socket:[<inode>]. Another user's
// fd directory cannot be read and is skipped — its sockets were filtered out
// by uid already.
func (s *Scanner) socketOwners(want map[uint64]int) map[uint64]int {
	root := s.root()
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	out := map[uint64]int{}
	for _, pid := range pids {
		if len(out) == len(want) {
			break
		}
		dir := filepath.Join(root, strconv.Itoa(pid), "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			l, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil || !strings.HasPrefix(l, "socket:[") || !strings.HasSuffix(l, "]") {
				continue
			}
			ino, err := strconv.ParseUint(l[len("socket:["):len(l)-1], 10, 64)
			if err != nil {
				continue
			}
			if _, ok := want[ino]; ok {
				if _, have := out[ino]; !have {
					out[ino] = pid
				}
			}
		}
	}
	return out
}

// paneWindow walks from pid up its parents to a pane's root process, and is
// that pane's window; false when the walk leaves the table, reaches pid 1 or
// 0, or exceeds maxDepth without meeting a pane.
func paneWindow(pid int, procs map[int]Proc, panes map[int]string) (string, bool) {
	for i := 0; i < maxDepth && pid > 1; i++ {
		if w, ok := panes[pid]; ok {
			return w, true
		}
		p, ok := procs[pid]
		if !ok {
			return "", false
		}
		pid = p.PPID
	}
	return "", false
}

// processName is comm as a report carries it: at most MaxProcessName bytes,
// every byte outside printable ASCII replaced by '?', so a name is never more
// than a label.
func processName(comm string) string {
	b := []byte(comm)
	if len(b) > MaxProcessName {
		b = b[:MaxProcessName]
	}
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}
