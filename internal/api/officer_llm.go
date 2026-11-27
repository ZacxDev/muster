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
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/ui"
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
	mux.HandleFunc(ChiefSessionModelsRoute, s.requireSession(s.handleChiefSessionModels))
	// The picker's own answer to the same search (ui.OpenRouterModelsOptionsPath).
	mux.HandleFunc("GET "+ui.OpenRouterModelsOptionsPath, s.requireSession(s.handleChiefSessionModelOptions))
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
	// 🔴 A REQUEST CARRYING X-Muster-Chief-Session IS A CHIEF SESSION'S, AND
	// GOES TO ITS DOOR ALONE (chief_session_llm.go), whether that door is on
	// or not: never on to the chief's own route, whose key is
	// MUSTER_AGENT_OPENROUTER_KEY, nor to an officer's. A request without the
	// header takes the path below exactly as before.
	if chiefSessionRequest(r) {
		s.serveChiefSessionLLM(w, r, a)
		return
	}
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
		s.llmHoldsEnded(o, s.officerRT.llm.ClaimedAttempt(o.ID, p, n, string(item), claimAttempts(claimBody)))
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
// this hold's, so the item's total is not known here. The hold's own cost
// still reaches HQ, as every hold's does when it ends (llmHoldsEnded), and
// HQ adds the holds up. An instance of a pool on
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
	return proxyCost(usd)
}

// proxyCost is usd, booked by the LLM proxy, as the cost HQ takes (an item's
// or a hold's): rounded to the micro-dollar muster books to
// (officer_llm_usage's NUMERIC(14,6)), or nil and costOverBound outside HQ's
// 0-10000 USD (above is a reporting error HQ refuses).
func proxyCost(usd float64) (*officers.ItemCost, string) {
	usd = math.Round(usd*1e6) / 1e6
	if math.IsNaN(usd) || usd < 0 || usd > officers.MaxItemCostUSD {
		return nil, costOverBound
	}
	return &officers.ItemCost{USD: usd, From: officers.CostFromLLMProxy}, ""
}

// holdCostTimeout bounds one hold-cost report to HQ (sent in the
// background: llmHoldsEnded).
const holdCostTimeout = 10 * time.Second

// holdCostMaxSends bounds the hold-cost reports running at once: one past it
// is dropped with a log line, never queued or retried.
const holdCostMaxSends = 64

// holdCostSenders is the hold-cost reports being sent (llmHoldsEnded).
type holdCostSenders struct {
	wg sync.WaitGroup
	n  atomic.Int32
}

// flushHoldCosts waits until every hold-cost report handed to a send so far
// is sent (or failed): the tests' hook.
func (s *Server) flushHoldCosts() { s.holdCostSends.wg.Wait() }

// llmHoldsEnded reports each ended hold of o's proxied instances to HQ
// (civ-hq docs/OFFICERS.md "Per-hold cost"): the hold's cost, by HQ's claim
// number, with o's own backend, which adds every hold of an item up. Every
// hold that ended — complete (any outcome: needs_judgement's hand-on
// included), release, ask, HQ's new claim of the same item, or a lapsed
// lease the wake loop saw — is handed here once by the meter. A claim of
// another item ends none: the meter parks the held one until one of those.
//
// 🔴 A HOLD WHOSE COST IS NOT KNOWN WHOLE SENDS NOTHING, never a 0
// (llmproxy holdWhy): HQ then never has every hold, and the item carries no
// per-hold total. 🔴 A REPORT NEVER FAILS OR BLOCKS ANYTHING: each is sent
// in the background (at most holdCostMaxSends at once, each bounded by
// holdCostTimeout), because the wake loop's read (llmSync, under
// officers.Config.Observed) must not block and the complete, release or
// claim it follows has already succeeded. A report not sent — a backend
// without the verb (the builtin one), an HQ without the route (404), any
// other failure, or one past the bound — is one log line, never retried.
// HQ adds the holds up in any order, so the sends need none.
func (s *Server) llmHoldsEnded(o officers.Officer, ended []llmproxy.HoldCost) {
	if len(ended) == 0 || s.officerRT == nil || s.officerRT.hq == nil {
		return
	}
	for _, h := range ended {
		instance := o.Pool(h.Pool).InstanceName(h.Instance)
		cost, why := (*officers.ItemCost)(nil), h.Why
		if why == "" {
			cost, why = proxyCost(h.USD)
		}
		if cost == nil {
			// One line per hold ended without a known cost, with why: the
			// known-vs-unknown ratio is measured from these. No item content.
			s.logger.Printf("officers: hold cost omitted officer=%s instance=%s item=%s attempt=%d reason=%s",
				o.ID, instance, h.Item, h.Attempt, why)
			continue
		}
		hb, err := officers.Serves[officers.HoldCostBackend](s.officerRT.hq.Backend(o), officers.CapHoldCost)
		if err != nil {
			s.logger.Printf("officers: hold cost not sent officer=%s instance=%s item=%s attempt=%d: %v", o.ID, instance, h.Item, h.Attempt, err)
			continue
		}
		w := &s.holdCostSends
		if w.n.Add(1) > holdCostMaxSends {
			w.n.Add(-1)
			s.logger.Printf("officers: hold cost not sent officer=%s instance=%s item=%s attempt=%d: %d reports already running",
				o.ID, instance, h.Item, h.Attempt, holdCostMaxSends)
			continue
		}
		w.wg.Add(1)
		officer, item, attempt, c := o.ID, h.Item, h.Attempt, *cost
		safeGoDone(s.logger, "officers: send a hold cost", &w.wg, func() {
			defer w.n.Add(-1)
			ctx, cancel := context.WithTimeout(context.Background(), holdCostTimeout)
			defer cancel()
			if _, err := hb.HoldCost(ctx, officers.ItemID(item), instance, attempt, c); err != nil {
				s.logger.Printf("officers: hold cost not recorded officer=%s instance=%s item=%s attempt=%d cost_usd=%.6f: %v",
					officer, instance, item, attempt, c.USD, err)
			}
		})
	}
}

func (s *Server) llmEnded(o officers.Officer, instance string, item officers.ItemID) {
	if s.officerRT == nil || s.officerRT.llm == nil || !o.UsesLLMProxy() {
		return
	}
	if p, n, ok := s.llmProxyInstance(o, instance); ok {
		s.llmHoldsEnded(o, s.officerRT.llm.Ended(o.ID, p, n, string(item)))
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
// meter (officers.Config.Observed). Every hold it ends (one HQ no longer
// shows: a lapsed lease) goes to ended (Server.llmHoldsEnded).
func llmSync(px *llmproxy.Proxy, reg *officers.Registry, ended func(officers.Officer, []llmproxy.HoldCost)) func(o officers.Officer, claimed []officers.Item, at time.Time) {
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
			ended(o, px.Sync(o.ID, pool, h, at))
		}
	}
}
