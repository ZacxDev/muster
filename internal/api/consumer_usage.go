package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/consumerusage"
)

// ---------------------------------------------------------------------------
// THE CONSUMER USAGE COLLECTOR, BOUND TO THE SERVER (internal/consumerusage,
// docs/officers.md "Consumer usage to HQ", MUSTER_CC_CONSUMER_USAGE): the
// Claude usage of this muster's NON-officer instances on the operator's pool
// accounts, read from each instance's ccd through the agent gateway by its
// own row, attributed per hour to the account it ran on (its failover
// history), and PUT to the pool push's HQ as this muster's consumer.
//
// Which instances: consumerUsageEligible (the one rule, used by the tick's
// list AND the pre-stop push) and, for a tick, up. The pre-stop push is the
// lifecycle adapter's hook (agentprovision.Adapter.UseBeforeStop): every Stop
// and every Destroy of an eligible instance, whoever asks for it, pushes
// once first.
// ---------------------------------------------------------------------------

// ConsumerUsageConfig is EnableConsumerUsage's input.
type ConsumerUsageConfig struct {
	// Consumer is the muster's consumer (consumerusage.ValidConsumer),
	// Instance its label (the pool push's), URL the consumer-usage route and
	// Token the pool push's bearer.
	Consumer, Instance, URL, Token string
	HTTPClient                     *http.Client
	// InstallBeforeStop installs the pre-stop push on the lifecycle adapter
	// (agentprovision.Adapter.UseBeforeStop). Required: a collector whose
	// last push of a stopping instance is never made leaves its last hour
	// partial at HQ.
	InstallBeforeStop func(func(ctx context.Context, a agents.Agent))
	// Source replaces where an instance's usage is read (tests); nil is the
	// agent gateway's ccd read. Backoff, Now and Interval replace the
	// sender's waits and the collector's clock and cadence (tests). NoRun
	// builds the collector without starting its loop (tests tick it).
	Source   consumerusage.Source
	Backoff  []time.Duration
	Sleep    func(ctx context.Context, d time.Duration) error
	Now      func() time.Time
	Interval time.Duration
	NoRun    bool
}

// EnableConsumerUsage builds the consumer usage collector, installs its
// pre-stop push and starts its loop. It is refused — the operator asked for
// it, so a muster that cannot do it must not boot as if it did — without the
// agents store, the provisioner, the pre-stop installer, or a gateway that
// can read an instance's usage.
func (s *Server) EnableConsumerUsage(cfg ConsumerUsageConfig) (*consumerusage.Collector, error) {
	if s.ext.Agents == nil || s.ext.Provisioner == nil {
		return nil, errors.New("consumer usage: needs the agents store and an agent provisioner")
	}
	if cfg.InstallBeforeStop == nil {
		return nil, errors.New("consumer usage: no way to push an instance's usage before it is stopped (no lifecycle adapter)")
	}
	src := cfg.Source
	if src == nil {
		r, ok := s.ext.Gateway.(usageReader)
		if !ok {
			return nil, errors.New("consumer usage: the agent gateway cannot read an instance's usage")
		}
		src = consumerGatewayUsage{s: s, r: r}
	}
	c, err := consumerusage.New(consumerusage.Config{Consumer: cfg.Consumer, Instance: cfg.Instance, Instances: s.consumerUsageInstances,
		Source: src, Poster: &consumerusage.Sender{URL: cfg.URL, Token: cfg.Token, Client: cfg.HTTPClient, Backoff: cfg.Backoff, Sleep: cfg.Sleep},
		Logger: s.logger, Now: cfg.Now, Interval: cfg.Interval})
	if err != nil {
		return nil, err
	}
	cfg.InstallBeforeStop(func(ctx context.Context, a agents.Agent) {
		if s.consumerUsageEligible(a) {
			c.Flush(ctx, a.Name)
		}
	})
	if !cfg.NoRun {
		// Ends with the server's streams (EndStreams, at shutdown).
		ctx := s.streams.context()
		safeGo(s.logger, "consumer usage collector", func() { c.Run(ctx) })
	}
	s.logger.Printf("consumer usage: ON — every %s, the Claude usage of this muster's up non-officer claude-code instances on the "+
		"operator's pool accounts is pushed to HQ as consumer %q, instance %q", consumerusage.DefaultInterval, cfg.Consumer, cfg.Instance)
	return c, nil
}

// consumerUsagePrincipal is ctx acting as the OPERATOR: the pool push's
// accounts are the operator's environment pool, so the instances on them are
// the operator's rows, and row-level security says no one else's are read.
func (s *Server) consumerUsagePrincipal(ctx context.Context) context.Context {
	return authz.Operator(ctx)
}

// consumerUsageEligible is THE rule for whose usage the consumer push carries:
// the operator's agents on a pool account — which are exactly its claude-code
// agents (migration 0003's agents_cc_account_matches_kind: a row names an
// account iff it is claude-code) — that are NOT an officer's instance (whose
// usage goes to its officer's HQ, internal/officers/usage: pushing it here
// too would count it twice).
func (s *Server) consumerUsageEligible(a agents.Agent) bool {
	if a.OwnerID != authz.OperatorUserID || a.CCAccount == "" {
		return false
	}
	_, _, officer := s.officerOf(a)
	return !officer
}

// consumerUsageInstances is the collector's list: every eligible agent whose
// instance is up, by name. It FAILS BLIND: an unreadable agent table or
// provisioner is an error, never "nothing is up".
func (s *Server) consumerUsageInstances(ctx context.Context) ([]string, error) {
	octx := s.consumerUsagePrincipal(ctx)
	list, err := s.ext.Agents.List(octx)
	if err != nil {
		return nil, fmt.Errorf("read agents: %w", err)
	}
	idx := s.instanceIndexWithin(octx, officerInstancesWithin)
	if idx == nil {
		return nil, errors.New("the provisioner's instances could not be read")
	}
	var out []string
	for _, a := range list {
		if s.consumerUsageEligible(a) && instanceUp(a, idx) {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// consumerFailoverLimit is how many of an instance's failovers are read: far
// more than a 25-hour window holds (each one is an account hitting its limit).
const consumerFailoverLimit = 100

// consumerGatewayUsage is consumerusage.Source over the agent gateway: the
// instance agent by name (as the operator), its ccd's GET /v1/usage, and its
// account failovers.
type consumerGatewayUsage struct {
	s *Server
	r usageReader
}

func (g consumerGatewayUsage) Read(ctx context.Context, instance string, since time.Time) (consumerusage.Reading, error) {
	octx := g.s.consumerUsagePrincipal(ctx)
	a, err := g.s.ext.Agents.GetByName(octx, instance)
	if err != nil {
		return consumerusage.Reading{}, fmt.Errorf("instance %s: %w", instance, err)
	}
	p, err := g.r.Usage(octx, a, since)
	if errors.Is(err, agentgateway.ErrUsageUnsupported) {
		return consumerusage.Reading{}, consumerusage.ErrUnsupported
	}
	if err != nil {
		return consumerusage.Reading{}, err
	}
	rd := consumerusage.Reading{Account: a.CCAccount, Buckets: p.Buckets}
	if g.s.ext.AccountFailover != nil {
		evs, err := g.s.ext.AccountFailover.Events(octx, a.ID, consumerFailoverLimit)
		if err != nil {
			// Without the history an hour before a failover would be reported
			// on the account the instance moved to.
			return consumerusage.Reading{}, fmt.Errorf("instance %s: its account failovers could not be read: %w", instance, err)
		}
		for _, e := range evs {
			rd.Failovers = append(rd.Failovers, consumerusage.Failover{From: e.From, To: e.To, At: e.At})
		}
	}
	return rd, nil
}
