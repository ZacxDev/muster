package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/podagent"
	"github.com/ZacxDev/muster/internal/procstat"
)

// ---------------------------------------------------------------------------
// STRAY PROCESSES: every live process in the pod that ccd cannot account for —
// in practice, one a tool call started and left running. Reported on GET
// /v1/clis (cliActivity.Processes) so muster's owner_asked guard
// (internal/api chief_owner_release.go) can refuse while one is alive.
//
// Why: the claude-code chief runs every tool without a prompt (PR #626). An
// injected message can make it start a detached loop (`nohup sh -c 'until …;
// curl $MUSTER_AGENT_CONNECT_URL …' &`) that holds the agent's muster token
// and waits for the owner's next turn — when the CLI-activity check passes —
// to make an owner_asked call itself. The CLIs' own statuses cannot see such a
// process; the process table can.
//
// 🔴 AN ALLOWLIST, NOT A DESCENT TEST. ccd runs as the pod's PID 1 (the
// entrypoint execs `ccd serve`; under Compose, `ccd compose` is PID 1 and ccd
// serve its child), so an orphaned loop is reparented to ccd itself: "descends
// from a CLI" would miss exactly the detached process this exists for. So
// every live process in the scope is a stray UNLESS it is one of:
//
//  1. ccd itself, or one of its ancestors (ccd compose);
//  2. a CLI ccd started (the primary's and each lane's tmux pane process,
//     read from tmux — never matched by name), or that CLI's parent (the tmux
//     server that runs the pane);
//  3. a DIRECT child of (1) whose executable is EXACTLY the resolved tmux,
//     git or ccd binary — ccd's own short helpers (tmux clients, the
//     checkout's git, compose's helpers);
//  4. a DIRECT child of a CLI in (2) that is the ccd binary (a hook: `ccd
//     hook`) or the stdio MCP server ccd registers itself (`hq mcp`:
//     executable EXACTLY the resolved hq binary, arguments exactly
//     podagent.HQArgs).
//
// Executables are compared by their full resolved path (/proc/<pid>/exe), so a
// copy of curl renamed to `git` is a stray. Exempt processes are exempt
// THEMSELVES only: a descendant of an exempt git, tmux, hook or MCP server is
// counted. Zombies (dead, unreaped) are not counted.
//
// ⚠ WHAT IT DOES NOT CLOSE: a process that ccd's own tmux pane was respawned
// into, or an exec of the ccd, tmux, git or hq binaries themselves (none of
// which makes a muster MCP call). A stdio MCP server an operator added to the
// chief's .claude.json is NOT on the allowlist (the file is writable from
// inside the pod, so trusting it would let a tool call allowlist itself): its
// process counts as a stray, and owner-asked actions stay paused while it
// runs. A transient process (a hook, a status line, a checkout's git child)
// alive at the instant of a scan also counts: the guard refuses, fail closed,
// and the chief can ask again.
// ---------------------------------------------------------------------------

// procReport is cliActivity.Processes (muster's agents.ProcessReport).
type procReport struct {
	// Scanned: the scan completed. False with Error set when it could not
	// (an unreadable /proc, a CLI pid tmux would not give) — muster refuses.
	Scanned bool        `json:"scanned"`
	Error   string      `json:"error,omitempty"`
	Strays  []strayProc `json:"strays,omitempty"`
}

// strayProc is one process the scan could not account for.
type strayProc struct {
	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	Comm string `json:"comm"`
}

// stdioServer is an MCP stdio server ccd registers: its executable and args.
type stdioServer struct {
	exe  string
	args []string
}

// strayInputs is what the scan is told: who ccd is, which CLIs it started,
// and which executables are its helpers.
type strayInputs struct {
	// self is ccd's pid; scope is the root of the process tree scanned (the
	// topmost ancestor of self: the pod's PID 1).
	self, scope int
	// clis are the pane processes ccd started.
	clis []int
	// helperExes may be direct children of ccd or its ancestors (rule 3);
	// hookExe and mcp may be direct children of a CLI (rule 4).
	helperExes []string
	hookExe    string
	mcp        []stdioServer
}

type procInfo struct {
	ppid int
	comm string
}

// procSelf is ccd's pid; tests point it at a fixture's.
var procSelf = os.Getpid

// strayProcesses scans root (a /proc) under in and returns every stray, sorted
// by pid, or an error when the scan cannot be trusted.
func strayProcesses(root string, in strayInputs) ([]strayProc, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	procs := map[int]procInfo{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			continue // exited between the listing and the read
		}
		st, ok := procstat.Parse(b)
		if !ok || st.State == "Z" || st.State == "X" {
			continue
		}
		procs[pid] = procInfo{ppid: st.PPID, comm: st.Comm}
	}
	if _, ok := procs[in.self]; !ok {
		return nil, fmt.Errorf("ccd's own process %d is not in %s", in.self, root)
	}
	// In scope: in.scope and everything under it.
	inScope := func(pid int) bool {
		for i := 0; i < maxProcWalk && pid > 0; i++ {
			if pid == in.scope {
				return true
			}
			p, ok := procs[pid]
			if !ok {
				return false
			}
			pid = p.ppid
		}
		return false
	}
	// Rule 1: ccd and its ancestors.
	core := map[int]bool{}
	for pid, i := in.self, 0; pid > 0 && i < maxProcWalk; i++ {
		core[pid] = true
		p, ok := procs[pid]
		if !ok {
			break
		}
		pid = p.ppid
	}
	// Rule 2: the CLIs ccd started, and each one's tmux server.
	cli := map[int]bool{}
	allowed := map[int]bool{}
	for _, pid := range in.clis {
		p, ok := procs[pid]
		if !ok {
			continue // exited since tmux named it: nothing left to count
		}
		cli[pid], allowed[pid], allowed[p.ppid] = true, true, true
	}
	exeIs := func(pid int, want string) bool {
		if want == "" {
			return false
		}
		got, err := procExe(root, pid)
		return err == nil && got == want
	}
	var out []strayProc
	for pid, p := range procs {
		if core[pid] || allowed[pid] || !inScope(pid) {
			continue
		}
		switch {
		case core[p.ppid] && slices.ContainsFunc(in.helperExes, func(x string) bool { return exeIs(pid, x) }):
			continue // rule 3
		case cli[p.ppid] && exeIs(pid, in.hookExe):
			continue // rule 4: a hook
		case cli[p.ppid] && slices.ContainsFunc(in.mcp, func(s stdioServer) bool { return exeIs(pid, s.exe) && argsAre(root, pid, s.args) }):
			continue // rule 4: ccd's own stdio MCP server
		}
		out = append(out, strayProc{PID: pid, PPID: p.ppid, Comm: p.comm})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// argsAre reports whether pid's arguments after argv[0] are exactly want.
func argsAre(root string, pid int, want []string) bool {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	argv := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	return len(argv) >= 1 && slices.Equal(argv[1:], want)
}

// scopeRoot is the topmost ancestor of self in root: the pod's PID 1.
func scopeRoot(root string, self int) int {
	pid := self
	for i := 0; i < maxProcWalk; i++ {
		pp, err := procParent(root, pid)
		if err != nil || pp <= 0 {
			return pid
		}
		pid = pp
	}
	return pid
}

// resolvedExe is the absolute, symlink-free path of program (looked up on
// PATH when it has no slash), or "" when there is none.
func resolvedExe(program string, look func(string) (string, error)) string {
	p, err := look(program)
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// cliPIDs is every CLI ccd started that is running now: the primary's and
// each lane's pane process. A CLI not started yet has no process to count; any
// other failure to name one fails the scan.
func (s *server) cliPIDs(ctx context.Context) ([]int, error) {
	srvs := []*server{s}
	if s.lanes != nil {
		s.lanes.mu.Lock()
		for _, ln := range s.lanes.set {
			if ln.srv != nil {
				srvs = append(srvs, ln.srv)
			}
		}
		s.lanes.mu.Unlock()
	}
	var out []int
	for _, sv := range srvs {
		if sv.cliPID == nil {
			continue
		}
		pid, err := sv.cliPID(ctx)
		if errors.Is(err, errNotStarted) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("name a CLI's process: %w", err)
		}
		out = append(out, pid)
	}
	return out, nil
}

// processReport is the scan of this pod, as GET /v1/clis reports it.
func (s *server) processReport(ctx context.Context) *procReport {
	clis, err := s.cliPIDs(ctx)
	if err != nil {
		return &procReport{Error: err.Error()}
	}
	self := procSelf()
	ccdExe := ""
	if p, err := os.Executable(); err == nil {
		ccdExe = resolvedExe(p, func(x string) (string, error) { return x, nil })
	}
	in := strayInputs{
		self: self, scope: scopeRoot(procRoot, self), clis: clis,
		helperExes: []string{resolvedExe("tmux", exec.LookPath), resolvedExe("git", exec.LookPath), ccdExe},
		hookExe:    ccdExe,
	}
	if hq := resolvedExe(podagent.HQCommand, hqLookPath); hq != "" {
		in.mcp = append(in.mcp, stdioServer{exe: hq, args: podagent.HQArgs})
	}
	strays, err := strayProcesses(procRoot, in)
	if err != nil {
		return &procReport{Error: err.Error()}
	}
	return &procReport{Scanned: true, Strays: strays}
}
