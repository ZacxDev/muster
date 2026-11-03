package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
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
	mux.HandleFunc(LLMMessagesRoute, s.handleLLMMessages)
	mux.HandleFunc(llmPathPrefix, s.handleLLMOtherPath)
}

// handleLLMOtherPath refuses every /llm/ request but POST /llm/v1/messages.
func (s *Server) handleLLMOtherPath(w http.ResponseWriter, r *http.Request) {
	metrics.OfficerLLMRefused.WithLabelValues("none", "path").Inc()
	s.logger.Printf("llm proxy: refused %s on a path it does not serve (only %s)", r.Method, LLMMessagesRoute)
	llmproxy.Refuse(w, http.StatusNotFound, "not_found_error", "only POST /llm/v1/messages is served here")
}

// handleLLMMessages is POST /llm/v1/messages: the calling agent is
// authenticated by its own token (authenticateCallingAgent, the connector
// surface's identity check, which refuses the shared hook and service
// tokens), and the request acts as its owner — the officer's owner, whose the
// usage and audit rows are (row-level security).
func (s *Server) handleLLMMessages(w http.ResponseWriter, r *http.Request) {
	if s.ext.Agents == nil {
		metrics.OfficerLLMRefused.WithLabelValues("none", "unconfigured").Inc()
		llmproxy.Refuse(w, http.StatusServiceUnavailable, "api_error", "no officer on this server declares `llm`")
		return
	}
	a, ok := s.authenticateCallingAgent(w, r)
	if !ok {
		return
	}
	p := s.principalFor(r.Context(), agentOwnerID(a), authz.ViaConnect)
	s.admit(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)), s.serveLLMMessages, p)
}

// serveLLMMessages is handleLLMMessages once the agent is admitted: it must
// be a routine instance of an officer that declares `llm`.
func (s *Server) serveLLMMessages(w http.ResponseWriter, r *http.Request) {
	a, _ := r.Context().Value(agentCtxKey{}).(agents.Agent)
	if s.officerRT == nil || s.officerRT.llm == nil {
		metrics.OfficerLLMRefused.WithLabelValues("none", "unconfigured").Inc()
		llmproxy.Refuse(w, http.StatusServiceUnavailable, "api_error", "no officer on this server declares `llm`")
		return
	}
	o, n, isOfficer := s.officerOf(a)
	if !isOfficer || !officers.RoutineAgent(o, a.Name) {
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
	s.officerRT.llm.ServeMessages(w, r, llmproxy.Caller{Officer: o.ID, Instance: n, AgentID: a.ID, AgentName: a.Name,
		Model: o.LLM.Model, DailyBudgetUSD: o.LLM.DailyBudgetUSD, MaxTokensPerItem: o.LLM.MaxTokensPerItem, Key: o.LLM.Key()})
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
	host := ""
	if u, err := url.Parse(cfg.LLMUpstream); err == nil && u.Host != "" {
		host = u.Host
	} else {
		u, _ := url.Parse(llmproxy.DefaultUpstream)
		host = u.Host
	}
	audit := func(ctx context.Context, rec llmproxy.Record) {
		if s.ext.Connectors == nil {
			return
		}
		if err := s.ext.Connectors.Audit(ctx, connectors.AuditEntry{Kind: "llm", AgentID: rec.AgentID, AgentName: rec.AgentName,
			Connector: "llm", Method: http.MethodPost, Host: host, Path: "/api" + llmproxy.UpstreamPath, Status: rec.Status,
			Detail: rec.Detail()}); err != nil {
			s.logger.Printf("llm proxy: audit for agent %q: %v", rec.AgentName, err)
		}
	}
	px, err := llmproxy.New(llmproxy.Config{Upstream: cfg.LLMUpstream, HTTP: cfg.LLMHTTP, Store: cfg.LLMStore, Audit: audit,
		Logger: s.logger, Now: cfg.Now})
	if err != nil {
		return nil, err
	}
	for _, o := range on {
		metrics.OfficerLLMBudget.WithLabelValues(o.ID).Set(o.LLM.DailyBudgetUSD)
		s.logger.Printf("officers: %s may reach a model through the LLM proxy: %s", o.ID, o.LLM.String())
	}
	return px, nil
}

// llmClaimed and llmEnded tell the proxy's per-item meter about an officer
// tool's claim, complete or release by instance (an agent name).
func (s *Server) llmClaimed(o officers.Officer, instance string, item officers.ItemID) {
	if s.officerRT == nil || s.officerRT.llm == nil || o.LLM == nil {
		return
	}
	if io, n, ok := s.officerRT.reg.ForAgentName(instance); ok && io.ID == o.ID {
		s.officerRT.llm.Claimed(o.ID, n, string(item))
	}
}

func (s *Server) llmEnded(o officers.Officer, instance string, item officers.ItemID) {
	if s.officerRT == nil || s.officerRT.llm == nil || o.LLM == nil {
		return
	}
	if io, n, ok := s.officerRT.reg.ForAgentName(instance); ok && io.ID == o.ID {
		s.officerRT.llm.Ended(o.ID, n, string(item))
	}
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
			if io, n, ok := reg.ForAgentName(it.ClaimedBy); ok && io.ID == o.ID {
				holding[n] = append(holding[n], string(it.ID))
			}
		}
		px.Sync(o.ID, holding, at)
	}
}
