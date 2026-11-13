package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/procstat"
)

// GET /v1/clis — WHICH CLIs IN THIS POD ARE WORKING, BY THEIR OWN ACCOUNT.
//
// muster's owner_asked (internal/api chief_owner_release.go) releases a gate
// only while the owner's turn runs in ONE lane and every OTHER CLI in the pod
// is idle: the pod's CLIs share one muster credential, so a tool call does not
// say which CLI made it, and "only the owner's lane is working" is what makes
// the caller the owner's lane.
//
// 🔴 THE STATUS IS THE CLI'S OWN — the `status` Claude Code writes in its
// sessions file (<CLAUDE_CONFIG_DIR>/sessions/<pid>.json: busy, waiting,
// shell, idle) — for every LIVE process that wrote one (the pid is running, not
// a zombie, and has the start time the file records). Never ccd's turn slot or
// hook bookkeeping: a turn ccd answered 504 to that the CLI is still running,
// and a turn the CLI started by itself (a background task finishing), both say
// "busy" here. Every CLI in the pod shares CLAUDE_CONFIG_DIR, so every one —
// the primary, each lane, a job's lane, a background job's own process — is
// listed; one ccd cannot name is listed as not Known. A live CLI PROCESS with no
// matching sessions file is listed as Unaccounted (liveCLIs), never skipped.
//
// A process that is not a Claude Code CLI is not in CLIs: any process the CLI's
// tools leave behind (a detached loop a Bash call started) runs as the same user
// with the agent's muster token and can call muster's MCP door directly. Such a
// process is reported separately, in Processes: every live process ccd cannot
// account for (strays.go), scanned while answering this request.
//
// The wire is muster's agents.CLIActivity (TestTheCLIsAnswerIsMusters).

const cliActivityVersion = 1

type cliEntry struct {
	Lane        string `json:"lane"`
	Primary     bool   `json:"primary"`
	Known       bool   `json:"known"`
	PID         int    `json:"pid"`
	SessionID   string `json:"session_id"`
	Status      string `json:"status"`
	Unaccounted bool   `json:"unaccounted"`
}

type cliActivity struct {
	Version int        `json:"version"`
	CLIs    []cliEntry `json:"clis"`
	// Processes is the stray-process scan (strays.go), made while answering
	// this request. muster refuses owner_asked when it is absent (an older
	// ccd), failed, or names a stray.
	Processes *procReport `json:"processes,omitempty"`
}

// knownCLIs is the session id of the primary's CLI and each lane's, from their
// SessionStart hooks: session id -> (lane key, primary).
func (s *server) knownCLIs() map[string]cliEntry {
	out := map[string]cliEntry{}
	s.mu.Lock()
	if s.sessionID != "" {
		out[s.sessionID] = cliEntry{Primary: true, Known: true}
	}
	s.mu.Unlock()
	if s.lanes == nil {
		return out
	}
	s.lanes.mu.Lock()
	list := make([]*lane, 0, len(s.lanes.set))
	for _, ln := range s.lanes.set {
		list = append(list, ln)
	}
	s.lanes.mu.Unlock()
	for _, ln := range list {
		ln.srv.mu.Lock()
		id := ln.srv.sessionID
		ln.srv.mu.Unlock()
		if id != "" {
			out[id] = cliEntry{Lane: ln.key, Known: true}
		}
	}
	return out
}

// cliStatusUnaccounted is the status of a live CLI process with no matching
// sessions file: it cannot say whether it is working, so it counts as working.
const cliStatusUnaccounted = "unaccounted"

// isCLIProcess reports whether a process is a Claude Code CLI: its argv[0]
// (base name) or its comm is the program ccd starts its CLIs as (bin, base
// name; "claude" by default). That is how every CLI in the pod is launched —
// the primary, each lane (`env CCD_LANE=… claude`, which execs it), and a
// `claude` a tool call runs — whether the binary is native or a node script
// that names itself.
func isCLIProcess(argv0, comm, bin string) bool {
	name := filepath.Base(bin)
	return name != "" && name != "." && (filepath.Base(argv0) == name || comm == name)
}

// liveCLIs is every live CLI process in the pod (root is /proc). One with a
// live sessions file under configDir — the pid running, not a zombie, with the
// start time the file records — is listed with the status the CLI wrote there
// and the lane known names it by. One with NO such file (missing, unreadable,
// naming another pid or a reused pid's start time) is listed too, Unaccounted
// and with status cliStatusUnaccounted: 🔴 it is never left out, because left
// out it would read as idle and muster's check would fail OPEN.
func liveCLIs(configDir, root string, known map[string]cliEntry, bin string) []cliEntry {
	type proc struct {
		argv0 string
		st    procstat.Stat
	}
	procs := map[int]proc{}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			continue
		}
		st, ok := procstat.Parse(stat)
		if !ok || st.State == "Z" {
			continue
		}
		argv0 := ""
		if cmd, err := os.ReadFile(filepath.Join(root, e.Name(), "cmdline")); err == nil {
			argv0, _, _ = strings.Cut(string(cmd), "\x00")
		}
		procs[pid] = proc{argv0: argv0, st: st}
	}
	out := []cliEntry{}
	registered := map[int]bool{}
	files, _ := filepath.Glob(filepath.Join(configDir, "sessions", "*.json"))
	for _, f := range files {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil || pid <= 1 {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var cs cliSession
		if json.Unmarshal(b, &cs) != nil || cs.PID != pid {
			continue
		}
		p, ok := procs[pid]
		if !ok || (cs.ProcStart != "" && cs.ProcStart != p.st.Start) {
			continue // dead, a zombie, or the pid was reused
		}
		registered[pid] = true
		c := known[cs.SessionID]
		c.PID, c.SessionID, c.Status = pid, cs.SessionID, cs.Status
		out = append(out, c)
	}
	for pid, p := range procs {
		if !registered[pid] && isCLIProcess(p.argv0, p.st.Comm, bin) {
			out = append(out, cliEntry{PID: pid, Status: cliStatusUnaccounted, Unaccounted: true})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].PID < out[j].PID
	})
	return out
}

func (s *server) handleCLIs(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.Bearer) {
		writeFailure(w, &failure{Status: http.StatusUnauthorized, Type: failUnauthorized, Message: "missing or wrong bearer"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cliActivity{Version: cliActivityVersion,
		CLIs:      liveCLIs(s.cfg.ConfigDir, procRoot, s.knownCLIs(), s.cliBin()),
		Processes: s.processReport(r.Context())})
}

// cliBin is the program ccd starts its CLIs as: the primary's supervisor's,
// else ccd's default.
func (s *server) cliBin() string {
	if s.sup != nil && s.sup.bin != "" {
		return s.sup.bin
	}
	return "claude"
}
