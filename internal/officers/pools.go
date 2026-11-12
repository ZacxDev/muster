package officers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
)

// PUT /officers/{id}/pools: the officer's DECLARED pools as muster runs them
// (docs/officers.md, "Pools to HQ"), with the officer's own token. The body is
// the officer's whole set — HQ deletes a pool the body does not name — so an
// officer reconfigured without a tier stops reporting it at the next push.

// PoolReport is one pool on the wire.
//
//   - Pool is the pool's tier: TierRoutine, TierJudgement, or "" for an
//     untiered officer (its one pool).
//   - Model is the pool's configured model ("" = the CLI's default).
//   - MaxInstances is the pool's max_instances.
//   - MaxSessionsPerDay is the judgement pool's daily session cap, null for a
//     pool without one.
//   - SessionsToday is how many sessions the pool used on the current UTC day
//     by THE counter that enforces the cap (JudgementLedger.Usage, the count
//     the "judgement cap reached (N/M …)" line prints); 0 for an uncapped pool,
//     which keeps no count.
//   - SessionsDayResetsAt is when that day ends: the next UTC midnight.
type PoolReport struct {
	Pool                string `json:"pool"`
	Model               string `json:"model"`
	MaxInstances        int    `json:"max_instances"`
	MaxSessionsPerDay   *int   `json:"max_sessions_per_day"`
	SessionsToday       int    `json:"sessions_today"`
	SessionsDayResetsAt string `json:"sessions_day_resets_at"`
}

// PoolsReport is the pools route's body; Pools is never null.
type PoolsReport struct {
	Pools      []PoolReport `json:"pools"`
	ReportedAt string       `json:"reported_at"`
	// Dropped are the configured models sent as "" because HQ would refuse
	// them (HQModelPattern); not on the wire.
	Dropped []string `json:"-"`
}

// HQModelPattern is THE model id HQ takes on an officer's routes (a pool's
// model, a usage bucket's): HQ refuses the whole request over any other.
var HQModelPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// PutPools is PUT /officers/{id}/pools.
func (c *Client) PutPools(ctx context.Context, r PoolsReport) error {
	if r.Pools == nil {
		r.Pools = []PoolReport{}
	}
	_, err := c.Do(ctx, http.MethodPut, c.officerPath()+"/pools", r)
	return err
}

// PoolsReportFor is officer o's pools report at now: every pool of o
// (Officer.Pools: an untiered officer's one, else judgement then routine),
// each day's session count read through ledger (nil: none counted). A ledger
// read failure is returned: a report with a wrong sessions_today is not sent.
func PoolsReportFor(ctx context.Context, o Officer, ledger *JudgementLedger, now time.Time) (PoolsReport, error) {
	reset := CapResetAt(now).Format(time.RFC3339)
	var rep PoolsReport
	pools := []PoolReport{}
	for _, p := range o.Pools() {
		pr := PoolReport{Pool: p.Tier, Model: p.Model, MaxInstances: p.MaxInstances, SessionsDayResetsAt: reset}
		if pr.Model != "" && !HQModelPattern.MatchString(pr.Model) {
			// muster takes a model HQ does not ("opus[1m]"), and HQ refuses the
			// WHOLE body over it: the pool is reported on HQ's default instead.
			rep.Dropped = append(rep.Dropped, pr.Model)
			pr.Model = ""
		}
		if c := p.SessionCap(); c > 0 {
			pr.MaxSessionsPerDay = &c
		}
		used, _, err := ledger.Usage(ctx, p, now)
		if err != nil {
			return PoolsReport{}, err
		}
		pr.SessionsToday = used
		pools = append(pools, pr)
	}
	// reported_at is now - reportedAtLead, on the second (RFC 3339 without
	// fractions): HQ refuses one in its future, and a small clock skew must
	// not make it one.
	rep.Pools, rep.ReportedAt = pools, now.Add(-reportedAtLead).UTC().Format(time.RFC3339)
	return rep, nil
}

// reportedAtLead is how far behind now a pools report dates itself.
const reportedAtLead = 2 * time.Second

// PoolsPoster is the HQ side of the pools push.
type PoolsPoster interface {
	PutPools(ctx context.Context, o Officer, r PoolsReport) error
}

type backendPoolsPoster struct{ b *Backends }

// NewBackendPoolsPoster is a PoolsPoster over each officer's own backend. A
// backend that does not serve pools answers ErrNotServed.
func NewBackendPoolsPoster(b *Backends) PoolsPoster { return backendPoolsPoster{b: b} }

func (p backendPoolsPoster) PutPools(ctx context.Context, o Officer, r PoolsReport) error {
	pb, err := Serves[PoolsBackend](p.b.Backend(o), CapPools)
	if err != nil {
		return err
	}
	return pb.PutPools(ctx, r)
}

// DefaultPoolsInterval is the pools push's cadence: the usage collector's.
const DefaultPoolsInterval = 10 * time.Minute

// DefaultPoolsBackoff is the waits before each retry of a pools push HQ did
// not take (a 5xx, a timeout, no answer).
var DefaultPoolsBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

// The results, the label values of muster_officer_pools_push_total.
const (
	poolsPushed      = "pushed"
	poolsRefused     = "hq_refused"
	poolsUnavailable = "hq_unavailable"
	poolsLedger      = "ledger_failed"
)

// PoolsReporterConfig is NewPoolsReporter's input. Zero values take the
// defaults; a nil Backoff is DefaultPoolsBackoff, an empty one no retry.
type PoolsReporterConfig struct {
	Officers []Officer
	Poster   PoolsPoster
	Ledger   *JudgementLedger
	Logger   *log.Logger
	Now      func() time.Time
	Interval time.Duration
	Backoff  []time.Duration
	Sleep    func(ctx context.Context, d time.Duration) error
	// RequestTimeout bounds one PUT (DefaultHQTimeout).
	RequestTimeout time.Duration
}

// PoolsReporter pushes every declared officer's pools to its HQ: once when it
// starts (boot — MUSTER_OFFICERS is read only at boot, so a configuration
// change is a restart and this push carries it), then every Interval, so
// sessions_today stays fresh.
//
// 🔴 NO RETRY STORM: a 4xx is not retried (the same body would be refused
// again) and is logged once per officer until HQ's answer changes; a 5xx or
// no answer is retried after each Backoff wait, then left to the next tick.
type PoolsReporter struct {
	cfg PoolsReporterConfig
	mu  sync.Mutex
	// state is each officer's last outcome class ("" = pushed), so a
	// failure is logged when it starts and once more when it ends.
	state map[string]string
	// dropLogged is each officer whose unsendable model was logged, once.
	dropLogged map[string]bool
}

// NewPoolsReporter builds a reporter.
func NewPoolsReporter(cfg PoolsReporterConfig) (*PoolsReporter, error) {
	if cfg.Poster == nil {
		return nil, errors.New("officers: a pools reporter needs a poster")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultPoolsInterval
	}
	if cfg.Backoff == nil {
		cfg.Backoff = DefaultPoolsBackoff
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultHQTimeout
	}
	return &PoolsReporter{cfg: cfg, state: map[string]string{}, dropLogged: map[string]bool{}}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run pushes at once, then every Interval until ctx ends.
func (r *PoolsReporter) Run(ctx context.Context) {
	r.tick(ctx)
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *PoolsReporter) tick(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			r.cfg.Logger.Printf("officers: PANIC in the pools reporter: %v", rec)
		}
	}()
	r.Tick(ctx)
}

// Tick pushes every officer's pools once.
func (r *PoolsReporter) Tick(ctx context.Context) {
	for _, o := range r.cfg.Officers {
		if ctx.Err() != nil {
			return
		}
		r.push(ctx, o)
	}
}

func (r *PoolsReporter) push(ctx context.Context, o Officer) {
	rep, err := PoolsReportFor(ctx, o, r.cfg.Ledger, r.cfg.Now())
	if err != nil {
		r.result(o, poolsLedger, poolsLedger, fmt.Sprintf("officers: %s: its pools were not pushed to HQ: the session count could not be read: %v", o.ID, err))
		return
	}
	if len(rep.Dropped) > 0 {
		r.mu.Lock()
		first := !r.dropLogged[o.ID]
		r.dropLogged[o.ID] = true
		r.mu.Unlock()
		if first {
			r.cfg.Logger.Printf("officers: %s: model %q is not one HQ takes (%s): its pool is reported to HQ with no model (HQ's default)",
				o.ID, strings.Join(rep.Dropped, `", "`), HQModelPattern)
		}
	}
	for attempt := 0; ; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
		err = r.cfg.Poster.PutPools(pctx, o, rep)
		cancel()
		var he *HQError
		switch {
		case err == nil:
			r.result(o, poolsPushed, "", fmt.Sprintf("officers: %s: its pools are pushed to HQ again", o.ID))
			return
		case errors.As(err, &he):
			r.result(o, poolsRefused, fmt.Sprintf("refused %d", he.Status),
				fmt.Sprintf("officers: %s: HQ refused the pools push (%d); not retried, pushed again every %s and logged again only when HQ's answer changes",
					o.ID, he.Status, r.cfg.Interval))
			return
		}
		if attempt >= len(r.cfg.Backoff) || ctx.Err() != nil || r.cfg.Sleep(ctx, r.cfg.Backoff[attempt]) != nil {
			r.result(o, poolsUnavailable, poolsUnavailable,
				fmt.Sprintf("officers: %s: HQ did not take the pools push after %d attempts (unavailable or timed out); the next tick tries again", o.ID, attempt+1))
			return
		}
	}
}

// result counts one outcome and logs officer o's state when it changes: msg
// when a failure class starts, and when pushing recovers from one.
func (r *PoolsReporter) result(o Officer, result, class, msg string) {
	metrics.OfficerPoolsPush.WithLabelValues(result).Inc()
	r.mu.Lock()
	prev, seen := r.state[o.ID]
	r.state[o.ID] = class
	r.mu.Unlock()
	if prev != class && (class != "" || seen) {
		r.cfg.Logger.Print(msg)
	}
}
