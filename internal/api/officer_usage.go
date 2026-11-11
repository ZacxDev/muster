package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/usage"
)

// ---------------------------------------------------------------------------
// THE OFFICER USAGE COLLECTOR, BOUND TO THE SERVER (internal/officers/usage,
// docs/officers.md "Claude usage to HQ"): every declared officer's up
// instances' Claude usage, read from each instance's ccd through the agent
// gateway BY ITS OWN AGENT ROW, pushed to the officer's HQ with the officer's
// token. The instances it reads are the wake loop's own view
// (officerFleet.Instances); the one extra push is officerFleet.Stop's.
// ---------------------------------------------------------------------------

// usageReader is the agent gateway's usage read (agentgateway.Gateway.Usage),
// asserted rather than added to Gateway: it is the usage collector's alone.
type usageReader interface {
	Usage(ctx context.Context, a agents.Agent, since time.Time) (cctranscript.UsagePage, error)
}

var _ usageReader = (*agentgateway.Gateway)(nil)

// gatewayUsage is usage.Source over the agent gateway: the instance agent by
// name (as its officer's owner), then its ccd's GET /v1/usage.
type gatewayUsage struct {
	s *Server
	r usageReader
}

func (g gatewayUsage) Read(ctx context.Context, o officers.Officer, instance string, since time.Time) (usage.Reading, error) {
	octx := g.s.officerPrincipal(ctx, o.Owner)
	a, err := g.s.ext.Agents.GetByName(octx, instance)
	if err != nil {
		return usage.Reading{}, fmt.Errorf("officers: instance %s: %w", instance, err)
	}
	p, err := g.r.Usage(octx, a, since)
	if errors.Is(err, agentgateway.ErrUsageUnsupported) {
		return usage.Reading{}, usage.ErrUnsupported
	}
	if err != nil {
		return usage.Reading{}, err
	}
	return usage.Reading{Account: instanceUsageAccount(o, a), Buckets: p.Buckets}, nil
}

// instanceUsageAccount is the Claude account instance a's usage is reported
// on: the account its row is on NOW (agents.Agent.CCAccount — an account
// failover moves it, and every hour pushed after the move is reported on the
// new account), and none for an instance of a pool on the LLM proxy
// (officers.ProxyAgent), which runs on no Claude account whatever its row
// carries.
func instanceUsageAccount(o officers.Officer, a agents.Agent) string {
	if _, _, proxied := officers.ProxyAgent(o, a.Name); proxied {
		return ""
	}
	return a.CCAccount
}

// newUsageCollector builds the collector for every declared officer, or nil
// — logged, never a boot refusal: it needs no configuration, so an operator
// cannot have asked for it — when there is no way to read an instance's
// usage.
func (s *Server) newUsageCollector(cfg OfficersConfig, hq *officers.ClientHQ) (*usage.Collector, error) {
	src := cfg.UsageSource
	if src == nil {
		r, ok := s.ext.Gateway.(usageReader)
		if !ok {
			s.logger.Printf("officers: Claude usage to HQ is off: the agent gateway cannot read an instance's usage")
			return nil, nil
		}
		src = gatewayUsage{s: s, r: r}
	}
	return usage.New(usage.Config{Officers: cfg.Officers, Instances: officerFleet{s: s}.Instances, Source: src,
		Poster: usage.NewClientPoster(hq), Logger: s.logger, Now: cfg.Now, Interval: cfg.UsageInterval})
}
