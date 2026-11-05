package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/credentials"
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
// its owner's chief (isChiefAgent: chiefPredicate on the agent's current row),
// and that chief is ON (chiefActing, the gate the chief-only tools use). The
// agent authenticates as every caller of the route does (requireConnectorAgent:
// its own token; the shared hook and service tokens refused).
//
// 🔴 THE KEY IS THE OWNER'S, RESOLVED PER REQUEST (ChiefLLM.Keys, the
// credential plane): an admin's is MUSTER_AGENT_OPENROUTER_KEY, a member's is
// the one they stored under Account → Credentials. There is no chief key.
//
// 🔴 MUSTER KEEPS NO BUDGET FOR THE CHIEF (llmproxy.Caller.Unbudgeted): no
// daily budget, no per-item cap, no usage row. The cap is the key's own
// OpenRouter credit limit, which the owner sets. Usage is still metered
// (metrics and the audit record, officer label "chief").
// ---------------------------------------------------------------------------

// DefaultChiefLLMModel is the model a chief on the LLM proxy is pinned to when
// ChiefLLM.Model is empty: an OpenRouter model id.
const DefaultChiefLLMModel = "deepseek/deepseek-v4.1-flash"

// chiefLLMLabel is the chief's officer label in the proxy's metrics, log line
// and audit detail.
const chiefLLMLabel = "chief"

// NeedChiefOpenRouterKey is the refusal a chief gets when its owner has no
// OpenRouter key.
const NeedChiefOpenRouterKey = "your chief runs on your own OpenRouter key and there is none: add your OpenRouter key in " +
	"Account → Credentials (an admin's is the server's MUSTER_AGENT_OPENROUTER_KEY); nothing was sent"

// chiefKeyRemedy is what the chief is told when the provider refuses its key.
const chiefKeyRemedy = "it is your own key: check it, and its credit limit, in Account → Credentials (an admin's is " +
	"the server's MUSTER_AGENT_OPENROUTER_KEY)"

// ChiefLLMKeys resolves an owner's OpenRouter key. *credentials.Plane is the
// production one: an admin's is the environment's, a member's is their own,
// and a member with none is a credentials.ErrNoCredential.
type ChiefLLMKeys interface {
	OpenRouterKey(ctx context.Context, owner int64) (string, error)
}

var _ ChiefLLMKeys = (*credentials.Plane)(nil)

// ChiefLLM is EnableChiefLLM's input: the chief's place on the LLM proxy.
type ChiefLLM struct {
	// Model is the OpenRouter model every chief request is pinned to
	// (MUSTER_CHIEF_LLM_MODEL); "" is DefaultChiefLLMModel.
	Model string
	// Keys resolves the owner's key per request. REQUIRED.
	Keys ChiefLLMKeys
	// Upstream replaces the provider's API base (llmproxy.DefaultUpstream when
	// ""): tests, and MUSTER_TEST_LLM_UPSTREAM. HTTP replaces the transport.
	Upstream string
	HTTP     *http.Client
}

// chiefLLMRuntime is the chief's proxy and its settings; nil on the Server
// when EnableChiefLLM was never called.
type chiefLLMRuntime struct {
	px    *llmproxy.Proxy
	model string
	keys  ChiefLLMKeys
}

// EnableChiefLLM puts the owner's chief on the LLM proxy. Call it after
// UseExtensions. It refuses, for the caller to refuse to boot, without a key
// resolver, the agents store or the connector store: the proxy's door
// (requireConnectorAgent) answers 503 to every request without them, and its
// audit rows go to the connector store.
//
// The proxy is its own (llmproxy.Config.UnbudgetedOnly), with no usage store:
// a chief is never budgeted, so nothing would read one, and a budgeted caller
// on it is refused rather than served.
func (s *Server) EnableChiefLLM(c ChiefLLM) error {
	if c.Keys == nil {
		return errors.New("chief llm: no way to resolve the owner's OpenRouter key")
	}
	if s.ext.Connectors == nil || s.ext.Agents == nil {
		return errors.New("chief llm: this server has no connector store or agents store: the LLM proxy authenticates " +
			"the chief through the connector door and audits into its store")
	}
	model := strings.TrimSpace(c.Model)
	if model == "" {
		model = DefaultChiefLLMModel
	}
	px, err := llmproxy.New(llmproxy.Config{Upstream: c.Upstream, HTTP: c.HTTP, UnbudgetedOnly: true,
		Audit: s.llmAudit(c.Upstream), Logger: s.logger})
	if err != nil {
		return err
	}
	s.chiefLLM = &chiefLLMRuntime{px: px, model: model, keys: c.Keys}
	s.logger.Printf("chief llm: a chief may reach %s through the LLM proxy on its owner's OpenRouter key, with no muster "+
		"budget (the key's credit limit is the cap)", model)
	return nil
}

// serveChiefLLM is POST /llm/v1/messages for agent a, which isChiefAgent has
// called its owner's chief.
func (s *Server) serveChiefLLM(w http.ResponseWriter, r *http.Request, a agents.Agent) {
	ctx := r.Context()
	if !s.chiefActing(ctx, a) {
		metrics.OfficerLLMRefused.WithLabelValues(chiefLLMLabel, "chief_off").Inc()
		s.logger.Printf("llm proxy: refused agent %d (%q): the chief, and its owner turned the chief off", a.ID, a.Name)
		llmproxy.Refuse(w, http.StatusForbidden, "permission_error", "your owner turned the chief off")
		return
	}
	key, err := s.chiefLLM.keys.OpenRouterKey(ctx, agentOwnerID(a))
	switch {
	case (err == nil && strings.TrimSpace(key) == "") || credentials.IsNoCredential(err):
		metrics.OfficerLLMRefused.WithLabelValues(chiefLLMLabel, "no_key").Inc()
		s.logger.Printf("llm proxy: refused the chief agent %d (%q): its owner has no OpenRouter key (nothing sent)", a.ID, a.Name)
		// A 400, not a 401/429: Claude Code retries those, and this ends the
		// turn with the message.
		llmproxy.Refuse(w, http.StatusBadRequest, "invalid_request_error", NeedChiefOpenRouterKey)
		return
	case err != nil:
		metrics.OfficerLLMRefused.WithLabelValues(chiefLLMLabel, "key_unreadable").Inc()
		s.logger.Printf("llm proxy: the chief agent %d (%q): its owner's OpenRouter key could not be read (nothing sent): %v", a.ID, a.Name, err)
		llmproxy.Refuse(w, http.StatusServiceUnavailable, "api_error", "your OpenRouter key could not be read just now; nothing was sent")
		return
	}
	s.chiefLLM.px.ServeMessages(w, r, llmproxy.Caller{Officer: chiefLLMLabel, InstanceLabel: chiefLLMLabel,
		AgentID: a.ID, AgentName: a.Name, Model: s.chiefLLM.model, Key: key, Unbudgeted: true,
		Title: "muster chief", KeyRemedy: chiefKeyRemedy})
}
