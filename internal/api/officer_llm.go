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
	// proxied: a routine pool's instance beside the officer's `llm`, or a
	// judgement pool's beside tiers.judgement.llm (officers.ProxyAgent, the one
	// membership rule; it answers the pool, whose model and budget apply).
	pool, _, proxied := officers.ProxyAgent(o, a.Name)
	proxied = proxied && isOfficer
	if s.chiefLLM != nil && !proxied {
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
	if !proxied {
		label := "none"
		if isOfficer && o.UsesLLMProxy() {
			label = o.ID
		}
		metrics.OfficerLLMRefused.WithLabelValues(label, "not_routine").Inc()
		s.logger.Printf("llm proxy: refused agent %d (%q): not an instance of an officer pool that declares `llm`", a.ID, a.Name)
		llmproxy.Refuse(w, http.StatusForbidden, "permission_error",
			"this agent is not an instance of an officer pool on the LLM proxy (a routine pool beside `llm`, or a "+
				"judgement pool with tiers.judgement.llm)")
		return
	}
	// THE model pin and the budget are the POOL's: tiers.<pool>.model and
	// its `llm` block (Officer.PoolLLM).
	l := pool.PoolLLM()
	s.officerRT.llm.ServeMessages(w, r, llmproxy.Caller{Officer: o.ID, Pool: pool.Tier, Instance: n, InstanceLabel: pool.InstanceLabel(n),
		AgentID: a.ID, AgentName: a.Name, Model: pool.Model, DailyBudgetUSD: l.DailyBudgetUSD,
		MaxTokensPerItem: l.MaxTokensPerItem, Key: l.Key()})
}

// newLLMProxy builds the proxy when a declared officer has an `llm` block,
// and nil otherwise. A declared `llm` with no usage store is a boot refusal:
// a budget that a restart resets is not a budget.
func (s *Server) newLLMProxy(cfg OfficersConfig) (*llmproxy.Proxy, error) {
	var on []officers.Officer
	for _, o := range cfg.Officers {
		if o.UsesLLMProxy() {
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
		for _, p := range o.Pools() {
			if l := p.PoolLLM(); l != nil {
				metrics.OfficerLLMBudget.WithLabelValues(o.ID, p.Tier).Set(l.DailyBudgetUSD)
				s.logger.Printf("officers: %s's %s pool may reach a model through the LLM proxy: %s", o.ID, p.Tier, l.String())
			}
		}
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
	if s.officerRT == nil || s.officerRT.llm == nil || !o.UsesLLMProxy() {
		return
	}
	if p, n, ok := s.llmProxyInstance(o, instance); ok {
		s.officerRT.llm.ClaimedAttempt(o.ID, p, n, string(item), claimAttempts(claimBody))
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

// Why a complete sends no cost, beside the proxy meter's own reasons
// (llmproxy.Cost*): the omitted-cost log line's reason.
const (
	costNoSource  = "no_source"  // not an instance of an officer pool on the LLM proxy: no cost source muster reads
	costHandedOn  = "handed_on"  // needs_judgement: the item goes on to a judgement session
	costOverBound = "over_bound" // above HQ's bound (a reporting error HQ refuses)
)

// llmItemCost is the cost a proxied instance's complete of item reports to
// HQ (civ-hq docs/OFFICERS.md "Item cost"): what its hold booked through the
// proxy, from muster:llm-proxy, or nil when that is not known to be the
// item's whole cost — then NOTHING is sent, never a 0 — with why (a short
// id, for the log line). Only a complete that finishes the item carries one:
// needs_judgement hands it on to a judgement session, whose cost is not
// this hold's, so the item's total is not known. An instance of a pool on
// the proxy — routine beside `llm`, or judgement with tiers.judgement.llm —
// has the proxy as its cost source; any other instance's Claude Code session
// has none muster reads: nil.
func (s *Server) llmItemCost(o officers.Officer, instance string, item officers.ItemID, outcome string) (*officers.ItemCost, string) {
	if s.officerRT == nil || s.officerRT.llm == nil || !o.UsesLLMProxy() {
		return nil, costNoSource
	}
	pool, n, ok := s.llmProxyInstance(o, instance)
	if !ok {
		return nil, costNoSource
	}
	if outcome == officers.OutcomeNeedsJudgement {
		return nil, costHandedOn
	}
	usd, why := s.officerRT.llm.ItemCostWhy(o.ID, pool, n, string(item))
	if why != "" {
		return nil, why
	}
	// HQ stores 0-10000 USD (above is a reporting error it refuses); muster
	// books to the micro-dollar (officer_llm_usage's NUMERIC(14,6)).
	usd = math.Round(usd*1e6) / 1e6
	if math.IsNaN(usd) || usd < 0 || usd > officers.MaxItemCostUSD {
		return nil, costOverBound
	}
	return &officers.ItemCost{USD: usd, From: officers.CostFromLLMProxy}, ""
}

func (s *Server) llmEnded(o officers.Officer, instance string, item officers.ItemID) {
	if s.officerRT == nil || s.officerRT.llm == nil || !o.UsesLLMProxy() {
		return
	}
	if p, n, ok := s.llmProxyInstance(o, instance); ok {
		s.officerRT.llm.Ended(o.ID, p, n, string(item))
	}
}

// llmProxyInstance is the pool and instance number of agent name, when it is
// an instance of one of o's pools on the proxy (officers.ProxyAgent): the
// meter counts proxied instances only, keyed by pool and number — judgement
// #1 and routine r1 are different instances.
func (s *Server) llmProxyInstance(o officers.Officer, name string) (string, int, bool) {
	p, n, ok := officers.ProxyAgent(o, name)
	if !ok {
		return "", 0, false
	}
	return p.Tier, n, true
}

// llmSync is the wake loop's view of o's claimed items, for the per-item
// meter (officers.Config.Observed).
func llmSync(px *llmproxy.Proxy, reg *officers.Registry) func(o officers.Officer, claimed []officers.Item, at time.Time) {
	return func(o officers.Officer, claimed []officers.Item, at time.Time) {
		if !o.UsesLLMProxy() {
			return
		}
		holding := map[string]map[int][]string{}
		for _, p := range o.Pools() {
			if p.PoolLLM() != nil {
				holding[p.Tier] = map[int][]string{}
			}
		}
		for _, it := range claimed {
			if io, _, ok := reg.ForAgentName(it.ClaimedBy); ok && io.ID == o.ID {
				if p, n, ok := officers.ProxyAgent(io, it.ClaimedBy); ok {
					holding[p.Tier][n] = append(holding[p.Tier][n], string(it.ID))
				}
			}
		}
		for pool, h := range holding {
			px.Sync(o.ID, pool, h, at)
		}
	}
}
