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
// listed; one ccd cannot name is listed as not Known.
//
// The wire is muster's agents.CLIActivity (TestTheCLIsAnswerIsMusters).

const cliActivityVersion = 1

type cliEntry struct {
	Lane      string `json:"lane"`
	Primary   bool   `json:"primary"`
	Known     bool   `json:"known"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

type cliActivity struct {
	Version int        `json:"version"`
	CLIs    []cliEntry `json:"clis"`
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

// liveCLIs reads every live CLI's sessions file under configDir.
func liveCLIs(configDir, root string, known map[string]cliEntry) []cliEntry {
	files, _ := filepath.Glob(filepath.Join(configDir, "sessions", "*.json"))
	out := []cliEntry{}
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
		stat, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
		if err != nil {
			continue // not running: a sessions file outlives its process
		}
		st, ok := procstat.Parse(stat)
		if !ok || st.State == "Z" || (cs.ProcStart != "" && cs.ProcStart != st.Start) {
			continue // dead, or the pid was reused
		}
		c := known[cs.SessionID]
		c.SessionID, c.Status = cs.SessionID, cs.Status
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

func (s *server) handleCLIs(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.Bearer) {
		writeFailure(w, &failure{Status: http.StatusUnauthorized, Type: failUnauthorized, Message: "missing or wrong bearer"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cliActivity{Version: cliActivityVersion,
		CLIs: liveCLIs(s.cfg.ConfigDir, procRoot, s.knownCLIs())})
}
