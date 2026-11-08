package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/officers"
)

// The officer LLM proxy's doors (internal/llmproxy has the proxy itself;
// docs/officers.md "The LLM proxy").
//
// A routine officer instance's Claude Code runs with ANTHROPIC_BASE_URL =
// $MUSTER_CONNECT_URL/llm and its own muster token as ANTHROPIC_AUTH_TOKEN,
// so it calls POST /llm/v1/messages on the connect listener, the one port its
// pod may reach. muster authenticates the instance, checks it is a routine
// instance of an officer that declares `llm`, and proxies the request with
// that officer's model, budget and key.
//
// 🔴 EXACTLY ONE PATH IS SERVED: POST /llm/v1/messages. It is the only path
// Claude Code was measured to call against its base URL (no count_tokens, no
// model list). Every other method or path under /llm/ answers 404, before any
// authentication, and is counted as refused (reason path).

// llmPathPrefix is the proxy's mount point on the connect surface.
const llmPathPrefix = "/llm/"

// LLMMessagesRoute is the one route the proxy serves.
const LLMMessagesRoute = "POST /llm/v1/messages"

func (s *Server) registerLLMRoutes(mux Mux) {
	mux.HandleFunc(LLMMessagesRoute, s.requireConnectorAgent(s.serveLLMMessages))
	mux.HandleFunc(llmPathPrefix, s.handleLLMOtherPath)
}

// handleLLMOtherPath refuses every /llm/ request but POST /llm/v1/messages.
func (s *Server) handleLLMOtherPath(w http.ResponseWriter, r *http.Request) {
	metrics.OfficerLLMRefused.WithLabelValues("none", "path").Inc()
	s.logger.Printf("llm proxy: refused %s on a path it does not serve (only %s)", r.Method, LLMMessagesRoute)
	llmproxy.Refuse(w, http.StatusNotFound, "not_found_error", "only POST /llm/v1/messages is served here")
}

// LLM requests are admitted by requireConnectorAgent, the connector
// surface's own door: the calling agent is resolved from its own token (the
// shared hook and service tokens are refused), and the request acts as its
// owner — the officer's owner, whose the usage and audit rows are (row-level
// security). One door, so the proxy authenticates exactly as /connect/ does.

// serveLLMMessages is POST /llm/v1/messages once the agent is admitted: it
// must be a routine instance of an officer that declares `llm`.
//
// Or a chief, when EnableChiefLLM was called (chief_llm.go, which serves only
// the admin's). Without it the chief check is skipped and a chief is refused
// below exactly as any other agent that is not a routine instance.
//
// A routine instance (decided from its name alone) is never asked whether it is
// the chief, so an officer's request costs no agents-list read. Any other agent
// is, and when that read fails the request is refused 503 with that cause: it
// is not passed on to the officer checks, whose refusal would name the wrong
// fault.
func (s *Server) serveLLMMessages(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	o, n, isOfficer := s.officerOf(a)
	routine := isOfficer && officers.RoutineAgent(o, a.Name)
	if s.chiefLLM != nil && !routine {
		_, chief, err := s.agentIsChief(r.Context(), a.ID)
		if err != nil {
			metrics.OfficerLLMRefused.WithLabelValues("none", "agents_read").Inc()
			s.logger.Printf("llm proxy: refused agent %d (%q): could not read the agents list to decide whether it is the chief "+
				"(nothing sent): %v", a.ID, a.Name, err)
			llmproxy.Refuse(w, http.StatusServiceUnavailable, "api_error", ChiefLLMAgentsReadRefusal)
			return
		}
		if chief {
			s.serveChiefLLM(w, r, a)
			return
		}
	}
	if s.officerRT == nil || s.officerRT.llm == nil {
		metrics.OfficerLLMRefused.WithLabelValues("none", "unconfigured").Inc()
		llmproxy.Refuse(w, http.StatusServiceUnavailable, "api_error", "no officer on this server declares `llm`")
		return
	}
	if !routine {
		label := "none"
		if isOfficer && o.LLM != nil {
			label = o.ID
		}
		metrics.OfficerLLMRefused.WithLabelValues(label, "not_routine").Inc()
		s.logger.Printf("llm proxy: refused agent %d (%q): not a routine instance of an officer that declares `llm`", a.ID, a.Name)
		llmproxy.Refuse(w, http.StatusForbidden, "permission_error",
			"this agent is not a routine-tier instance of an officer that declares `llm`")
		return
	}
	// THE model pin is the routine pool's: tiers.routine.model.
	p := o.Pool(officers.TierRoutine)
	s.officerRT.llm.ServeMessages(w, r, llmproxy.Caller{Officer: o.ID, Instance: n, InstanceLabel: p.InstanceLabel(n),
		AgentID: a.ID, AgentName: a.Name, Model: p.Model, DailyBudgetUSD: o.LLM.DailyBudgetUSD,
		MaxTokensPerItem: o.LLM.MaxTokensPerItem, Key: o.LLM.Key()})
}

// newLLMProxy builds the proxy when a declared officer has an `llm` block,
// and nil otherwise. A declared `llm` with no usage store is a boot refusal:
// a budget that a restart resets is not a budget.
func (s *Server) newLLMProxy(cfg OfficersConfig) (*llmproxy.Proxy, error) {
	var on []officers.Officer
	for _, o := range cfg.Officers {
		if o.LLM != nil {
			on = append(on, o)
		}
	}
	if len(on) == 0 {
		return nil, nil
	}
	if cfg.LLMStore == nil {
		return nil, errors.New("officers: an officer declares `llm`, but there is no usage store to keep its budget in")
	}
	// The proxy's door is requireConnectorAgent, which answers 503 to every
	// request without a connector store, and the audit rows go to that store.
	if s.ext.Connectors == nil {
		ids := make([]string, 0, len(on))
		for _, o := range on {
			ids = append(ids, o.ID)
		}
		return nil, fmt.Errorf("officers: %s declare `llm`, but this server has no connector store: the LLM proxy "+
			"authenticates instances through the connector door and audits into its store", strings.Join(ids, ", "))
	}
	px, err := llmproxy.New(llmproxy.Config{Upstream: cfg.LLMUpstream, HTTP: cfg.LLMHTTP, Store: cfg.LLMStore,
		Audit: s.llmAudit(cfg.LLMUpstream), Logger: s.logger, Now: cfg.Now})
	if err != nil {
		return nil, err
	}
	for _, o := range on {
		metrics.OfficerLLMBudget.WithLabelValues(o.ID).Set(o.LLM.DailyBudgetUSD)
		s.logger.Printf("officers: %s may reach a model through the LLM proxy: %s", o.ID, o.LLM.String())
	}
	return px, nil
}

// llmAudit is the proxy's audit sink: one connector_audit row (kind llm) per
// request, its host the provider's (upstream, or llmproxy.DefaultUpstream
// when ""). The officer and the chief proxies share it.
func (s *Server) llmAudit(upstream string) func(ctx context.Context, rec llmproxy.Record) {
	host := ""
	if u, err := url.Parse(upstream); err == nil && u.Host != "" {
		host = u.Host
	} else {
		u, _ := url.Parse(llmproxy.DefaultUpstream)
		host = u.Host
	}
	return func(ctx context.Context, rec llmproxy.Record) {
		if s.ext.Connectors == nil {
			return
		}
		if err := s.ext.Connectors.Audit(ctx, connectors.AuditEntry{Kind: "llm", AgentID: rec.AgentID, AgentName: rec.AgentName,
			Connector: "llm", Method: http.MethodPost, Host: host, Path: "/api" + llmproxy.UpstreamPath, Status: rec.Status,
			Detail: rec.Detail()}); err != nil {
			s.logger.Printf("llm proxy: audit for agent %q: %v", rec.AgentName, err)
		}
	}
}

// llmClaimed and llmEnded tell the proxy's per-item meter about an officer
// tool's claim, complete or release by instance (an agent name).
//
// claimBody is HQ's answer to the claim (or muster's already-yours answer):
// its item's `attempts` tells the meter whether this is HQ's first claim of
// the item, the only hold whose cost can be the item's whole (claimAttempts).
func (s *Server) llmClaimed(o officers.Officer, instance string, item officers.ItemID, claimBody []byte) {
	if s.officerRT == nil || s.officerRT.llm == nil || o.LLM == nil {
		return
	}
	if n, ok := s.llmRoutineInstance(o, instance); ok {
		s.officerRT.llm.ClaimedAttempt(o.ID, n, string(item), claimAttempts(claimBody))
	}
}

// claimAttempts is the `attempts` of the item in a claim answer: 0 when it
// has none (an HQ without the field) or is muster's already-yours answer
// (officerClaim: a heartbeat of a live hold, not a new HQ claim).
func claimAttempts(body []byte) int {
	var b struct {
		AlreadyYours bool `json:"already_yours"`
		Item         *struct {
			Attempts int `json:"attempts"`
		} `json:"item"`
	}
	if json.Unmarshal(body, &b) != nil || b.AlreadyYours || b.Item == nil {
		return 0
	}
	return b.Item.Attempts
}

// llmItemCost is the cost a routine instance's complete of item reports to
// HQ (civ-hq docs/OFFICERS.md "Item cost"): what its hold booked through the
// proxy, from muster:llm-proxy, or nil when that is not known to be the
// item's whole cost — then NOTHING is sent, never a 0. Only a complete that
// finishes the item carries one: needs_judgement hands it on to a judgement
// session, whose cost muster has no source for, so the item's total is not
// known. A judgement or untiered instance's Claude Code session has no cost
// source muster reads: nil.
func (s *Server) llmItemCost(o officers.Officer, instance string, item officers.ItemID, outcome string) *officers.ItemCost {
	if s.officerRT == nil || s.officerRT.llm == nil || o.LLM == nil || outcome == officers.OutcomeNeedsJudgement {
		return nil
	}
	n, ok := s.llmRoutineInstance(o, instance)
	if !ok {
		return nil
	}
	usd, known := s.officerRT.llm.ItemCost(o.ID, n, string(item))
	// HQ stores 0-10000 USD (above is a reporting error it refuses); muster
	// books to the micro-dollar (officer_llm_usage's NUMERIC(14,6)).
	usd = math.Round(usd*1e6) / 1e6
	if !known || math.IsNaN(usd) || usd < 0 || usd > officers.MaxItemCostUSD {
		return nil
	}
	return &officers.ItemCost{USD: usd, From: officers.CostFromLLMProxy}
}

func (s *Server) llmEnded(o officers.Officer, instance string, item officers.ItemID) {
	if s.officerRT == nil || s.officerRT.llm == nil || o.LLM == nil {
		return
	}
	if n, ok := s.llmRoutineInstance(o, instance); ok {
		s.officerRT.llm.Ended(o.ID, n, string(item))
	}
}

// llmRoutineInstance is the routine instance number of agent name, when it is
// one of o's routine pool (officers.RoutineAgent): the meter counts routine
// instances only, keyed by their number in that pool — a judgement instance
// with the same number is not the same instance.
func (s *Server) llmRoutineInstance(o officers.Officer, name string) (int, bool) {
	if !officers.RoutineAgent(o, name) {
		return 0, false
	}
	_, n, ok := s.officerRT.reg.ForAgentName(name)
	return n, ok
}

// llmSync is the wake loop's view of o's claimed items, for the per-item
// meter (officers.Config.Observed).
func llmSync(px *llmproxy.Proxy, reg *officers.Registry) func(o officers.Officer, claimed []officers.Item, at time.Time) {
	return func(o officers.Officer, claimed []officers.Item, at time.Time) {
		if o.LLM == nil {
			return
		}
		holding := map[int][]string{}
		for _, it := range claimed {
			if io, n, ok := reg.ForAgentName(it.ClaimedBy); ok && io.ID == o.ID && officers.RoutineAgent(io, it.ClaimedBy) {
				holding[n] = append(holding[n], string(it.ID))
			}
		}
		px.Sync(o.ID, holding, at)
	}
}
