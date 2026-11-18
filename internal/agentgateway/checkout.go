package agentgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// checkoutReadTimeout bounds one /healthz read. ccd answers it from memory.
const checkoutReadTimeout = 5 * time.Second

// Checkout reads the repository checkout a claude-code agent's ccd reports on
// /healthz (`repo`; cmd/ccd/checkout.go): the input internal/agentkickoff needs
// to hold a first turn, unstamped, until the repository exists
// (agents.DecideFirstTurn). It contacts the runtime but sends it no turn.
//
// A pod with no repository reports no `repo`, which is [agents.CheckoutUnknown]
// with a nil error. A read that fails is an error; the caller decides what an
// unread checkout means (agents.DecideFirstTurn: as before, unless the row
// already recorded a wait).
//
// ⚠ IT ACCEPTS A 503. ccd's /healthz answers non-200 when the session is
// unhealthy (a CLI in crash_loop), and its body still carries `repo`; the probe
// is a liveness signal, not this read's.
func (g *Gateway) Checkout(ctx context.Context, t Target) (agents.CheckoutStatus, error) {
	body, err := g.healthz(ctx, t)
	if err != nil {
		return agents.CheckoutStatus{}, fmt.Errorf("read checkout: %w", err)
	}
	return ParseHealthzCheckout(body)
}

// MCPSessionLanes reads whether agent a's ccd starts a chat lane with an MCP
// config naming its session (`mcp_session_lanes` on /healthz): what muster
// asks before it runs an officer's chat there (internal/api
// officerChatLanes.Supports). It contacts the runtime but sends it no turn.
func (g *Gateway) MCPSessionLanes(ctx context.Context, a agents.Agent) (bool, error) {
	t, err := g.Resolve(ctx, a)
	if err != nil {
		return false, err
	}
	body, err := g.healthz(ctx, t)
	if err != nil {
		return false, fmt.Errorf("read mcp_session_lanes: %w", err)
	}
	return ParseHealthzMCPSessionLanes(body)
}

// healthz is t's /healthz body, a 200 or a 503 (see Checkout).
func (g *Gateway) healthz(ctx context.Context, t Target) ([]byte, error) {
	if t.ep.IsZero() {
		return nil, fmt.Errorf("the target has no address")
	}
	ctx, cancel := context.WithTimeout(ctx, checkoutReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(t.ep.URL(), "/")+"/healthz", nil)
	if err != nil {
		return nil, err
	}
	// 🔴 A REDIRECT IS NOT FOLLOWED. /healthz is the agent's own; a 3xx is not
	// it, and following one would read another host's body as this agent's
	// checkout (and dial wherever it points). A copy: the shared client's
	// turns are untouched.
	client := *g.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return nil, fmt.Errorf("/healthz answered %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<16))
}

// ParseHealthzCheckout extracts `repo` from a ccd /healthz body. Exported so
// cmd/ccd's test can feed it the body ccd's own handler writes: the two sides of
// this wire meet only in a running pod otherwise.
func ParseHealthzCheckout(body []byte) (agents.CheckoutStatus, error) {
	var h struct {
		Repo *agents.CheckoutStatus `json:"repo"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return agents.CheckoutStatus{}, fmt.Errorf("read checkout: /healthz body: %w", err)
	}
	if h.Repo == nil {
		return agents.CheckoutStatus{}, nil
	}
	return *h.Repo, nil
}

// ParseHealthzMCPSessionLanes reads `mcp_session_lanes` from a ccd /healthz
// body: true when the ccd starts a chat lane's CLI with an MCP config naming
// its session (cmd/ccd lanes.go "A CHAT LANE SAYS WHO IT IS"); absent (an
// older ccd) is false.
func ParseHealthzMCPSessionLanes(body []byte) (bool, error) {
	var h struct {
		MCPSessionLanes bool `json:"mcp_session_lanes"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return false, fmt.Errorf("/healthz body: %w", err)
	}
	return h.MCPSessionLanes, nil
}
