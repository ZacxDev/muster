// Package usage pushes each officer instance's Claude usage — API calls,
// tokens and turns per session and UTC hour — to the officer's HQ
// (docs/officers.md, "Claude usage to HQ").
//
// THE SOURCE is the instance's own Claude Code transcripts, counted by the
// instance pod's ccd on GET /v1/usage (cmd/ccd/usage_route.go) and read
// through the agent gateway with the instance's turn bearer. muster-server
// counts nothing itself: ccd owns the counting rule, next to the files.
//
// THE SINK is HQ's PUT /officers/{id}/usage (officers.Client.PutUsage), sent
// with the officer's own token. HQ upserts each bucket and REPLACES its
// counts, so every push is the whole of the window (Window) and a push is
// idempotent: a later one only ever corrects an earlier one.
//
// WHEN: every Interval, for every declared officer, for each instance the
// wake loop's own view (officers.Fleet.Instances) says exists and is up — and
// once more just before the wake loop scales an instance to zero (Flush), so
// the hour it stopped in is not left partial at HQ until it next runs. Its
// pod volume is kept on scale-to-zero, so the transcripts survive; but hours
// older than Window when the instance comes back are never pushed.
//
// 🔴 IT NEVER RETRIES WITHIN A TICK AND NEVER BLOCKS THE OFFICER: a failed
// read or push is counted (muster_officer_usage_push_total{result}) and
// logged once per change of state, and the next tick's push covers it.
// Flush is bounded by FlushTimeout and its failure never fails the stop. No
// transcript text passes through here, and an HQ refusal is logged by its
// status, never its body.
package usage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/officers"
)

// ErrUnsupported is an instance whose ccd does not serve GET /v1/usage (an
// image that predates it). The instance is skipped quietly.
var ErrUnsupported = errors.New("usage: the instance's ccd does not serve GET /v1/usage (its image predates it)")

// Reading is one instance's usage as its Source read it.
type Reading struct {
	// Account is the pooled Claude account the instance is on NOW, "" for a
	// pool on the LLM proxy (officers.UsageReport.Account).
	Account string
	Buckets []cctranscript.UsageBucket
}

// Source reads an officer instance's usage since since (production: its
// ccd's GET /v1/usage, through the agent gateway). An image without the route
// is ErrUnsupported.
type Source interface {
	Read(ctx context.Context, o officers.Officer, instance string, since time.Time) (Reading, error)
}

// Instances is the wake loop's view of pool o's instances
// (officers.Fleet.Instances): THE rule for which instance exists and is up.
type Instances func(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error)

// Poster is the HQ side.
type Poster interface {
	PutUsage(ctx context.Context, o officers.Officer, r officers.UsageReport) error
}

type clientPoster struct{ hq *officers.ClientHQ }

// NewClientPoster is a Poster over each officer's own HQ client.
func NewClientPoster(hq *officers.ClientHQ) Poster { return clientPoster{hq: hq} }

func (p clientPoster) PutUsage(ctx context.Context, o officers.Officer, r officers.UsageReport) error {
	return p.hq.Client(o).PutUsage(ctx, r)
}

const (
	// DefaultInterval is how often every up instance's usage is pushed.
	DefaultInterval = 10 * time.Minute
	// Window is how far back each push reaches. ccd truncates its start down
	// to the hour, so the oldest hour pushed is whole. Longer than a day, so
	// a muster restart or an HQ outage of up to a day loses nothing.
	Window = 25 * time.Hour
	// DefaultRequestTimeout bounds one read or one push.
	DefaultRequestTimeout = 30 * time.Second
	// FlushTimeout bounds the push before a scale-to-zero, read and push
	// together.
	FlushTimeout = 10 * time.Second
)

// Config is New's input. Zero values take the defaults.
type Config struct {
	// Officers are the declared officers; each one's pools are walked.
	Officers  []officers.Officer
	Instances Instances
	Source    Source
	Poster    Poster
	Logger    *log.Logger
	Now       func() time.Time
	// Interval is the cadence (DefaultInterval).
	Interval time.Duration
	// RequestTimeout bounds one read or push (DefaultRequestTimeout).
	RequestTimeout time.Duration
}

// The results, the label values of muster_officer_usage_push_total.
const (
	resultPushed         = "pushed"
	resultUnsupported    = "ccd_unsupported"
	resultReadFailed     = "ccd_failed"
	resultHQNotFound     = "hq_not_found"
	resultHQRefused      = "hq_refused"
	resultHQUnavailable  = "hq_unavailable"
	resultFlushSkipped   = "skipped"
	stateOK              = ""
	unsupportedLogFormat = "officers: %s: instance %s's ccd does not serve its Claude usage (GET /v1/usage): its image predates it; " +
		"skipped until it runs a newer one"
)

// Collector is the usage collector.
type Collector struct {
	cfg Config
	log *log.Logger
	now func() time.Time

	// sem lets one collect (a read and its push) run at a time, so a tick's
	// push and a pre-stop flush of the same instance can never land at HQ out
	// of order (an older window replacing a newer one).
	sem chan struct{}

	mu sync.Mutex
	// state is each instance's last result class ("" = pushed), so a failure
	// is logged once when it starts and once when it ends.
	state map[string]string
	// hqAbsent is each officer whose HQ answered the route 404, logged once.
	hqAbsent map[string]bool
}

// New builds a collector.
func New(cfg Config) (*Collector, error) {
	if cfg.Instances == nil || cfg.Source == nil || cfg.Poster == nil {
		return nil, errors.New("usage: a collector needs the fleet's instances, a source and a poster")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	return &Collector{cfg: cfg, log: cfg.Logger, now: cfg.Now, sem: make(chan struct{}, 1),
		state: map[string]string{}, hqAbsent: map[string]bool{}}, nil
}

// Run ticks every Interval until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

func (c *Collector) tick(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			c.log.Printf("officers: PANIC in the usage collector: %v", rec)
		}
	}()
	c.Tick(ctx)
}

// Tick pushes the usage of every up instance of every declared officer's
// pools, one at a time. A pool whose instances cannot be seen is skipped
// this tick.
func (c *Collector) Tick(ctx context.Context) {
	for _, o := range c.cfg.Officers {
		for _, p := range o.Pools() {
			states, err := c.cfg.Instances(ctx, p)
			if err != nil {
				c.log.Printf("officers: %s: usage: its instances could not be listed this tick: %v", p.ID, err)
				continue
			}
			for _, st := range states {
				if !st.Exists || !st.Up {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case c.sem <- struct{}{}:
				}
				c.collect(ctx, p, st.Name)
				<-c.sem
			}
		}
	}
}

// Flush pushes instance's usage once, now: the wake loop is about to scale
// it to zero (officers.Fleet.Stop). Bounded by FlushTimeout, read and push
// together; best-effort — whatever happens is counted and logged, and
// nothing is returned for the stop to fail on.
func (c *Collector) Flush(ctx context.Context, o officers.Officer, instance string) {
	ctx, cancel := context.WithTimeout(ctx, FlushTimeout)
	defer cancel()
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		metrics.OfficerUsagePush.WithLabelValues(resultFlushSkipped).Inc()
		c.log.Printf("officers: %s: instance %s: its usage was not pushed before scale-to-zero (another push held the collector past %s)",
			o.ID, instance, FlushTimeout)
		return
	}
	defer func() { <-c.sem }()
	c.collect(ctx, o, instance)
}

// collect reads instance's usage and pushes it, once.
func (c *Collector) collect(ctx context.Context, o officers.Officer, instance string) {
	now := c.now()
	rctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	rd, err := c.cfg.Source.Read(rctx, o, instance, now.Add(-Window))
	cancel()
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			c.result(o, instance, resultUnsupported, fmt.Sprintf(unsupportedLogFormat, o.ID, instance))
			return
		}
		c.result(o, instance, resultReadFailed, fmt.Sprintf("officers: %s: instance %s: its Claude usage could not be read: %v", o.ID, instance, err))
		return
	}
	buckets := capBuckets(rd.Buckets)
	if dropped := len(rd.Buckets) - len(buckets); dropped > 0 {
		c.log.Printf("officers: %s: instance %s: %d usage buckets, over HQ's %d: sending the newest %d, dropping the oldest %d",
			o.ID, instance, len(rd.Buckets), officers.MaxUsageBuckets, len(buckets), dropped)
	}
	r := officers.UsageReport{Instance: instance, Pool: poolOf(o), Account: rd.Account, GeneratedAt: now.UTC().Format(time.RFC3339), Buckets: buckets}
	pctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	err = c.cfg.Poster.PutUsage(pctx, o, r)
	cancel()
	var he *officers.HQError
	switch {
	case err == nil:
		c.result(o, instance, resultPushed, "")
	case errors.As(err, &he) && he.Status == 404:
		metrics.OfficerUsagePush.WithLabelValues(resultHQNotFound).Inc()
		c.mu.Lock()
		first := !c.hqAbsent[o.ID]
		c.hqAbsent[o.ID] = true
		c.mu.Unlock()
		if first {
			c.log.Printf("officers: %s: HQ answered 404 to the usage push: its HQ has no usage route yet (or does not know the officer); "+
				"pushes continue every %s and are dropped until it does", o.ID, c.cfg.Interval)
		}
	case errors.As(err, &he):
		c.result(o, instance, resultHQRefused, fmt.Sprintf("officers: %s: instance %s: HQ refused the usage push (%d)", o.ID, instance, he.Status))
	default:
		c.result(o, instance, resultHQUnavailable, fmt.Sprintf("officers: %s: instance %s: HQ did not take the usage push (unavailable or timed out)", o.ID, instance))
	}
}

// result counts one outcome and logs it when instance's state changes: a
// failure when it starts (msg), and the return to pushing.
func (c *Collector) result(o officers.Officer, instance, result, msg string) {
	metrics.OfficerUsagePush.WithLabelValues(result).Inc()
	class := result
	if result == resultPushed {
		class = stateOK
	}
	c.mu.Lock()
	prev, seen := c.state[instance]
	c.state[instance] = class
	if result == resultPushed {
		delete(c.hqAbsent, o.ID)
	}
	c.mu.Unlock()
	switch {
	case class == stateOK && seen && prev != stateOK:
		c.log.Printf("officers: %s: instance %s: its Claude usage is pushed to HQ again", o.ID, instance)
	case class != stateOK && prev != class:
		c.log.Print(msg)
	}
}

// capBuckets is bs sorted by hour then session, and at most
// officers.MaxUsageBuckets of them: the newest.
func capBuckets(bs []cctranscript.UsageBucket) []cctranscript.UsageBucket {
	out := make([]cctranscript.UsageBucket, len(bs))
	for i, b := range bs {
		b.Hour = b.Hour.UTC()
		out[i] = b
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Hour.Equal(out[j].Hour) {
			return out[i].Hour.Before(out[j].Hour)
		}
		return out[i].Session < out[j].Session
	})
	if n := len(out) - officers.MaxUsageBuckets; n > 0 {
		out = out[n:]
	}
	return out
}

// poolOf is the report's pool: the pool's tier for a tiered officer, "" for
// an untiered one (whose Tier field means nothing).
func poolOf(o officers.Officer) string {
	if o.Tiers == nil {
		return ""
	}
	return o.Tier
}
