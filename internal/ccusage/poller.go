package ccusage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// Why an account's latest attempt did not produce a reading (Row.ErrorKind).
// "" is a good reading.
const (
	// KindNoToken: the account has no token to read with. No call is made.
	KindNoToken = "no_token"
	// KindUnauthorized: the PROBE answered 401 — the token cannot make an
	// inference call at all. Not read again until its token changes.
	KindUnauthorized = "unauthorized"
	// KindForbidden: the probe answered 403. Not read again until its token
	// changes. (A 403 from the usage endpoint is not this: it only retires the
	// endpoint for the token, Row.UsageAPI.)
	KindForbidden = "forbidden"
	// KindRateLimited: the probe answered 429. The account backs off
	// (Row.NextAt), doubling per consecutive 429, at least Retry-After.
	KindRateLimited = "rate_limited"
	// KindCooling: the account pool marks the account rate-limited (a turn on
	// it hit its limit) and its cooldown has not ended, so it is not probed
	// until then (Row.NextAt).
	KindCooling = "cooling"
	// KindError: anything else (a 5xx, a network error, an answer with no
	// rate-limit headers). The next pass tries again.
	KindError = "error"
)

// Kinds is every ErrorKind a row may hold ("" included), in the order the
// migration's CHECK spells them (TestKindsMatchTheMigration).
var Kinds = []string{"", KindNoToken, KindUnauthorized, KindForbidden, KindRateLimited, KindCooling, KindError}

// The usage endpoint's state for an account's CURRENT token (Row.UsageAPI).
const (
	// UsageAPIUntried: never called with this token; the next read makes the
	// one discovery call.
	UsageAPIUntried = ""
	// UsageAPIOK: a call with this token succeeded; it is read that way.
	UsageAPIOK = "ok"
	// UsageAPIRefused: the endpoint refused this token (401/403, a setup-token
	// without the profile scope); it is never called with it again.
	UsageAPIRefused = "refused"
)

// Poll interval bounds and default (MUSTER_CC_USAGE_POLL_INTERVAL). The probe
// spends a token of quota per account per read, hence 15 minutes.
const (
	DefaultInterval = 15 * time.Minute
	MinInterval     = 5 * time.Minute
	MaxInterval     = 24 * time.Hour
	// MaxBackoff caps a rate-limited account's wait.
	MaxBackoff = 6 * time.Hour
	// maxError bounds the stored error sentence (the migration's CHECK).
	maxError = 300
)

// Row is the latest reading of one of an owner's accounts, and what the latest
// attempt made of it. Numbers and short reasons only: no token, no body.
type Row struct {
	Account string
	// Windows, Source, Status, RepresentativeClaim and ReadAt are the latest
	// GOOD reading (kept through later failed attempts); ReadAt zero for never.
	Windows map[string]Window
	// Source is where the reading came from: SourceUsageAPI or
	// SourceRateLimitHeaders ("" for none yet).
	Source string
	// Status and RepresentativeClaim are the probe's overall
	// `anthropic-ratelimit-unified-status` / `-representative-claim` ("" from
	// the usage endpoint).
	Status              string
	RepresentativeClaim string
	ReadAt              time.Time
	// CheckedAt is the latest attempt, good or not.
	CheckedAt time.Time
	// ErrorKind and Error say why the latest attempt produced no reading
	// (Kind*); both "" after a good one.
	ErrorKind string
	Error     string
	// TokenFP is the ccpool.Fingerprint of the token the latest attempt used:
	// a refusal, a backoff and UsageAPI hold only while the token is unchanged.
	TokenFP string
	// UsageAPI is the usage endpoint's state for this token (UsageAPI*), and
	// UsageAPINextAt when it may be called again after a 429 from it.
	UsageAPI       string
	UsageAPINextAt time.Time
	// RateLimitStreak is the probe's consecutive 429s; NextAt the earliest next
	// attempt of any kind (zero: the next pass).
	RateLimitStreak int
	NextAt          time.Time
}

// Store keeps the latest Row per (owner, account).
type Store interface {
	Latest(ctx context.Context, owner int64) (map[string]Row, error)
	Save(ctx context.Context, owner int64, r Row) error
}

// Result is one call of the usage endpoint or of the probe.
type Result struct {
	// Status is the HTTP status; 0 when no answer came (Err set).
	Status int
	// Reading is the parsed answer: the usage endpoint's body on a 200, or the
	// probe's rate-limit headers on any status (HasReading says it had them).
	Reading    Reading
	HasReading bool
	// RetryAfter is a 429's Retry-After, when it stated seconds.
	RetryAfter time.Duration
	// Err is a transport failure or an unreadable 200. It never carries the
	// token or a body.
	Err error
}

// Fetcher makes the two calls with one token.
type Fetcher interface {
	// Usage calls the usage endpoint.
	Usage(ctx context.Context, token string) Result
	// Probe makes the one-token Messages call and reads its headers.
	Probe(ctx context.Context, token string) Result
}

// HTTPFetcher is the real Fetcher.
type HTTPFetcher struct {
	Client *http.Client
	// URL and ProbeURL are the endpoints; "" is [Endpoint] / [ProbeURL].
	URL      string
	ProbeURL string
}

func (f HTTPFetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// do sends req with token and returns the status, headers and (bounded) body.
//
// 🔴 THE TOKEN IS IN ONE HEADER AND NOWHERE ELSE. A transport error from
// net/http names the URL, never a header; even so, only its class is kept
// (errClass), so no error this returns can carry anything the request held.
// No body is ever logged or returned in an error.
func (f HTTPFetcher) do(ctx context.Context, req *http.Request, token string) (Result, http.Header, []byte) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", BetaHeader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "muster-ccusage")
	resp, err := f.client().Do(req)
	if err != nil {
		return Result{Err: fmt.Errorf("ccusage: request failed (%s)", errClass(ctx, err))}, nil, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody+1))
	res := Result{Status: resp.StatusCode}
	if resp.StatusCode == http.StatusTooManyRequests {
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			res.RetryAfter = time.Duration(s) * time.Second
		}
	}
	if err != nil {
		res.Err = errors.New("ccusage: the answer could not be read")
	}
	return res, resp.Header, body
}

// Usage implements Fetcher.
func (f HTTPFetcher) Usage(ctx context.Context, token string) Result {
	u := f.URL
	if u == "" {
		u = Endpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{Err: errors.New("ccusage: build the request")}
	}
	res, _, body := f.do(ctx, req, token)
	if res.Status != http.StatusOK || res.Err != nil {
		return res
	}
	if len(body) > MaxResponseBody {
		res.Err = fmt.Errorf("ccusage: the answer is over %d bytes", MaxResponseBody)
		return res
	}
	r, err := Parse(body)
	if err != nil {
		res.Err = err
		return res
	}
	res.Reading, res.HasReading = r, true
	return res
}

// probeBody is the probe's request: one output token, one user message.
var probeBody = []byte(`{"model":"` + ProbeModel + `","max_tokens":1,"messages":[{"role":"user","content":"."}]}`)

// Probe implements Fetcher. The answer's body is read (and dropped) only so
// the connection can be reused; its headers are the reading.
func (f HTTPFetcher) Probe(ctx context.Context, token string) Result {
	u := f.ProbeURL
	if u == "" {
		u = ProbeURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(probeBody))
	if err != nil {
		return Result{Err: errors.New("ccusage: build the request")}
	}
	req.Header.Set("anthropic-version", anthropicVersion)
	res, h, _ := f.do(ctx, req, token)
	if h != nil {
		res.Reading, res.HasReading = ParseRateLimitHeaders(h)
	}
	return res
}

func errClass(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "transport error"
}

// Poller reads every account of Owner on each pass and saves one Row per
// account.
type Poller struct {
	// Source is the account pool's source (internal/credentials.Plane): the
	// accounts, and their tokens, read at each pass.
	Source ccpool.Source
	Store  Store
	Fetch  Fetcher
	// Cooling is when the account pool's rate-limit cooldown for an account
	// ends (ccpool.Pool.CoolingUntil; zero when it is not cooling). nil: no
	// account is ever cooling. A read error is logged and treated as cooling
	// for one interval: the probe spends quota, so it fails toward not probing.
	Cooling func(ctx context.Context, owner int64, account string) (time.Time, error)
	// Owner is the user whose accounts are read: the operator (user 1), whose
	// environment pool this is. Members' own accounts are not polled.
	Owner    int64
	Interval time.Duration
	Logger   *log.Logger
	Now      func() time.Time
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Poller) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}

// Run polls at once, then every Interval, until ctx ends. It acts as the
// operator (authz.Operator): the rows are the operator's, under row-level
// security, and nobody else's are read.
func (p *Poller) Run(ctx context.Context) {
	ctx = authz.Operator(ctx)
	iv := p.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := p.Pass(ctx); err != nil {
			p.logf("ccusage: pass: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass reads each of Owner's accounts once (those not held back by Step) and
// saves what changed. ctx must act as Owner.
func (p *Poller) Pass(ctx context.Context) error {
	acc, err := p.Source.Accounts(ctx, p.Owner)
	if err != nil {
		return fmt.Errorf("read the accounts: %w", err)
	}
	prev, err := p.Store.Latest(ctx, p.Owner)
	if err != nil {
		return fmt.Errorf("read the latest readings: %w", err)
	}
	names := make([]string, 0, len(acc))
	for n := range acc {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		now := p.now()
		var cooling time.Time
		if p.Cooling != nil {
			c, err := p.Cooling(ctx, p.Owner, n)
			if err != nil {
				p.logf("ccusage: account %s: read the pool's cooldown (not probing it this pass): %v", n, err)
				c = now.Add(p.interval())
			}
			cooling = c
		}
		before := prev[n]
		row, changed := Step(ctx, before, n, acc[n], now, StepConfig{Interval: p.Interval, CoolingUntil: cooling}, p.Fetch)
		if !changed {
			continue
		}
		if row.ErrorKind != before.ErrorKind || row.Error != before.Error {
			if row.ErrorKind == "" {
				p.logf("ccusage: account %s: reading again (%s)", n, row.Source)
			} else {
				p.logf("ccusage: account %s: %s", n, row.Error)
			}
		}
		if err := p.Store.Save(ctx, p.Owner, row); err != nil {
			p.logf("ccusage: account %s: save: %v", n, err)
		}
	}
	return nil
}

func (p *Poller) interval() time.Duration {
	if p.Interval <= 0 {
		return DefaultInterval
	}
	return p.Interval
}

// StepConfig is what Step needs beside the row: the poll interval, and when
// the account pool's cooldown for the account ends (zero: not cooling).
type StepConfig struct {
	Interval     time.Duration
	CoolingUntil time.Time
}

// Step is one account's turn of a pass, pure but for fetch: the row to save
// and whether there is one (false: nothing was attempted, prev stands).
//
//   - No token: KindNoToken, no call.
//   - A 401 or 403 from the PROBE on this token: no call until the token is
//     replaced (a dead token costs one call, not one per interval).
//   - A wait still running (NextAt after now) on this token: no call.
//   - The usage endpoint first, when this token has never been tried on it
//     (one discovery call) or a call with it succeeded. A 2xx is the reading
//     (SourceUsageAPI). A 401/403 retires the endpoint for this token and a
//     429 rests it until Retry-After; both fall through to the probe.
//   - The probe, unless the pool marks the account cooling (KindCooling, no
//     call until the cooldown ends). Its rate-limit headers are the reading
//     (SourceRateLimitHeaders); an overall status `rejected` waits until its
//     reset. A 429 backs off (Backoff, at least Retry-After).
//
// A failed attempt keeps the last good reading; a 429 whose headers carry a
// reading stores it, since those numbers are current.
func Step(ctx context.Context, prev Row, account, token string, now time.Time, cfg StepConfig, fetch Fetcher) (Row, bool) {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	r := prev
	r.Account = account
	if strings.TrimSpace(token) == "" {
		if prev.ErrorKind == KindNoToken {
			return prev, false
		}
		r.CheckedAt, r.TokenFP, r.ErrorKind = now, "", KindNoToken
		r.Error = "the account has no token in the pool"
		r.UsageAPI, r.UsageAPINextAt = UsageAPIUntried, time.Time{}
		r.RateLimitStreak, r.NextAt = 0, time.Time{}
		return r, true
	}
	fp := ccpool.Fingerprint(token)
	if fp == prev.TokenFP {
		if prev.ErrorKind == KindUnauthorized || prev.ErrorKind == KindForbidden {
			return prev, false
		}
		if now.Before(prev.NextAt) {
			return prev, false
		}
	} else {
		// A replaced token starts over: untried on the usage endpoint, no backoff.
		r.UsageAPI, r.UsageAPINextAt = UsageAPIUntried, time.Time{}
		r.RateLimitStreak, r.NextAt = 0, time.Time{}
	}
	r.TokenFP = fp
	r.CheckedAt = now
	tried := false // whether this step called anything (else nothing changes but the note)

	if r.UsageAPI != UsageAPIRefused && !now.Before(r.UsageAPINextAt) {
		tried = true
		res := fetch.Usage(ctx, token)
		switch {
		case res.Status >= 200 && res.Status < 300 && res.Err == nil && res.HasReading:
			r.UsageAPI, r.UsageAPINextAt = UsageAPIOK, time.Time{}
			good(&r, res.Reading, SourceUsageAPI, now)
			return r, true
		case res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden:
			r.UsageAPI = UsageAPIRefused
		case res.Status == http.StatusTooManyRequests:
			r.UsageAPINextAt = now.Add(maxDur(res.RetryAfter, interval))
		}
	}

	if now.Before(cfg.CoolingUntil) {
		r.ErrorKind = KindCooling
		r.Error = "the account pool marks the account rate-limited until " + cfg.CoolingUntil.UTC().Format(time.RFC3339) + "; it is not probed until then"
		r.NextAt = cfg.CoolingUntil
		if !tried && prev.ErrorKind == KindCooling && prev.NextAt.Equal(r.NextAt) && prev.TokenFP == fp {
			return prev, false
		}
		return r, true
	}

	res := fetch.Probe(ctx, token)
	switch {
	case res.Status >= 200 && res.Status < 300 && res.Err == nil && res.HasReading:
		good(&r, res.Reading, SourceRateLimitHeaders, now)
		if res.Reading.Status == "rejected" && res.Reading.ResetsAt != nil && res.Reading.ResetsAt.After(now) {
			r.NextAt = *res.Reading.ResetsAt
		}
	case res.Status >= 200 && res.Status < 300 && res.Err == nil:
		r.ErrorKind, r.RateLimitStreak, r.NextAt = KindError, 0, time.Time{}
		r.Error = "the probe's answer carried no rate-limit headers"
	case res.Status == http.StatusUnauthorized:
		r.ErrorKind, r.RateLimitStreak, r.NextAt = KindUnauthorized, 0, time.Time{}
		r.Error = "the probe was refused (HTTP 401): the token cannot make an inference call; it is not read again until it is replaced"
	case res.Status == http.StatusForbidden:
		r.ErrorKind, r.RateLimitStreak, r.NextAt = KindForbidden, 0, time.Time{}
		r.Error = "the probe was refused (HTTP 403); the token is not read again until it is replaced"
	case res.Status == http.StatusTooManyRequests:
		r.RateLimitStreak = prev.RateLimitStreak + 1
		if fp != prev.TokenFP {
			r.RateLimitStreak = 1
		}
		r.NextAt = now.Add(Backoff(interval, r.RateLimitStreak, res.RetryAfter))
		if res.HasReading {
			// The 429's own headers are current numbers: keep them.
			r.Windows, r.Source, r.ReadAt = res.Reading.Windows, SourceRateLimitHeaders, now
			r.Status, r.RepresentativeClaim = res.Reading.Status, res.Reading.RepresentativeClaim
			if rs := res.Reading.ResetsAt; res.Reading.Status == "rejected" && rs != nil && rs.After(r.NextAt) {
				r.NextAt = *rs // the account is out of quota: not probed until its window resets
			}
		}
		r.ErrorKind = KindRateLimited
		r.Error = fmt.Sprintf("the probe answered 429 (%d in a row); next read after %s", r.RateLimitStreak, r.NextAt.UTC().Format(time.RFC3339))
	default:
		r.ErrorKind, r.RateLimitStreak, r.NextAt = KindError, 0, time.Time{}
		switch {
		case res.Err != nil:
			r.Error = res.Err.Error()
		default:
			r.Error = fmt.Sprintf("the probe answered HTTP %d", res.Status)
		}
	}
	if len(r.Error) > maxError {
		r.Error = r.Error[:maxError]
	}
	return r, true
}

// good stores a reading on r and clears its error and backoff.
func good(r *Row, rd Reading, source string, now time.Time) {
	r.Windows, r.Source, r.ReadAt = rd.Windows, source, now
	r.Status, r.RepresentativeClaim = rd.Status, rd.RepresentativeClaim
	r.ErrorKind, r.Error = "", ""
	r.RateLimitStreak, r.NextAt = 0, time.Time{}
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// Backoff is a rate-limited account's wait after its streak-th consecutive
// 429: interval doubled per 429 (interval·2^streak), at least retryAfter, at
// most MaxBackoff.
func Backoff(interval time.Duration, streak int, retryAfter time.Duration) time.Duration {
	if streak < 1 {
		streak = 1
	}
	wait := interval
	for i := 0; i < streak && wait < MaxBackoff; i++ {
		wait *= 2
	}
	if retryAfter > wait {
		wait = retryAfter
	}
	if wait > MaxBackoff {
		wait = MaxBackoff
	}
	return wait
}
