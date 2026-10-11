package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE APPROVAL RELAY: a claude-code pod's permission prompts, routed to the
// operator's phone THROUGH muster.
//
// 🔴 WHY THE POD DOES NOT TALK TO THE PERMISSION ROUTER ITSELF. The router has
// no credential an agent pod can safely hold. Its shared hook token
// is one value whose tier also READS every
// session's transcript and every tmux pane, resolves anyone's attention entries,
// deletes anyone's pending request by id and sends arbitrary push
// notifications; its terminal and chief tokens type into terminals; its service
// token is muster's own. A shell-capable agent holding any of them could read or
// interfere with the operator's other sessions. So the pod authenticates to
// MUSTER with its own per-agent token (the connector surface's credential, with
// the shared tokens refused by name), and muster files the request through the
// gate it already uses for checkpoints (gate.go, the service tier).
//
// WHAT AN AGENT CAN DO HERE, AND NOTHING ELSE:
//   - file a request (POST /agent/approvals). The card's Host and Project are
//     muster's, derived from the agent's identity, never the caller's, so a card
//     always names the agent that asked;
//   - read or clear a request IT filed (GET/DELETE /agent/approvals/{id}). Any
//     other id answers 404, whoever filed it;
//   - report that its terminal is blocked on a permission prompt
//     (POST /agent/blocked), which only drives this agent's own badge.
// There is no route that DECIDES. A decision exists only on the router's human
// tier.
//
// ⚠ THE CARD IS A GATE TYPE (checkpoint), WHICH THE ROUTER EXEMPTS FROM EVERY
// AUTO-APPROVAL PATH. That is deliberate: the operator's decision for these pods
// is "default permissions + approvals, never bypass", and an auto-approve window
// opened for a laptop project must not approve a pod's tool call.
//
// ⚠ THE LEDGER IS IN MEMORY. A muster restart forgets which agent filed which
// request; the pod's next poll then answers 404 and its hook falls back to the
// in-terminal prompt. That is the safe direction: no decision is ever invented.
// ---------------------------------------------------------------------------

const (
	// approvalOwnerTTL bounds how long a filed request stays readable by its
	// agent. The pod's hook gives up after ~170s; this only has to outlive it.
	approvalOwnerTTL = 10 * time.Minute
	// approvalMaxOutstanding caps the requests one agent may have open at once,
	// so a looping agent cannot flood the operator's phone.
	approvalMaxOutstanding = 4
	// blockedReportTTL is how long a "blocked on a prompt" report counts without
	// a refresh. ccd re-reports every minute while blocked, so a pod that died
	// blocked stops showing the badge within this window.
	blockedReportTTL = 3 * time.Minute
	// approvalTextCap bounds each free-text field the agent supplies.
	approvalTextCap = 2000
	// approvalContextCap bounds the context lines the agent supplies.
	approvalContextCap = 12
)

// approvalLedger is the relay's in-memory state.
type approvalLedger struct {
	mu sync.Mutex
	// owners maps a filed request id to the agent that filed it.
	owners map[string]approvalOwner
	// blocked maps an agent id to its latest "blocked on a prompt" report.
	blocked map[int64]blockedReport
}

type approvalOwner struct {
	agentID int64
	filed   time.Time
}

type blockedReport struct {
	since, seen time.Time
}

func (l *approvalLedger) init() {
	if l.owners == nil {
		l.owners = map[string]approvalOwner{}
	}
	if l.blocked == nil {
		l.blocked = map[int64]blockedReport{}
	}
}

// pruneLocked drops expired owner entries and stale blocked reports.
func (l *approvalLedger) pruneLocked(now time.Time) {
	for id, o := range l.owners {
		if now.Sub(o.filed) > approvalOwnerTTL {
			delete(l.owners, id)
		}
	}
	for id, b := range l.blocked {
		if now.Sub(b.seen) > blockedReportTTL {
			delete(l.blocked, id)
		}
	}
}

// AwaitingApproval reports whether an agent is waiting on a human: it has a
// request filed through the relay that is still open, or its terminal reported
// a permission prompt recently. It drives the agent's "awaiting approval" badge.
func (s *Server) AwaitingApproval(agentID int64) bool {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	l.pruneLocked(s.now())
	if _, ok := l.blocked[agentID]; ok {
		return true
	}
	for _, o := range l.owners {
		if o.agentID == agentID {
			return true
		}
	}
	return false
}

func (s *Server) registerApprovalRoutes(mux Mux) {
	mux.HandleFunc("POST /agent/approvals", s.requireRelayAgent(s.handleApprovalCreate))
	mux.HandleFunc("GET /agent/approvals/{id}", s.requireRelayAgent(s.handleApprovalGet))
	mux.HandleFunc("DELETE /agent/approvals/{id}", s.requireRelayAgent(s.handleApprovalDelete))
	mux.HandleFunc("POST /agent/blocked", s.requireRelayAgent(s.handleAgentBlocked))
}

// requireRelayAgent authenticates the calling agent by its own token, exactly as
// the connector surface does (authenticateCallingAgent), without requiring the
// connector store to be configured.
func (s *Server) requireRelayAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Agents == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "this server has no agent store", HookUnarmedField: true,
			})
			return
		}
		a, ok := s.authenticateCallingAgent(w, r)
		if !ok {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
	}
}

// ApprovalRequest is the POST /agent/approvals body the pod's hook sends.
type ApprovalRequest struct {
	Tool    string   `json:"tool"`
	Command string   `json:"command"`
	Input   string   `json:"input"`
	Cwd     string   `json:"cwd"`
	Session string   `json:"session"`
	Context []string `json:"context"`
}

// ApprovalCardHost is the Host a relayed card carries: muster's, naming the
// agent. The caller cannot choose it.
func ApprovalCardHost(a agents.Agent) string { return "muster-agent/" + a.Name }

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func (s *Server) handleApprovalCreate(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	var body ApprovalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	tool := clip(body.Tool, 200)
	if tool == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "`tool` is required"})
		return
	}
	cmd := clip(body.Command, approvalTextCap)
	if cmd == "" {
		cmd = clip(body.Input, approvalTextCap)
	}
	var ctxLines []string
	for _, l := range body.Context {
		if len(ctxLines) == approvalContextCap {
			break
		}
		if l = clip(l, 500); l != "" {
			ctxLines = append(ctxLines, l)
		}
	}
	g := s.gate()
	if g == nil {
		// No approval queue: the hook falls back to the in-terminal prompt.
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": errNoGate.Error(), HookUnarmedField: true})
		return
	}

	l := &s.approvals
	now := s.now()
	l.mu.Lock()
	l.init()
	l.pruneLocked(now)
	open := 0
	for _, o := range l.owners {
		if o.agentID == a.ID {
			open++
		}
	}
	l.mu.Unlock()
	if open >= approvalMaxOutstanding {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "this agent already has the maximum number of open approval requests",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	id, err := g.MintGate(ctx, GateSpec{
		Type:    gateTypeCheckpoint,
		Tool:    tool,
		Command: cmd,
		Host:    ApprovalCardHost(a),
		Project: a.Name,
		Cwd:     clip(body.Cwd, 500),
		Session: clip(body.Session, 200),
		Context: ctxLines,
	})
	if err != nil {
		s.logger.Printf("approvals: %s: filing a card: %v", a.Name, err)
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the approval queue could not be reached"})
		return
	}
	l.mu.Lock()
	l.init()
	l.owners[id] = approvalOwner{agentID: a.ID, filed: now}
	l.mu.Unlock()
	s.logger.Printf("approvals: %s filed %s tool=%q", a.Name, id, tool)
	s.writeJSON(w, http.StatusCreated, map[string]any{"requestId": id})
}

// ownedBy reports whether request id was filed by agent a and is still tracked.
func (s *Server) ownedBy(id string, a agents.Agent) bool {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	l.pruneLocked(s.now())
	o, ok := l.owners[id]
	return ok && o.agentID == a.ID
}

func (s *Server) forgetApproval(id string) {
	l := &s.approvals
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	delete(l.owners, id)
}

// errNotYours is the single refusal for an id this agent did not file, whether
// it exists or not: answering differently would tell an agent which ids exist.
var errNotYours = errors.New("no such approval request for this agent")

func (s *Server) handleApprovalGet(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id := r.PathValue("id")
	if !s.ownedBy(id, a) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": errNotYours.Error()})
		return
	}
	g := s.gate()
	if g == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": errNoGate.Error()})
		return
	}
	d, err := g.ReadGate(r.Context(), id)
	if err != nil {
		// A failed read is NOT a state. The hook keeps polling until its own
		// deadline and then falls back to the terminal.
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the approval queue could not be read"})
		return
	}
	switch d.State {
	case GateStateDecided, GateStateGone:
		s.forgetApproval(id)
	}
	out := map[string]any{"state": d.State}
	if d.State == GateStateDecided {
		out["response"] = d.Response
		if d.Comment != "" {
			out["comment"] = d.Comment
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleApprovalDelete(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id := r.PathValue("id")
	if !s.ownedBy(id, a) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": errNotYours.Error()})
		return
	}
	s.forgetApproval(id)
	s.clearGateRequest(r.Context(), id)
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// blockedBody is the POST /agent/blocked body ccd sends.
type blockedBody struct {
	Blocked bool `json:"blocked"`
}

func (s *Server) handleAgentBlocked(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	var body blockedBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	l := &s.approvals
	now := s.now()
	l.mu.Lock()
	l.init()
	prev, was := l.blocked[a.ID]
	if body.Blocked {
		since := now
		if was {
			since = prev.since
		}
		l.blocked[a.ID] = blockedReport{since: since, seen: now}
	} else {
		delete(l.blocked, a.ID)
	}
	l.mu.Unlock()
	if was != body.Blocked {
		s.broadcast(EventAgentChanged, a.Name)
	}
	w.WriteHeader(http.StatusNoContent)
}
