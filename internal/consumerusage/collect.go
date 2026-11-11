package consumerusage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/metrics"
)

// ErrUnsupported is an instance whose ccd does not serve GET /v1/usage (an
// image that predates it). It is skipped quietly.
var ErrUnsupported = errors.New("consumerusage: the instance's ccd does not serve GET /v1/usage (its image predates it)")

// Reading is one instance's usage as its Source read it.
type Reading struct {
	// Account is the pool account the instance is on now.
	Account string
	// Failovers are the instance's account moves (any order), from which each
	// hour's account is told (AccountAt).
	Failovers []Failover
	Buckets   []cctranscript.UsageBucket
}

// Source reads instance's usage since since (production: its ccd's GET
// /v1/usage through the agent gateway, and its failover history). An image
// without the route is ErrUnsupported.
type Source interface {
	Read(ctx context.Context, instance string, since time.Time) (Reading, error)
}

// Poster is the HQ side (*Sender).
type Poster interface {
	Put(ctx context.Context, r Report) error
}

const (
	// DefaultInterval is the officer usage collector's cadence.
	DefaultInterval = 10 * time.Minute
	// Window is how far back each push reaches, as the officer collector's:
	// a muster restart or an HQ outage of up to a day loses nothing.
	Window = 25 * time.Hour
	// DefaultRequestTimeout bounds one read, and one push with its retries.
	DefaultRequestTimeout = 30 * time.Second
	// FlushTimeout bounds the push before a scale-to-zero, read and push
	// together.
	FlushTimeout = 10 * time.Second
)

// The results, the label values of muster_cc_consumer_usage_push_total.
const (
	resultPushed        = "pushed"
	resultEmpty         = "empty"
	resultUnsupported   = "ccd_unsupported"
	resultReadFailed    = "ccd_failed"
	resultHQRefused     = "hq_refused"
	resultHQUnavailable = "hq_unavailable"
	resultFlushSkipped  = "skipped"
)

// Config is New's input. Zero durations take the defaults.
type Config struct {
	// Consumer is the muster's consumer (ValidConsumer); Instance its label.
	Consumer, Instance string
	// Instances is THE list of this muster's instances whose usage is pushed:
	// up, on a pool account, and never an officer's.
	Instances func(ctx context.Context) ([]string, error)
	Source    Source
	Poster    Poster
	Logger    *log.Logger
	Now       func() time.Time
	Interval  time.Duration
	// RequestTimeout bounds one read, and one push with its retries.
	RequestTimeout time.Duration
}

// Collector is the consumer usage collector.
type Collector struct {
	cfg Config
	log *log.Logger

	// sem lets one collect run at a time, so a tick's push and a pre-stop
	// flush of the same instance never land at HQ out of order.
	sem chan struct{}

	mu sync.Mutex
	// state is each instance's last read result ("" = read), so a read
	// failure is logged once when it starts and once when it ends.
	state map[string]string
	// hq is HQ's last answer class ("" = took the push, "refused 404", …),
	// logged once per change: HQ is one endpoint for every instance, so a
	// refusal is logged once, not once per instance per tick.
	hq string
	// invalidLogged is each instance whose rows were left out as invalid,
	// logged once.
	invalidLogged map[string]bool
}

// New builds a collector.
func New(cfg Config) (*Collector, error) {
	if !ValidConsumer(cfg.Consumer) {
		return nil, fmt.Errorf("consumerusage: unknown consumer %q (want one of %v)", cfg.Consumer, Consumers)
	}
	if cfg.Instance == "" || cfg.Instances == nil || cfg.Source == nil || cfg.Poster == nil {
		return nil, errors.New("consumerusage: a collector needs its instance label, the instance list, a source and a poster")
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
	return &Collector{cfg: cfg, log: cfg.Logger, sem: make(chan struct{}, 1), state: map[string]string{}, invalidLogged: map[string]bool{}}, nil
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
			c.log.Printf("consumer usage: PANIC in the collector: %v", rec)
		}
	}()
	c.Tick(ctx)
}

// Tick pushes every listed instance's usage, one at a time. When HQ does
// not take a push (5xx or no answer, after its retries) the rest of the tick
// is skipped: the next tick's pushes cover it, and an HQ that is down is not
// sent one retried push per instance.
func (c *Collector) Tick(ctx context.Context) {
	names, err := c.cfg.Instances(ctx)
	if err != nil {
		c.log.Printf("consumer usage: the instances could not be listed this tick: %v", err)
		return
	}
	for _, name := range names {
		select {
		case <-ctx.Done():
			return
		case c.sem <- struct{}{}:
		}
		r := c.collect(ctx, name)
		<-c.sem
		if r == resultHQUnavailable {
			return
		}
	}
}

// Flush pushes instance's usage once, now: it is about to be scaled to zero
// or destroyed. Bounded by FlushTimeout; best-effort — counted and logged,
// nothing returned for the stop to fail on.
func (c *Collector) Flush(ctx context.Context, instance string) {
	ctx, cancel := context.WithTimeout(ctx, FlushTimeout)
	defer cancel()
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		metrics.CCConsumerUsagePush.WithLabelValues(resultFlushSkipped).Inc()
		c.log.Printf("consumer usage: instance %s: its usage was not pushed before it stopped (another push held the collector past %s)",
			instance, FlushTimeout)
		return
	}
	defer func() { <-c.sem }()
	c.collect(ctx, instance)
}

// collect reads instance's usage and pushes it, once, and returns the result.
func (c *Collector) collect(ctx context.Context, instance string) string {
	now := c.cfg.Now()
	rctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	rd, err := c.cfg.Source.Read(rctx, instance, now.Add(-Window))
	cancel()
	switch {
	case errors.Is(err, ErrUnsupported):
		return c.readResult(instance, resultUnsupported, fmt.Sprintf("consumer usage: instance %s's ccd does not serve its Claude usage "+
			"(GET /v1/usage): its image predates it; skipped until it runs a newer one", instance))
	case err != nil:
		return c.readResult(instance, resultReadFailed, fmt.Sprintf("consumer usage: instance %s: its Claude usage could not be read: %v", instance, err))
	}
	c.readResult(instance, "", fmt.Sprintf("consumer usage: instance %s: its Claude usage is read again", instance))
	rows, invalid := Rows(rd.Buckets, rd.Account, rd.Failovers)
	if invalid > 0 {
		c.mu.Lock()
		first := !c.invalidLogged[instance]
		c.invalidLogged[instance] = true
		c.mu.Unlock()
		if first {
			c.log.Printf("consumer usage: instance %s: %d usage buckets left out (a session HQ refuses, an hour not on the hour, or no account)",
				instance, invalid)
		}
	}
	if len(rows) == 0 {
		metrics.CCConsumerUsagePush.WithLabelValues(resultEmpty).Inc()
		return resultEmpty
	}
	pctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	err = c.cfg.Poster.Put(pctx, Report{Instance: c.cfg.Instance, Consumer: c.cfg.Consumer, Hours: rows})
	cancel()
	var refused *RefusedError
	switch {
	case err == nil:
		return c.hqResult(resultPushed, "", "consumer usage: HQ takes the pushes again")
	case errors.As(err, &refused):
		return c.hqResult(resultHQRefused, fmt.Sprintf("refused %d", refused.Status),
			fmt.Sprintf("consumer usage: HQ refused the push (HTTP %d); not retried, pushes continue every %s and this is logged again only when HQ's answer changes",
				refused.Status, c.cfg.Interval))
	default:
		return c.hqResult(resultHQUnavailable, "unavailable",
			fmt.Sprintf("consumer usage: HQ did not take the push (%v); the next tick tries again", err))
	}
}

// readResult counts a read failure (result non-empty) and logs instance's
// read state when it changes: msg when a failure starts, and when reading
// recovers from one. A good read ("") is not counted here.
func (c *Collector) readResult(instance, result, msg string) string {
	if result != "" {
		metrics.CCConsumerUsagePush.WithLabelValues(result).Inc()
	}
	c.mu.Lock()
	prev, seen := c.state[instance]
	c.state[instance] = result
	c.mu.Unlock()
	if prev != result && (result != "" || seen) {
		c.log.Print(msg)
	}
	return result
}

// hqResult counts a push's result and logs HQ's answer class when it
// changes: msg when a failure class starts, and when HQ takes pushes again.
func (c *Collector) hqResult(result, class, msg string) string {
	metrics.CCConsumerUsagePush.WithLabelValues(result).Inc()
	c.mu.Lock()
	prev := c.hq
	c.hq = class
	c.mu.Unlock()
	if prev != class {
		c.log.Print(msg)
	}
	return result
}
