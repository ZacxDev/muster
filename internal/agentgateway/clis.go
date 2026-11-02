package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// ErrCLIActivityUnsupported is an agent runtime without GET /v1/clis: a ccd
// image older than the route, or a runtime that is not ccd.
var ErrCLIActivityUnsupported = errors.New("agentgateway: the agent's runtime does not report its CLIs (GET /v1/clis); its image predates it")

// cliActivityTimeout bounds one read; maxCLIActivityBody its body.
const (
	cliActivityTimeout = 5 * time.Second
	maxCLIActivityBody = 1 << 20
)

// CLIActivity reads which CLIs in agent a's pod are working (ccd's GET
// /v1/clis, agents.CLIActivity). The bearer is the turn bearer.
func (g *Gateway) CLIActivity(ctx context.Context, a agents.Agent) (agents.CLIActivity, error) {
	t, err := g.Resolve(ctx, a)
	if err != nil {
		return agents.CLIActivity{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, cliActivityTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(t.ep.URL(), "/")+"/v1/clis", nil)
	if err != nil {
		return agents.CLIActivity{}, fmt.Errorf("agentgateway: clis request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.bearer)
	resp, err := g.client.Do(req)
	if err != nil {
		return agents.CLIActivity{}, fmt.Errorf("agentgateway: read agent %q's CLIs: %w", a.Name, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return agents.CLIActivity{}, ErrCLIActivityUnsupported
	case resp.StatusCode != http.StatusOK:
		return agents.CLIActivity{}, fmt.Errorf("agentgateway: agent %q's CLIs: its runtime answered %d", a.Name, resp.StatusCode)
	}
	var out agents.CLIActivity
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCLIActivityBody)).Decode(&out); err != nil {
		return agents.CLIActivity{}, fmt.Errorf("agentgateway: agent %q's CLIs are not an answer: %v", a.Name, err)
	}
	if out.Version != agents.CLIActivityVersion {
		return agents.CLIActivity{}, fmt.Errorf("agentgateway: agent %q's CLIs answer version %d, want %d", a.Name, out.Version, agents.CLIActivityVersion)
	}
	return out, nil
}
