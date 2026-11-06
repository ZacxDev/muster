package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/mirror"
)

// The host paste guard: what may run in a pane before this host types into
// it.
//
// 🔴 IT FAILS CLOSED. It answers "could anything in this pane's process tree
// read a typed key as something other than a message", so a paste is refused
// on any doubt. A false refusal is safe; a false admit is not.
//
// It refuses a pane whose tree holds:
//   - an opencode process ON THE PANE'S TERMINAL (openCodeShape,
//     onPaneTerminal): sending to opencode is not supported yet, and a blind
//     paste+Enter there can answer its permission dialog;
//   - a terminal multiplexer CLIENT (tmux, screen, dtach, abduco, zellij):
//     `TMUX= tmux -L other attach` in a pane shows a session of ANOTHER server,
//     whose processes are not in this pane's tree, so an opencode there would
//     be invisible to the walk. Every nested multiplexer is refused — a false
//     refusal for one that runs no agent, accepted as safe;
//   - a process it could not read (its stat or cmdline), which is not
//     presumed safe. A child forked after the walk read the process table is
//     read on its own, then (readProcEntry): only one still unreadable
//     refuses. A process whose command line reads empty (mid-exec, exiting)
//     is read once more: gone, its pid reused or a zombie by then, it is
//     skipped; still alive with none, or unreadable while still there, it
//     refuses.
//
// Known gaps: an agent run over ssh or `docker exec` inside the pane
// (invisible to a local process walk); and a host with no /proc (macOS),
// where this guard sees no processes and admits — a ratified gap: failing
// closed there would take the Terminal view's message box away from every
// macOS host.

// The guard's refusals. Their text is the person's (mirror.PasteRefused*Text):
// muster shows a host's refusal as is. Never with internals appended — those
// are logged on the host.
var (
	// ErrMultiplexerPane: the pane's process tree holds a multiplexer client.
	ErrMultiplexerPane = errors.New(mirror.PasteRefusedMultiplexerText)
	// ErrPaneUnchecked: the guard could not read the pane's processes.
	ErrPaneUnchecked = errors.New(mirror.PasteRefusedUncheckedText)
	// ErrOpenCodePane: the pane's process tree holds opencode, which muster
	// does not type into (openCodeShape).
	ErrOpenCodePane = errors.New(mirror.PasteRefusedOpenCodeText)
)

// pasteRefusalOf is the Msg.Refusal kind of a guard refusal, "" for any other
// error: a runtime's own refusal (pasteGuard) is its kind.
func pasteRefusalOf(err error) string {
	switch {
	case errors.Is(err, ErrMultiplexerPane):
		return mirror.PasteRefusalMultiplexer
	case errors.Is(err, ErrPaneUnchecked):
		return mirror.PasteRefusalUnchecked
	}
	for _, r := range knownRuntimes {
		if g, ok := r.(pasteGuard); ok && errors.Is(err, g.refusal()) {
			return g.refusalKind()
		}
	}
	return claudeRefusalOf(err)
}

// GuardProc is the process in a pane's tree that made the guard refuse.
type GuardProc struct {
	// PID is the process.
	PID int
	// Rule names which pattern matched (one of the rule* constants): what a
	// log line or a diagnostic prints. Never argv, which can hold anything
	// the person typed.
	Rule string
}

// The patterns of the guard.
const (
	// ruleMultiplexer: a terminal multiplexer client, which can show an agent
	// this walk cannot see.
	ruleMultiplexer = "multiplexer client"
	// ruleOpenCode: an opencode process (openCodeShape) on the pane's
	// terminal: a paste into opencode is blind (nothing checks what its
	// screen shows), and Enter there can answer its permission dialog.
	// muster's send to opencode goes through opencode's own API, never
	// keystrokes, once that exists.
	ruleOpenCode = "opencode"
)

// multiplexers are the terminal multiplexers whose client, in a pane, shows
// sessions whose processes are outside that pane's tree.
var multiplexers = map[string]bool{"tmux": true, "screen": true, "dtach": true, "abduco": true, "zellij": true}

// PaneProcs reads what runs in a pane from the proc filesystem: the paste
// guard's process walk.
type PaneProcs struct {
	// Proc is the proc filesystem root ("" = /proc). A test points it at a
	// fixture tree, as ClaudeLocator's do.
	Proc string
}

func (h PaneProcs) proc() string {
	if h.Proc != "" {
		return h.Proc
	}
	return "/proc"
}

// paneProcEntry is how InTree reads one process again (readProcEntry), which
// a test wraps to change it between two reads.
var paneProcEntry = readProcEntry

// noArgv reports whether p's command line could not be read or reads empty.
func noArgv(p procInfo) bool { return p.argvUnread || len(strings.Join(p.argv, "")) == 0 }

// paneProcTable is the table InTree walks: readProcTable, which a test wraps
// to fork a process between the table's read and the walk.
var paneProcTable = readProcTable

// procWalkBound is how many processes InTree looks at before giving up — and
// refusing — so a pathological tree cannot stall a paste.
var procWalkBound = 4096

// InTree is the nearest process at or below root (a pane's #{pane_pid}),
// breadth-first, that makes the guard refuse — an opencode process on the
// pane's terminal or a multiplexer client — and true; or false when the tree
// holds none.
//
// The error is the guard's fail-closed case, which the caller refuses on: the
// proc root could not be read; root is not in it; a process in the tree could
// not be read (its stat, or its cmdline); or the tree is larger than the
// walk's bound. A process's children are taken from the parent links AND from
// the kernel's own lists (/proc/<pid>/task/<tid>/children), so a child absent
// from the table is still found. Such a child is read on its own then
// (readProcEntry): one forked after the table was read — a busy Claude Code
// pane's tool calls fork constantly — is walked like any other, one that has
// exited meanwhile is skipped, and only one that still cannot be read
// refuses. A process whose command line reads empty (mid-exec, exiting) is
// read once more the same way, and refuses only if it is still alive with
// none. A zombie is skipped: it reads no input. The one exception is a
// host with no proc filesystem at all (not Linux, default root): there is
// nothing to walk, so it answers false — a ratified gap, not a check that
// passed.
func (h PaneProcs) InTree(root int) (GuardProc, bool, error) {
	procs, children, err := paneProcTable(h.proc())
	if err != nil {
		if h.Proc == "" && runtime.GOOS != "linux" && errors.Is(err, os.ErrNotExist) {
			return GuardProc{}, false, nil
		}
		return GuardProc{}, false, fmt.Errorf("read %s: %w", h.proc(), err)
	}
	if _, ok := procs[root]; !ok {
		return GuardProc{}, false, fmt.Errorf("process %d is not in %s", root, h.proc())
	}
	queue, seen := []int{root}, map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		if len(seen) >= procWalkBound {
			return GuardProc{}, false, fmt.Errorf("the process tree of %d is larger than %d processes", root, procWalkBound)
		}
		seen[pid] = true
		p, ok := procs[pid]
		if !ok {
			// Listed by the kernel as a child, absent from the table: forked
			// since the table was read, exited since, or unreadable. Read it
			// now. Gone is fine; unreadable is not.
			var err error
			if p, err = readProcEntry(h.proc(), pid); err != nil {
				// Gone is its directory gone, not just its stat: a
				// directory whose stat cannot be read is not proof.
				if _, lerr := os.Lstat(filepath.Join(h.proc(), strconv.Itoa(pid))); errors.Is(lerr, os.ErrNotExist) {
					continue
				}
				return GuardProc{}, false, fmt.Errorf("process %d in the tree of %d could not be read", pid, root)
			}
			procs[pid] = p
		}
		if p.state == "Z" {
			continue
		}
		if noArgv(p) {
			// A command line that reads empty (or not at all) is a process
			// mid-exec or exiting as often as a hidden one: read it ONCE
			// more. Gone, reused by another process (a new start time) or a
			// zombie now reads no input of the process first read; a command
			// line now is walked like any other; still alive with none
			// refuses, and so does a second read that fails on a process
			// whose directory is still there.
			q, err := paneProcEntry(h.proc(), pid)
			switch {
			case err != nil:
				if _, lerr := os.Lstat(filepath.Join(h.proc(), strconv.Itoa(pid))); errors.Is(lerr, os.ErrNotExist) {
					continue
				}
				return GuardProc{}, false, fmt.Errorf("process %d in the tree of %d could not be read again", pid, root)
			case q.start != p.start, q.state == "Z":
				continue
			case noArgv(q):
				return GuardProc{}, false, fmt.Errorf("the command line of process %d in the tree of %d could not be read", pid, root)
			}
			p = q
		}
		if rule, ok := guardShape(p); ok && (rule != ruleOpenCode || h.onPaneTerminal(pid, p, root, procs)) {
			return GuardProc{PID: pid, Rule: rule}, true, nil
		}
		queue = append(queue, children[pid]...)
		queue = append(queue, h.kernelChildren(pid)...)
	}
	return GuardProc{}, false, nil
}

// onPaneTerminal reports whether process pid (p) is on the terminal of the
// pane whose root process is root: its stdin (fd 0) is the root's stdin, or
// its controlling terminal (tty_nr) is the root's. 🔴 IT FAILS CLOSED, as
// the guard does: true when the pane's terminal cannot be read (the root's
// stdin unreadable and no controlling terminal), when pid's stdin cannot be
// read, or when pid has a controlling terminal and the root has none to
// compare. false only for a process PROVEN off it: its stdin readable and
// not the pane's, and no controlling terminal or another one.
func (h PaneProcs) onPaneTerminal(pid int, p procInfo, root int, procs map[int]procInfo) bool {
	rootFd0 := h.fd0(root)
	rootTTY := procs[root].tty
	if rootFd0 == "" && rootTTY == 0 {
		return true
	}
	fd0 := h.fd0(pid)
	switch {
	case fd0 == "":
		return true
	case rootFd0 != "" && fd0 == rootFd0:
		return true
	case p.tty != 0 && (rootTTY == 0 || p.tty == rootTTY):
		return true
	}
	return false
}

// fd0 is what pid's stdin is (its /proc fd/0 link), "" when unreadable.
func (h PaneProcs) fd0(pid int) string {
	d, err := os.Readlink(filepath.Join(h.proc(), strconv.Itoa(pid), "fd", "0"))
	if err != nil {
		return ""
	}
	return d
}

// kernelChildren is pid's children as the kernel lists them, per thread
// (/proc/<pid>/task/<tid>/children); none when the lists cannot be read (a
// kernel without them, a fixture that writes none).
func (h PaneProcs) kernelChildren(pid int) []int {
	files, _ := filepath.Glob(filepath.Join(h.proc(), strconv.Itoa(pid), "task", "*", "children"))
	var out []int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, s := range strings.Fields(string(b)) {
			if c, err := strconv.Atoi(s); err == nil && c > 0 {
				out = append(out, c)
			}
		}
	}
	return out
}

// multiplexerName is a comm or argv[0] reduced to the program's name: its
// basename, before any ':' (tmux's client sets its comm to "tmux: client"),
// lowercased (GNU screen's server is "SCREEN").
func multiplexerName(s string) string {
	s = filepath.Base(s)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// guardShape reports whether p makes the guard refuse, and which rule
// matched: a multiplexer client, else the first runtime whose pasteGuard
// shape matches (knownRuntimes' order).
func guardShape(p procInfo) (string, bool) {
	if multiplexers[multiplexerName(p.comm)] || (len(p.argv) > 0 && multiplexers[multiplexerName(p.argv[0])]) {
		return ruleMultiplexer, true
	}
	for _, r := range knownRuntimes {
		if g, ok := r.(pasteGuard); ok {
			if rule, ok := g.shape(p); ok {
				return rule, true
			}
		}
	}
	return "", false
}

// openCodeNames are the names an opencode process goes by: the program's
// own, and the Nix wrapper's target's comm (makeWrapper execs
// `.opencode-wrapped`, which the kernel truncates to 15 bytes,
// `.opencode-wrapp`; argv[0] stays "opencode", or the full path of
// `.opencode-wrapped` when run by path — whose comm still matches).
var openCodeNames = map[string]bool{"opencode": true, ".opencode-wrapp": true}

// openCodeShape reports whether p is opencode by its PROCESS identity — the
// same identity the mirror's "opencode" label reads from tmux
// (#{pane_current_command} is argv[0]'s basename), taken here from /proc: its
// comm or its argv[0]'s basename. It is the identity only: the walk refuses
// such a process only when it is ON THE PANE'S TERMINAL (onPaneTerminal) —
// an opencode a paste could reach. One that Claude Code's Bash tool started
// with no terminal (`opencode run`, stdin /dev/null, setsid) reads no typed
// key, and does not refuse the Claude pane it runs under.
// A JavaScript shim (an npm or bun install) is not matched itself: it spawns
// the native binary, and the walk, going on into a process that does not
// match, finds that child.
func openCodeShape(p procInfo) bool {
	return openCodeNames[p.comm] || (len(p.argv) > 0 && openCodeNames[filepath.Base(p.argv[0])])
}

// PaneGuardBackend is a backend that can tell what runs in a pane: the paste
// guard's view of it. The tmux backend implements it. A backend that does not
// (a test's fake, the browser suite's fake tmux) has no processes to look at,
// and what it types is not guarded.
type PaneGuardBackend interface {
	// PaneGuard is the process in pane's tree that makes the guard refuse,
	// read NOW (PaneProcs.InTree); false when it holds none. An error means
	// it could not be checked.
	PaneGuard(ctx context.Context, pane string) (GuardProc, bool, error)
}

// PaneGuardOf is b's PaneGuardBackend, looking through wrappers.
func PaneGuardOf(b Backend) (PaneGuardBackend, bool) { return capabilityOf[PaneGuardBackend](b) }

// PaneGuard implements PaneGuardBackend: the pane's root process
// (#{pane_pid}, read now by the pane's id) and a walk of its tree.
func (t Tmux) PaneGuard(ctx context.Context, paneID string) (GuardProc, bool, error) {
	pid, err := t.panePID(ctx, paneID)
	if err != nil {
		return GuardProc{}, false, err
	}
	return PaneProcs{Proc: t.Claude.Proc}.InTree(pid)
}

// panePID is pane's #{pane_pid}, by its id.
func (t Tmux) panePID(ctx context.Context, paneID string) (int, error) {
	if !strings.HasPrefix(paneID, "%") {
		return 0, fmt.Errorf("%q is not a tmux pane id", paneID)
	}
	cmd := exec.CommandContext(ctx, t.bin(), t.query("display-message", "-p", "-t", paneID, "#{pane_pid}")...)
	cmd.Env = tmuxEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("tmux display-message: %v: %s", err, strings.TrimSpace(string(out)))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("tmux reported pane %s's process as %q", paneID, strings.TrimSpace(string(out)))
	}
	return pid, nil
}

// typeInto is the ONE way this host types into a pane: the Terminal view's
// paste, the rendered view's paste and an answer's keys all come through here
// (paneguard_ledger_test.go fails when a call that types reaches tmux any
// other way). 🔴 It runs the paste guard on pane FIRST — by what runs in the
// pane NOW, whatever muster sent, so it holds whichever server version asked —
// then cancels this connection's copy-mode, then types. A refusal types
// nothing and touches nothing.
func (h *hostSide) typeInto(ctx context.Context, pane string, keys func() error) error {
	if err := h.guardPane(ctx, pane); err != nil {
		return err
	}
	h.leaveCopyModeFor(pane)
	return keys()
}

// guardPane is the paste guard for pane: nil to admit, else the
// refusal. Internals (tmux's stderr, which process could not be read) are
// logged here and never put in the refusal the person sees.
func (h *hostSide) guardPane(ctx context.Context, pane string) error {
	g, ok := PaneGuardOf(h.c.cfg.Backend)
	if !ok {
		return nil
	}
	hp, found, err := g.PaneGuard(ctx, pane)
	switch {
	case err != nil:
		h.c.log.Printf("muster-mirror: could not check what runs in pane %s, so nothing is typed into it: %v", pane, err)
		return ErrPaneUnchecked
	case !found:
		return nil
	case hp.Rule == ruleMultiplexer:
		h.c.log.Printf("muster-mirror: pane %s runs a terminal multiplexer client (pid %d), which can hide an agent; nothing is typed into it", pane, hp.PID)
		return ErrMultiplexerPane
	}
	// The runtime whose rule matched refuses in its own words. A rule no
	// runtime owns is refused as unchecked: the guard fails closed.
	for _, r := range knownRuntimes {
		if rg, ok := r.(pasteGuard); ok && rg.owns(hp.Rule) {
			h.c.log.Printf("muster-mirror: pane %s runs %s (pid %d, %s); nothing is typed into it", pane, rg.label(), hp.PID, hp.Rule)
			return rg.refusal()
		}
	}
	h.c.log.Printf("muster-mirror: pane %s: process %d matched rule %q, which no runtime owns; nothing is typed into it", pane, hp.PID, hp.Rule)
	return ErrPaneUnchecked
}
