package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/cctasks"
	"github.com/ZacxDev/muster/internal/procstat"
)

// LIVE WORK COUNTS (docs/design/agent-cards.md (a), "ccd: POST /agent/work"):
// how many subagents and background shells this pod's Claude Code CLIs — the
// primary and every lane — have running now, reported to muster for the
// agent's card, which draws the chip a mirrored window's card does.
//
// THE RULE IS THE MIRROR'S, cctasks.Decide, over the same three inputs:
//
//   - the CLI's RAW session status, from its own sessions file
//     (<CLAUDE_CONFIG_DIR>/sessions/<pid>.json — measured in a live pod on
//     2.1.296: "idle", with pid, sessionId, startedAt, procStart). With no live
//     file there is no Work: the CLI says nothing, so the card shows nothing.
//   - its background shells, from its process tree: cctasks.CountShells, the
//     mirror's own function — exact while nothing can run in the foreground,
//     "1+" while a turn may be running (operator decision 3).
//   - its running subagents, read only while the status is busy or waiting.
//
// 🔴 THE ONE DIFFERENCE IS WHERE THE SUBAGENTS COME FROM. The mirror reads them
// from the transcript (internal/mirrorhost/work.go's ledger, which lives in
// the host package and would bring the host's dependencies into the pod). ccd
// owns its CLIs' hook settings and already hears their hooks, so it keeps the
// set from them (trackSubagentsLocked):
//
//   - SubagentStart adds agent_id, SubagentStop removes it (payload fields read
//     from the 2.1.296 bundle in the image: `hook_event_name:"SubagentStart",
//     agent_id, agent_type`);
//   - Stop and SubagentStop carry `background_tasks`, the CLI's own registry of
//     the background tasks still running ({id, type, status}: type "subagent"
//     for an agent, whose id IS its agent_id — the bundle builds the task with
//     id = agentId). Each one REPLACES the set, so a missed start or stop is
//     corrected by the next one;
//   - an "idle" or "shell" status — the CLI saying no agent runs — empties it of
//     every agent learned before that status was written.
//
// So a missed hook over- or under-counts only while the CLI stays busy, and
// only until the next Stop, SubagentStop or idle; the mirror's ledger has the
// same window for a kill it does not see. ⚠ It counts a subagent's OWN
// subagents too (both sources include them); the mirror's ledger does not.
//
// STALE-SAFE: the report is re-sent every workHeartbeat while non-zero, and
// muster drops one it has not seen refreshed within its TTL
// (internal/api/agent_work.go) — a pod that died loses its chip. A CLI that
// exited leaves its sessions file behind, but not its process: a file is live
// only while its pid runs with the start time the file records.

// workInterval is how often ccd samples its CLIs' work; a hook that changes the
// subagent set samples at once (server.workPoke).
const workInterval = 5 * time.Second

// workHeartbeat is how often an unchanged non-zero report is re-sent.
const workHeartbeat = time.Minute

// maxTrackedSubagents bounds a CLI's subagent set: hooks are claims any tool
// call holding the hook secret can make, and this is memory they cannot grow.
const maxTrackedSubagents = 256

// subagentIDPattern bounds what an id may be (the measured form is "a" and 16
// hex; this does not insist on it, so a longer id in a later build still counts).
var subagentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// backgroundTask is one entry of a Stop / SubagentStop hook's background_tasks.
type backgroundTask struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	AgentType string `json:"agent_type"`
}

// isRunningSubagent: an agent task the CLI says is still running. A
// "main-session" agent is a backgrounded conversation, not a subagent.
func (t backgroundTask) isRunningSubagent() bool {
	return t.Type == "subagent" && (t.Status == "running" || t.Status == "pending") &&
		t.AgentType != "main-session" && subagentIDPattern.MatchString(t.ID)
}

// trackSubagentsLocked updates s.subagents from one hook. Caller holds s.mu.
func (s *server) trackSubagentsLocked(ev hookEvent) {
	// A hook naming another session is a `claude` some tool call ran inside this
	// one (proctree.go): its subagents are not this CLI's.
	own := ev.SessionID == "" || s.sessionID == "" || ev.SessionID == s.sessionID
	before := len(s.subagents)
	changed := false
	switch strings.ToLower(ev.Event) {
	case "sessionstart", "sessionend":
		changed = before > 0
		s.subagents = nil
	case "subagentstart":
		if own && subagentIDPattern.MatchString(ev.AgentID) {
			if s.subagents == nil {
				s.subagents = map[string]time.Time{}
			}
			if _, seen := s.subagents[ev.AgentID]; !seen && len(s.subagents) < maxTrackedSubagents {
				s.subagents[ev.AgentID] = s.clock()
				changed = true
			}
		}
	case "stop", "subagentstop":
		if !own {
			return
		}
		if ev.BackgroundTasks != nil {
			next := map[string]time.Time{}
			for _, t := range *ev.BackgroundTasks {
				if !t.isRunningSubagent() || len(next) >= maxTrackedSubagents {
					continue
				}
				at, known := s.subagents[t.ID]
				if !known {
					at = s.clock()
				}
				next[t.ID] = at
			}
			changed = !sameIDs(next, s.subagents)
			s.subagents = next
		}
		// The agent that stopped is no longer running, whether or not the
		// snapshot was taken before it was marked done.
		if _, ok := s.subagents[ev.AgentID]; ok && ev.AgentID != "" {
			delete(s.subagents, ev.AgentID)
			changed = true
		}
	}
	if changed && s.workPoke != nil {
		s.workPoke()
	}
}

func sameIDs(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// runningSubagents is how many subagents the CLI has running, by its hooks.
func (s *server) runningSubagents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subagents)
}

// forgetSubagentsBefore drops every subagent learned before t: the CLI wrote a
// status at t that says none was running then.
func (s *server) forgetSubagentsBefore(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, at := range s.subagents {
		if at.Before(t) {
			delete(s.subagents, id)
		}
	}
}

// currentSession is the session id the CLI's last SessionStart named.
func (s *server) currentSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sessionStarted {
		return ""
	}
	return s.sessionID
}

// cliSession is a sessions file, the fields read.
type cliSession struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
	ProcStart       string `json:"procStart"`
}

// procTable is the pod's processes: each one's argv, and each one's children.
type procTable struct {
	argv     map[int][]string
	children map[int][]int
}

func readProcTable(root string) procTable {
	t := procTable{argv: map[int][]string{}, children: map[int][]int{}}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			continue
		}
		st, ok := procstat.Parse(b)
		if !ok {
			continue
		}
		t.children[st.PPID] = append(t.children[st.PPID], pid)
		if cmd, err := os.ReadFile(filepath.Join(root, e.Name(), "cmdline")); err == nil {
			for _, a := range bytes.Split(bytes.TrimRight(cmd, "\x00"), []byte{0}) {
				t.argv[pid] = append(t.argv[pid], string(a))
			}
		}
	}
	for _, c := range t.children {
		sort.Ints(c)
	}
	return t
}

// liveSessions is every sessions file under configDir whose process is alive —
// its pid runs, with the start time the file records — by session id.
func liveSessions(configDir, root string) map[string]cliSession {
	out := map[string]cliSession{}
	files, _ := filepath.Glob(filepath.Join(configDir, "sessions", "*.json"))
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cs cliSession
		if json.Unmarshal(b, &cs) != nil || cs.PID <= 1 || cs.SessionID == "" {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(root, strconv.Itoa(cs.PID), "stat"))
		if err != nil {
			continue
		}
		st, ok := procstat.Parse(stat)
		if !ok || st.State == "Z" || (cs.ProcStart != "" && cs.ProcStart != st.Start) {
			continue
		}
		if prev, dup := out[cs.SessionID]; dup && prev.PID < cs.PID {
			continue // two live files for one session: the lower pid, deterministically
		}
		out[cs.SessionID] = cs
	}
	return out
}

// podWork is the work of every CLI in servers, summed: subagents and shells
// added, a lower bound if any is one. ok is false when no CLI has a live
// sessions file to read a status from — nothing may be claimed.
func podWork(configDir, root string, servers []*server) (cctasks.Work, bool) {
	sessions := liveSessions(configDir, root)
	var tbl *procTable
	var sum cctasks.Work
	any := false
	for _, s := range servers {
		cs, ok := sessions[s.currentSession()]
		if !ok {
			continue
		}
		var sh cctasks.Shells
		switch cs.Status {
		case cctasks.StatusShell, cctasks.StatusBusy, cctasks.StatusWaiting:
			if tbl == nil {
				t := readProcTable(root)
				tbl = &t
			}
			sh = cctasks.CountShells(root, cs.PID, tbl.children, func(pid int) []string { return tbl.argv[pid] }, configDir)
		case cctasks.StatusIdle:
		default:
			continue
		}
		if cs.Status == cctasks.StatusIdle || cs.Status == cctasks.StatusShell {
			// The CLI says no agent runs: what the hooks still hold from before
			// that is a stop ccd never heard.
			if cs.StatusUpdatedAt > 0 {
				s.forgetSubagentsBefore(time.UnixMilli(cs.StatusUpdatedAt))
			}
		}
		w, ok := cctasks.Decide(cs.Status, sh, s.runningSubagents)
		if !ok {
			continue
		}
		any = true
		sum.Subagents += w.Subagents
		sum.Shells += w.Shells
		sum.ShellsAtLeast = sum.ShellsAtLeast || w.ShellsAtLeast
	}
	return sum, any
}

// workServers is every CLI's server: the primary's and each running lane's.
func (s *server) workServers() []*server {
	out := []*server{s}
	if s.lanes == nil {
		return out
	}
	s.lanes.mu.Lock()
	defer s.lanes.mu.Unlock()
	ids := make([]string, 0, len(s.lanes.set))
	for id := range s.lanes.set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, s.lanes.set[id].srv)
	}
	return out
}

// workReporter samples the pod's work and posts it to muster (POST /agent/work,
// the agent's own token, like /agent/blocked) when it changes, and every
// heartbeat while it is non-zero.
type workReporter struct {
	base, token string
	client      *http.Client
	interval    time.Duration
	heartbeat   time.Duration
	sample      func() cctasks.Work
	poke        chan struct{} // capacity 1
	now         func() time.Time
	logf        func(format string, args ...any)
}

func newWorkReporter(base, token string, sample func() cctasks.Work, logf func(string, ...any)) *workReporter {
	return &workReporter{
		base: strings.TrimRight(base, "/"), token: token,
		client:   &http.Client{Timeout: 5 * time.Second, CheckRedirect: noRedirects.CheckRedirect},
		interval: workInterval, heartbeat: workHeartbeat,
		sample: sample, poke: make(chan struct{}, 1), now: time.Now, logf: logf,
	}
}

// pokeNow asks for a sample now, without blocking.
func (r *workReporter) pokeNow() {
	select {
	case r.poke <- struct{}{}:
	default:
	}
}

func (r *workReporter) run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	var last cctasks.Work
	var sent time.Time
	failing := false
	for {
		w := r.sample()
		if w != last || (w != (cctasks.Work{}) && r.now().Sub(sent) >= r.heartbeat) {
			if err := r.post(ctx, w); err != nil {
				if !failing {
					r.logf("ccd: reporting work %+v to muster: %v", w, err)
				}
				failing = true // retried at the next sample; logged once
			} else {
				last, sent, failing = w, r.now(), false
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.poke:
		}
	}
}

// workBody is POST /agent/work's body: internal/mirror.WindowWork's JSON.
type workBody struct {
	Subagents     int  `json:"subagents,omitempty"`
	Shells        int  `json:"shells,omitempty"`
	ShellsAtLeast bool `json:"shellsAtLeast,omitempty"`
}

func (r *workReporter) post(ctx context.Context, w cctasks.Work) error {
	b, _ := json.Marshal(workBody{Subagents: w.Subagents, Shells: w.Shells, ShellsAtLeast: w.ShellsAtLeast})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/agent/work", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Muster-Token", r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &httpStatusError{resp.StatusCode}
	}
	return nil
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return "status " + strconv.Itoa(e.code) }
