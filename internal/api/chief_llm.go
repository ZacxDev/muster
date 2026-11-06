package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/metrics"
)

// ---------------------------------------------------------------------------
// THE CHIEF ON THE LLM PROXY
//
// The compose install's chief is a claude-code agent; when it runs on
// OpenRouter rather than a Claude subscription, its Claude Code points
// ANTHROPIC_BASE_URL at $MUSTER_CONNECT_URL/llm exactly as an officer's routine
// instance does, and POST /llm/v1/messages admits it here.
//
// It is admitted exactly when EnableChiefLLM was called, the calling agent is
// not a routine officer instance but its owner's chief (agentIsChief:
// chiefPredicate on the agent's current row; a failed read is a 503),
// that owner is the admin (authz.OperatorUserID — the compose runtime runs one
// chief, the admin's), and that chief is ON (chiefActing, the gate the
// chief-only tools use). The agent authenticates as every caller of the route
// does (requireConnectorAgent: its own token; the shared hook and service
// tokens refused). On Compose (Extensions.ComposeChief set) it is admitted
// only when ComposeChief.Credential says OpenRouter, on the key the
// credential plane gives (composeChiefLLMKey).
//
// 🔴 THE KEY IS THE ADMIN'S, MUSTER_AGENT_OPENROUTER_KEY (ChiefLLM.Key). There
// is no chief key, and no other user's chief is served on it.
//
// 🔴 MUSTER KEEPS NO BUDGET FOR THE CHIEF (llmproxy.Caller.Unbudgeted): no
// daily budget, no per-item cap, no usage row. The cap is the key's own
// OpenRouter credit limit, which the admin sets. Usage is still metered
// (metrics and the audit record, officer label "chief").
// ---------------------------------------------------------------------------

// DefaultChiefLLMModel is the model a chief on the LLM proxy is pinned to when
// ChiefLLM.Model is empty: an OpenRouter model id.
const DefaultChiefLLMModel = "deepseek/deepseek-v4.1-flash"

// chiefLLMLabel is the chief's officer label in the proxy's metrics, log line
// and audit detail.
const chiefLLMLabel = "chief"

// NeedChiefOpenRouterKey is the refusal a chief gets when the server has no
// OpenRouter key for it.
const NeedChiefOpenRouterKey = "the chief runs on the server's OpenRouter key and there is none: set MUSTER_AGENT_OPENROUTER_KEY " +
	"in .env and restart muster; nothing was sent"

// chiefNotAdminRefusal is what any chief but the admin's is told.
const chiefNotAdminRefusal = "only the admin's chief is served on the server's OpenRouter key; this chief belongs to another user"

// ChiefLLMAgentsReadRefusal is the 503 a caller of POST /llm/v1/messages gets
// when, with the chief LLM on, the agents list could not be read to tell
// whether it is the chief (serveLLMMessages).
const ChiefLLMAgentsReadRefusal = "muster could not read its agents list to tell whether this agent is the chief; nothing was " +
	"sent, retry shortly"

// chiefKeyRemedy is what the chief is told when the provider refuses its key,
// or answers 402 (its credit limit is spent).
const chiefKeyRemedy = "it is the server's MUSTER_AGENT_OPENROUTER_KEY: check it, and its credit limit, then restart muster"

// ChiefLLM is EnableChiefLLM's input: the chief's place on the LLM proxy.
type ChiefLLM struct {
	// Model is the OpenRouter model every chief request is pinned to; "" is
	// DefaultChiefLLMModel.
	Model string
	// Key is the admin's OpenRouter key (MUSTER_AGENT_OPENROUTER_KEY). Empty
	// is not a boot refusal: each request is refused (400, nothing sent) with
	// a message naming the variable.
	Key string
	// Upstream replaces the provider's API base (llmproxy.DefaultUpstream when
	// ""), and HTTP its transport: tests.
	Upstream string
	HTTP     *http.Client
}

// chiefLLMRuntime is the chief's proxy and its settings; nil on the Server
// when EnableChiefLLM was never called.
type chiefLLMRuntime struct {
	px    *llmproxy.Proxy
	model string
	key   string
}

// EnableChiefLLM puts the admin's chief on the LLM proxy. Call it after
// UseExtensions. It refuses, for the caller to refuse to boot, without the
// agents store or the connector store: the proxy's door (requireConnectorAgent)
// answers 503 to every request without them, and its audit rows go to the
// connector store.
//
// The proxy is its own, with no usage store: the chief is never budgeted, so
// nothing would read one, and a budgeted caller on it is refused rather than
// served (llmproxy.Config.Store).
func (s *Server) EnableChiefLLM(c ChiefLLM) error {
	if s.ext.Connectors == nil || s.ext.Agents == nil {
		return errors.New("chief llm: this server has no connector store or agents store: the LLM proxy authenticates " +
			"the chief through the connector door and audits into its store")
	}
	model := strings.TrimSpace(c.Model)
	if model == "" {
		model = DefaultChiefLLMModel
	}
	px, err := llmproxy.New(llmproxy.Config{Upstream: c.Upstream, HTTP: c.HTTP, Audit: s.llmAudit(c.Upstream), Logger: s.logger})
	if err != nil {
		return err
	}
	s.chiefLLM = &chiefLLMRuntime{px: px, model: model, key: strings.TrimSpace(c.Key)}
	if s.chiefLLM.key == "" {
		s.logger.Printf("chief llm: ON for the admin's chief (%s), but MUSTER_AGENT_OPENROUTER_KEY is empty: every request "+
			"is refused until it is set", model)
		return nil
	}
	s.logger.Printf("chief llm: the admin's chief may reach %s through the LLM proxy on MUSTER_AGENT_OPENROUTER_KEY, "+
		"with no muster budget (the key's credit limit is the cap)", model)
	return nil
}

// serveChiefLLM is POST /llm/v1/messages for agent a, which agentIsChief has
// called its owner's chief (serveLLMMessages).
func (s *Server) serveChiefLLM(w http.ResponseWriter, r *http.Request, a agents.Agent) {
	ctx := r.Context()
	if agentOwnerID(a) != authz.OperatorUserID {
		metrics.OfficerLLMRefused.WithLabelValues(chiefLLMLabel, "not_admin").Inc()
		s.logger.Printf("llm proxy: refused agent %d (%q): a chief, but not the admin's", a.ID, a.Name)
		llmproxy.Refuse(w, http.StatusForbidden, "permission_error", chiefNotAdminRefusal)
		return
	}
	if !s.chiefActing(ctx, a) {
		metrics.OfficerLLMRefused.WithLabelValues(chiefLLMLabel, "chief_off").Inc()
		s.logger.Printf("llm proxy: refused agent %d (%q): the chief, and its owner turned the chief off", a.ID, a.Name)
		llmproxy.Refuse(w, http.StatusForbidden, "permission_error", "your owner turned the chief off")
		return
	}
	key := s.chiefLLM.key
	// On Compose the one credential rule decides, and the key is the plane's
	// (compose_chief.go composeChiefLLMKey).
	if s.ext.ComposeChief != nil {
		var ok bool
		if key, ok = s.composeChiefLLMKey(w, ctx, a); !ok {
			return
		}
	}
	if key == "" {
		metrics.OfficerLLMRefused.WithLabelValues(chiefLLMLabel, "no_key").Inc()
		s.logger.Printf("llm proxy: refused the chief agent %d (%q): MUSTER_AGENT_OPENROUTER_KEY is empty (nothing sent)", a.ID, a.Name)
		// A 400, not a 401/429: Claude Code retries those, and this ends the
		// turn with the message.
		llmproxy.Refuse(w, http.StatusBadRequest, "invalid_request_error", NeedChiefOpenRouterKey)
		return
	}
	s.chiefLLM.px.ServeMessages(w, r, llmproxy.Caller{Officer: chiefLLMLabel, InstanceLabel: chiefLLMLabel,
		AgentID: a.ID, AgentName: a.Name, Model: s.chiefLLM.model, Key: key, Unbudgeted: true,
		Title: "muster chief", KeyRemedy: chiefKeyRemedy})
}
