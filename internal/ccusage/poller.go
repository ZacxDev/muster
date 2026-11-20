package ccusage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
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
	// KindCooling: the account pool's cooldown for the account could not be
	// read, so it is not probed this pass (the probe spends quota; this fails
	// toward not probing). An account the pool DOES read as cooling is read
	// like any other — that is how its mark gets cleared (LiveUsable).
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
	// Cooling is the account pool's rate-limit cooldown for an account
	// (ccpool.Pool.Cooldown; zero Until when it is not cooling). A read error
	// is logged and the account is not probed this pass: the probe spends
	// quota, so it fails toward not probing.
	Cooling func(ctx context.Context, owner int64, account string) (ccpool.Cooldown, error)
	// ClearCooling clears the pool's rate-limit mark of an account whose LIVE
	// reading shows it usable (LiveUsable) — only while the mark is still the
	// one read before the reading (markedAt), so a newer one stands
	// (ccpool.Pool.ClearRateLimit). nil: no mark is ever cleared here.
	ClearCooling func(ctx context.Context, owner int64, account string, markedAt time.Time) (bool, error)
	// Owner is the user whose accounts are read when Source is not
	// [PoolOwned]. Members' own accounts are not polled.
	Owner    int64
	Interval time.Duration
	Logger   *log.Logger
	Now      func() time.Time
	// Reading, when set, is told each GOOD live reading a pass takes (no
	// ErrorKind, taken by this attempt), with the detail of the account's
	// rate-limit mark as read before it ("" when not cooling): internal/
	// limitpark, which tells a pane still on an old usage-limit menu from a
	// live limit by it. It must not block.
	Reading func(ctx context.Context, owner int64, account string, row Row, markDetail string)
	// AfterPass, when set, runs once after each pass (Run), whether or not the
	// pass read anything: the usage pusher (Pusher.PushAfterPass,
	// MUSTER_CC_USAGE_PUSH_URL). It runs in the poll loop, so one cycle makes
	// at most one call of it, and a slow one delays the next pass rather than
	// overlapping it.
	AfterPass func(ctx context.Context)
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

// Run polls at once, then every Interval, until ctx ends.
//
// 🔴 IT ACTS AS authz.System, AND READS ONE OWNER'S ROWS. The owner is the
// pool's ([PoolOwned]), derived per pass — the lowest-id enabled admin, not a
// fixed user 1 — so no fixed principal can be its own. Every Store query
// names that owner (PG), as do the pool's marks queries, so the system scope
// reaches the pool owner's usage and marks rows and nobody else's.
func (p *Poller) Run(ctx context.Context) {
	ctx = authz.System(ctx)
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
		if p.AfterPass != nil && ctx.Err() == nil {
			p.AfterPass(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PoolOwned is a [ccpool.Source] whose shared pool is kept under a DERIVED
// owner: internal/credentials.Plane, whose EnvPoolOwner is also the owner its
// MarksOwner files the pool's marks under.
type PoolOwned interface {
	EnvPoolOwner(ctx context.Context) (int64, bool)
}

// owner is the owner a pass reads.
//
// 🔴 A [PoolOwned] Source NAMES IT, AND Owner IS THEN NOT READ. The pool's
// marks are kept under the Source's EnvPoolOwner, so the accounts this poller
// reads, the cooldowns it reads and clears and the readings it saves are that
// owner's — the marks it clears are the marks the selection reads. It is read
// off the Source itself rather than wired beside it, so the two cannot be
// handed different answers. false: no owner, and the pass reads nothing.
func (p *Poller) owner(ctx context.Context) (int64, error) {
	po, ok := p.Source.(PoolOwned)
	if !ok {
		return p.Owner, nil
	}
	o, ok := po.EnvPoolOwner(ctx)
	if !ok || o <= 0 {
		return 0, errors.New("the server pool has no owner to read (no enabled admin could be read)")
	}
	return o, nil
}

// Pass reads each of the owner's accounts once (those not held back by Step)
// and saves what changed. ctx must be able to read and write that owner's rows
// (Run: the system scope).
func (p *Poller) Pass(ctx context.Context) error {
	owner, err := p.owner(ctx)
	if err != nil {
		return err
	}
	acc, err := p.Source.Accounts(ctx, owner)
	if err != nil {
		return fmt.Errorf("read the accounts: %w", err)
	}
	prev, err := p.Store.Latest(ctx, owner)
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
		sc := StepConfig{Interval: p.Interval}
		// 🔴 cd IS READ BEFORE THE READING IS TAKEN: its MarkedAt is what the
		// clear below compares against, so a mark a turn writes while the
		// probe is in flight (newer) is never the one cleared.
		var cd ccpool.Cooldown
		switch c, err := p.cooling(ctx, owner, n); {
		case err != nil:
			p.logf("ccusage: account %s: read the pool's cooldown (not probing it this pass): %v", n, err)
			sc.CoolingUntil, sc.CoolingUnknown = now.Add(p.interval()), true
		default:
			cd = c
			sc.CoolingUntil = c.Until
		}
		before := prev[n]
		row, changed := Step(ctx, before, n, acc[n], now, sc, p.Fetch)
		if !changed {
			continue
		}
		p.clearIfUsable(ctx, owner, n, cd, row, now)
		if p.Reading != nil && row.ErrorKind == "" && row.ReadAt.Equal(now) {
			p.Reading(ctx, owner, n, row, cd.Detail)
		}
		if row.ErrorKind != before.ErrorKind || row.Error != before.Error {
			if row.ErrorKind == "" {
				p.logf("ccusage: account %s: reading again (%s)", n, row.Source)
			} else {
				p.logf("ccusage: account %s: %s", n, row.Error)
			}
		}
		if err := p.Store.Save(ctx, owner, row); err != nil {
			p.logf("ccusage: account %s: save: %v", n, err)
		}
	}
	return nil
}

// cooling is the pool's cooldown for account. 🔴 WITH NO Cooling SOURCE IT IS
// AN ERROR, so the account is not probed: the probe spends quota, and without
// the pool muster cannot tell an account that is out of it.
func (p *Poller) cooling(ctx context.Context, owner int64, account string) (ccpool.Cooldown, error) {
	if p.Cooling == nil {
		return ccpool.Cooldown{}, errors.New("no account pool to read the cooldown from")
	}
	return p.Cooling(ctx, owner, account)
}

// clearIfUsable clears the pool's rate-limit mark of an account that was
// cooling (cd, read before this step) when row — this step's reading — shows
// it usable (LiveUsable). It logs one line when it clears.
func (p *Poller) clearIfUsable(ctx context.Context, owner int64, account string, cd ccpool.Cooldown, row Row, now time.Time) {
	if cd.Until.IsZero() || cd.MarkedAt.IsZero() || p.ClearCooling == nil {
		return
	}
	reason, ok := LiveUsable(row, cd.Detail, now)
	if !ok {
		return
	}
	cleared, err := p.ClearCooling(ctx, owner, account, cd.MarkedAt)
	switch {
	case err != nil:
		p.logf("ccusage: account %s: clear its rate-limit mark: %v", account, err)
	case cleared:
		p.logf("ccusage: account %s: cleared its rate-limit mark of %s (cooldown was until %s): %s",
			account, cd.MarkedAt.UTC().Format(time.RFC3339), cd.Until.UTC().Format(time.RFC3339), reason)
	}
}

// LiveUsable reports whether r — a reading taken at now — shows the account
// usable right now for the limit its rate-limit mark names (markDetail, the
// mark's stored detail: the CLI's limit message), and says why. Only a GOOD
// reading taken at now counts (no ErrorKind, ReadAt equal to now): a kept
// older reading, a 429 or a refusal is not live evidence of anything.
//
//   - From the probe's headers (SourceRateLimitHeaders), ALL of: the overall
//     `anthropic-ratelimit-unified-status` is "allowed" or "allowed_warning";
//     the mark names the session or the weekly limit ([MarkedLimitWindow]);
//     and that limit's own window in the headers — 5h for the session limit,
//     7d for the weekly one — is present and under its limit (or past its
//     reset; [headerWindowUnder]). 🔴 The probe runs on [ProbeModel], and its
//     overall status speaks for that model only: a mark set by any OTHER limit
//     (a per-model one, "You've hit your Opus limit", or a message that names
//     no window) is never cleared here, or the account would be put back,
//     re-fail, and be marked again every interval. Such a mark ends at its
//     stated reset.
//   - From the usage endpoint (SourceUsageAPI): at least one window carries a
//     utilization, and every window that does is below 100% — or its stated
//     reset is not after now. One window at 100% that has not reset (a
//     per-model weekly one included) keeps the account limited: this reads
//     conservatively, and a mark it keeps still ends at its stated reset.
func LiveUsable(r Row, markDetail string, now time.Time) (string, bool) {
	if r.ErrorKind != "" || r.ReadAt.IsZero() || !r.ReadAt.Equal(now) {
		return "", false
	}
	switch r.Source {
	case SourceRateLimitHeaders:
		if r.Status != "allowed" && r.Status != "allowed_warning" {
			return "", false
		}
		name, abbr := MarkedLimitWindow(markDetail)
		if name == "" {
			return "", false
		}
		if w, ok := r.Windows[name]; !ok || !headerWindowUnder(w, now) {
			return "", false
		}
		return "the live probe's rate-limit status is " + r.Status + " and its " + abbr + " window is under its limit", true
	case SourceUsageAPI:
		n := 0
		for _, w := range r.Windows {
			if w.Utilization == nil {
				continue
			}
			if *w.Utilization >= 100 && (w.ResetsAt == nil || w.ResetsAt.After(now)) {
				return "", false
			}
			n++
		}
		if n > 0 {
			return "the usage endpoint reports every window below 100% (or past its reset)", true
		}
	}
	return "", false
}

// The limit messages the CLI writes that name ONE subscription window
// (internal/cctranscript limit.go and its pinned samples): "You've hit your
// session limit · …" and the earlier CLI's "5-hour limit reached ∙ …" are the
// 5-hour window; "You've hit your weekly limit · …" is the 7-day one. The
// earlier "Claude usage limit reached" names no window, and "You've hit your
// <model> limit" is a per-model one: neither matches.
var (
	sessionLimitText = regexp.MustCompile(`(?i)\byou(?:'|’)?ve hit your session limit\b|\b5-hour limit reached\b`)
	weeklyLimitText  = regexp.MustCompile(`(?i)\byou(?:'|’)?ve hit your weekly limit\b`)
)

// MarkedLimitWindow is the subscription window a rate-limit mark's detail
// names — FiveHour ("5h") for the session limit, SevenDay ("7d") for the
// weekly one — and "" for any other text (a per-model limit, a message naming
// no window, or one naming both).
func MarkedLimitWindow(detail string) (name, abbr string) {
	session, weekly := sessionLimitText.MatchString(detail), weeklyLimitText.MatchString(detail)
	switch {
	case session && !weekly:
		return FiveHour, "5h"
	case weekly && !session:
		return SevenDay, "7d"
	}
	return "", ""
}

// headerWindowUnder reports whether w, one window from the probe's headers,
// is under its limit at now: its reset has passed, or it is not `rejected`
// and either carries a utilization below 100% or, carrying none, says
// "allowed" / "allowed_warning". A window with nothing to say is not under.
func headerWindowUnder(w Window, now time.Time) bool {
	if w.ResetsAt != nil && !w.ResetsAt.After(now) {
		return true
	}
	if w.Status == "rejected" {
		return false
	}
	if w.Utilization != nil {
		return *w.Utilization < 100
	}
	return w.Status == "allowed" || w.Status == "allowed_warning"
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
	Interval time.Duration
	// CoolingUntil is when the pool's cooldown ends (zero: not cooling). A
	// cooling account is READ like any other — the live reading is what clears
	// its mark (LiveUsable) — so on its own this changes nothing in Step.
	CoolingUntil time.Time
	// CoolingUnknown: the pool's cooldown could not be read, and CoolingUntil
	// is a stand-in one interval ahead. The account is not probed this pass.
	CoolingUnknown bool
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
//   - The probe — also when the pool marks the account cooling, since its
//     live answer is what can clear the mark (it spends one output token) —
//     unless the pool's cooldown could not be read (KindCooling, no probe this
//     pass). Its rate-limit headers are the reading (SourceRateLimitHeaders);
//     an overall status `rejected` waits until its reset — except on a
//     cooling account, which is read again at the ordinary cadence (a limit
//     lifted before that reset is exactly what the reading is for). A 429
//     backs off (Backoff, at least Retry-After).
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
		// A KindCooling row's NextAt was the pool's stand-in, not a wait of
		// the account's own (and before the pool's cooldown stopped holding the
		// probe, it was the whole cooldown): the pool is asked again each pass.
		if now.Before(prev.NextAt) && prev.ErrorKind != KindCooling {
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
			good(&r, res.Reading, SourceUsageAPI, now, interval)
			return r, true
		case res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden:
			r.UsageAPI = UsageAPIRefused
		case res.Status == http.StatusTooManyRequests:
			r.UsageAPINextAt = now.Add(maxDur(res.RetryAfter, interval))
		}
	}

	if cfg.CoolingUnknown {
		r.ErrorKind = KindCooling
		r.Error = "the account pool's cooldown for the account could not be read; it is not probed this pass"
		r.NextAt = cfg.CoolingUntil
		if !tried && prev.ErrorKind == KindCooling && prev.NextAt.Equal(r.NextAt) && prev.TokenFP == fp {
			return prev, false
		}
		return r, true
	}

	// A `rejected` answer holds the account until its stated reset — but not
	// one the pool marks cooling: its live state is read at the ordinary
	// cadence, so a limit lifted early clears the mark within an interval.
	holdForReset := !now.Before(cfg.CoolingUntil)
	res := fetch.Probe(ctx, token)
	switch {
	case res.Status >= 200 && res.Status < 300 && res.Err == nil && res.HasReading:
		good(&r, res.Reading, SourceRateLimitHeaders, now, interval)
		if holdForReset && res.Reading.Status == "rejected" && res.Reading.ResetsAt != nil && res.Reading.ResetsAt.After(now) {
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
			if rs := res.Reading.ResetsAt; holdForReset && res.Reading.Status == "rejected" && rs != nil && rs.After(r.NextAt) {
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
//
// It also sets the next read (NextAt) one interval out, less a tenth so a
// ticker's drift does not skip a whole interval: persisted, so a restarted
// server does not read (and probe) every account at once.
func good(r *Row, rd Reading, source string, now time.Time, interval time.Duration) {
	r.Windows, r.Source, r.ReadAt = rd.Windows, source, now
	r.Status, r.RepresentativeClaim = rd.Status, rd.RepresentativeClaim
	r.ErrorKind, r.Error = "", ""
	r.RateLimitStreak, r.NextAt = 0, now.Add(interval-interval/10)
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
