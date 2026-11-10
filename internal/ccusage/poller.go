package ccusage

import (
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
	// KindUnauthorized: the endpoint answered 401. The account is NOT polled
	// again until its token changes (a different fingerprint).
	KindUnauthorized = "unauthorized"
	// KindForbidden: the endpoint answered 403 — expected for a token without
	// the profile scope (see the package doc). Not polled again until its
	// token changes.
	KindForbidden = "forbidden"
	// KindRateLimited: the endpoint answered 429. The account backs off
	// (Row.NextAt), doubling per consecutive 429, at least Retry-After.
	KindRateLimited = "rate_limited"
	// KindError: anything else (a 5xx, a network error, an answer that is not
	// a usage answer). The next pass tries again.
	KindError = "error"
)

// Kinds is every ErrorKind a row may hold ("" included), in the order the
// migration's CHECK spells them (TestKindsMatchTheMigration).
var Kinds = []string{"", KindNoToken, KindUnauthorized, KindForbidden, KindRateLimited, KindError}

// Poll interval bounds and default (MUSTER_CC_USAGE_POLL_INTERVAL).
const (
	DefaultInterval = 5 * time.Minute
	MinInterval     = time.Minute
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
	// Windows and ReadAt are the latest GOOD reading (kept through later
	// failed attempts); ReadAt zero for never.
	Windows map[string]Window
	ReadAt  time.Time
	// CheckedAt is the latest attempt, good or not.
	CheckedAt time.Time
	// ErrorKind and Error say why the latest attempt produced no reading
	// (Kind*); both "" after a good one.
	ErrorKind string
	Error     string
	// TokenFP is the ccpool.Fingerprint of the token the latest attempt used:
	// a 401/403 holds only while the token is unchanged.
	TokenFP string
	// RateLimitStreak is the consecutive 429s; NextAt the earliest next attempt
	// (zero: the next pass).
	RateLimitStreak int
	NextAt          time.Time
}

// Store keeps the latest Row per (owner, account).
type Store interface {
	Latest(ctx context.Context, owner int64) (map[string]Row, error)
	Save(ctx context.Context, owner int64, r Row) error
}

// Result is one call of the endpoint.
type Result struct {
	// Status is the HTTP status; 0 when no answer came (Err set).
	Status int
	// Reading is the parsed answer of a 200.
	Reading Reading
	// RetryAfter is a 429's Retry-After, when it stated seconds.
	RetryAfter time.Duration
	// Err is a transport failure or an unreadable 200. It never carries the token.
	Err error
}

// Fetcher calls the endpoint with one token.
type Fetcher interface {
	Fetch(ctx context.Context, token string) Result
}

// HTTPFetcher is the real Fetcher.
type HTTPFetcher struct {
	Client *http.Client
	// URL is the endpoint; "" is [Endpoint].
	URL string
}

// Fetch implements Fetcher.
//
// 🔴 THE TOKEN IS IN ONE HEADER AND NOWHERE ELSE. A transport error from
// net/http names the URL, never a header; even so, only its class is kept
// (errClass), so no error this returns can carry anything the request held.
func (f HTTPFetcher) Fetch(ctx context.Context, token string) Result {
	u := f.URL
	if u == "" {
		u = Endpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{Err: errors.New("ccusage: build the request")}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", BetaHeader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "muster-ccusage")
	c := f.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return Result{Err: fmt.Errorf("ccusage: request failed (%s)", errClass(ctx, err))}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody+1))
	res := Result{Status: resp.StatusCode}
	if resp.StatusCode == http.StatusTooManyRequests {
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			res.RetryAfter = time.Duration(s) * time.Second
		}
	}
	if resp.StatusCode != http.StatusOK {
		return res
	}
	if err != nil {
		res.Err = errors.New("ccusage: the answer could not be read")
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
	res.Reading = r
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
		before := prev[n]
		row, changed := Step(ctx, before, n, acc[n], p.now(), p.Interval, p.Fetch)
		if !changed {
			continue
		}
		if row.ErrorKind != before.ErrorKind || row.Error != before.Error {
			if row.ErrorKind == "" {
				p.logf("ccusage: account %s: reading again", n)
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

// Step is one account's turn of a pass, pure but for fetch: the row to save
// and whether there is one (false: nothing was attempted, prev stands).
//
//   - No token: KindNoToken, no call.
//   - A 401 or 403 on THIS token (same fingerprint): no call — a refusal is
//     not retried until the token is replaced, so a dead or inference-only
//     token costs one call, not one per interval.
//   - A backoff still running (NextAt after now) on this token: no call.
//   - Otherwise one call, and the row records what it answered. A good
//     answer replaces the windows; a failed one keeps the last good windows
//     and their ReadAt.
func Step(ctx context.Context, prev Row, account, token string, now time.Time, interval time.Duration, fetch Fetcher) (Row, bool) {
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
	}
	// A replaced token starts with no backoff: every case below sets the
	// streak and NextAt itself (a 429 on a new token counts from 1).
	res := fetch.Fetch(ctx, token)
	r.CheckedAt, r.TokenFP = now, fp
	switch {
	case res.Status == http.StatusOK && res.Err == nil:
		r.Windows, r.ReadAt = res.Reading.Windows, now
		r.ErrorKind, r.Error = "", ""
		r.RateLimitStreak, r.NextAt = 0, time.Time{}
	case res.Status == http.StatusUnauthorized:
		r.ErrorKind = KindUnauthorized
		r.Error = "the usage endpoint refused the account's token (HTTP 401); it is not read again until the token is replaced"
		r.RateLimitStreak, r.NextAt = 0, time.Time{}
	case res.Status == http.StatusForbidden:
		r.ErrorKind = KindForbidden
		r.Error = "the usage endpoint refused the account's token (HTTP 403; a setup-token may lack the profile scope the endpoint needs); it is not read again until the token is replaced"
		r.RateLimitStreak, r.NextAt = 0, time.Time{}
	case res.Status == http.StatusTooManyRequests:
		r.RateLimitStreak = prev.RateLimitStreak + 1
		if fp != prev.TokenFP {
			r.RateLimitStreak = 1
		}
		wait := Backoff(interval, r.RateLimitStreak, res.RetryAfter)
		r.NextAt = now.Add(wait)
		r.ErrorKind = KindRateLimited
		r.Error = fmt.Sprintf("the usage endpoint answered 429 (%d in a row); next read after %s", r.RateLimitStreak, r.NextAt.UTC().Format(time.RFC3339))
	default:
		r.ErrorKind = KindError
		r.RateLimitStreak, r.NextAt = 0, time.Time{}
		switch {
		case res.Err != nil && res.Status == http.StatusOK:
			r.Error = "the usage endpoint's answer could not be read: " + res.Err.Error()
		case res.Err != nil:
			r.Error = res.Err.Error()
		default:
			r.Error = fmt.Sprintf("the usage endpoint answered HTTP %d", res.Status)
		}
	}
	if len(r.Error) > maxError {
		r.Error = r.Error[:maxError]
	}
	return r, true
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
