package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
)

// THE MACHINE ROUTE THAT CREATES AN AGENT: POST /api/agents, and the CLI's
// `muster agent create` (docs/design/chief-dispatch.md, section 4 and phase 2).
//
// It replaced the web UI's dispatch form (the sidebar "+", the home page's
// "Dispatch an agent", a task card's Dispatch, and "Save for later"), which
// phase 2 deleted. Without it, the chief would be the only way to create an
// agent, and several kinds of deployment have no chief that can: a user not in
// MUSTER_CHIEF_DISPATCH_OWNERS, a deployment without task routing, a chief that
// runs as a gateway, and one whose KickoffUndeliverableReason refuses every
// agent_dispatch (where "create stopped, then Start" still works).
//
// 🔴 WHO MAY CALL IT: A PERSONAL TOKEN OF SCOPE agent-create, AND NOTHING
// ELSE. The door (admitAPIToken, explicitScopeRoutes) refuses every other
// scope — a FULL token included, so the CLI's ordinary token cannot create
// agents — and this handler refuses every principal that did not come through
// a personal token: the shared hook secret (ViaHook, which acts as user 1
// whoever holds it) above all. The owner mints the token deliberately, at
// /account/tokens (Create agents).
//
// ⚠ NO CAPS AND NO BUDGET, like the form it replaced: it is the owner acting
// with their own credential, not the chief. The form's checks run unchanged,
// in createAndDispatchAgent: kind and account, worker pool, connectedRepo, the
// model credential; and here, a gated note_id and KickoffUndeliverableReason
// when start is true.

// agentCreateRequest is POST /api/agents' body: the deleted form's fields as
// JSON. Start true dispatches (the form's Dispatch); false creates the agent
// stopped, provisioning nothing (what "Save for later" did) — Start on its page,
// POST /agents/{id}/start, brings it up later.
type agentCreateRequest struct {
	Kind       string `json:"kind"`
	Repo       string `json:"repo"`
	RepoBranch string `json:"repo_branch"`
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	NoteID     *int64 `json:"note_id"`
	CCAccount  string `json:"cc_account"`
	CCFailover bool   `json:"cc_failover"`
	WorkerPool string `json:"worker_pool"`
	Start      *bool  `json:"start"`
}

// agentCreateMaxBody bounds the request body. The prompt is the only free text.
const agentCreateMaxBody = 64 << 10

// AgentCreateViaRefusal is the 403 body for any principal on POST /api/agents
// that did not come through a personal token: the shared hook secret, a
// service credential, anything else.
const AgentCreateViaRefusal = "POST /api/agents takes only a personal API token of scope agent-create " +
	"(mint one at " + accountTokensPath + ", Create agents); the shared hook secret and service credentials cannot create agents"

// AgentCreateStartRequired is the 400 body for a request that does not say
// whether to start the agent: the two outcomes differ too much to default.
const AgentCreateStartRequired = `"start" is required: true dispatches the agent now, false creates it stopped (Start it later from its page)`

// handleAPIAgentCreate serves POST /api/agents. See the file header.
func (s *Server) handleAPIAgentCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if p, ok := authz.From(ctx); !ok || p.Via != authz.ViaToken {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": AgentCreateViaRefusal})
		return
	}
	var req agentCreateRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, agentCreateMaxBody+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if req.Start == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": AgentCreateStartRequired})
		return
	}
	start := *req.Start
	// The form's pre-flight, unchanged: a dispatch this deployment cannot
	// deliver is refused BEFORE the row, with the provisioner's own reason; a
	// stopped create asks for nothing it cannot do (handleAgentStart's remedy).
	if start {
		if reason := s.ext.Provisioner.KickoffUndeliverableReason(); reason != "" {
			s.writeJSON(w, http.StatusConflict, map[string]any{"error": reason})
			return
		}
	}
	noteText := strings.TrimSpace(req.Prompt)
	var noteID *int64
	if req.NoteID != nil {
		if s.ext.Notes == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "this server has no task store, so note_id names nothing"})
			return
		}
		// Read as the caller (row-level security): another user's task is "not
		// found", exactly like a task that does not exist.
		n, err := s.ext.Notes.Get(ctx, *req.NoteID)
		if err != nil {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such task"})
			return
		}
		// A `gate:<reason>` tag means the task is not dispatchable yet; a
		// stopped create is still allowed (it provisions nothing).
		if reason, gated := notes.GateReason(n.Tags); gated && start {
			s.writeJSON(w, http.StatusConflict, map[string]any{"error": "task is gated (" + reason + ") — remove the gate: tag to dispatch"})
			return
		}
		noteText = n.Body
		id := *req.NoteID
		noteID = &id
	}
	rec, err := s.createAndDispatchAgent(ctx, dispatchParams{
		Repo: strings.TrimSpace(req.Repo), RepoBranch: strings.TrimSpace(req.RepoBranch), Model: strings.TrimSpace(req.Model),
		NoteID: noteID, NoteText: noteText, Kickoff: start, TrackOutcome: true,
		Kind: strings.TrimSpace(req.Kind), CCAccount: strings.TrimSpace(req.CCAccount), CCFailover: req.CCFailover,
		WorkerPool: strings.TrimSpace(req.WorkerPool),
	})
	if err != nil {
		s.logger.Printf("agents: api create: %v", err)
		switch {
		case errors.Is(err, ErrDispatchRefused):
			s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		case errors.Is(err, agents.ErrModelNotCredentialed):
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		default:
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not create agent"})
		}
		return
	}
	// A dispatch on a task advances it open→in_progress, as the form's did.
	if start && noteID != nil {
		s.advanceTaskToInProgress(ctx, *noteID)
	}
	out := newAgentJSON(rec, time.Time{})
	s.writeJSON(w, http.StatusCreated, map[string]any{"agent": out, "started": start, "href": s.agentPageHref(ctx, rec)})
}
