// Package ccpool is the per-account Claude Code `setup-token` pool: which
// operator account a new claude-code agent runs on, and what each account's
// sessions have reported back.
//
// # The shape
//
// The operator has several INDIVIDUAL Pro/Max accounts and runs `claude
// setup-token` once per account. Each token is one value in muster's own
// environment (MUSTER_AGENT_CC_TOKEN_<NAME>, named by MUSTER_AGENT_CC_ACCOUNTS —
// see cmd/muster-server/config.go). This package holds that map and answers
// three questions:
//
//   - Select: which account should a NEW claude-code agent use?
//   - Token: which setup-token does a pod on account X get?
//   - MarkFailure: ccd reported the account's usage limit (a `rate_limited`
//     [AccountFailure] reads as one, never a transient 429) or `auth_failed`
//     for an agent on account X — remember it, with a timestamp, in Postgres.
//
// # Whose accounts (bring-your-own, migration 0011)
//
// The environment pool is the ADMIN's. A member's agents run only on accounts
// the member entered in muster (internal/credentials). The [Source] answers
// "which accounts may this OWNER use", and every selection, mark and live count
// is per (owner, account): there is no path from a member's dispatch to an
// environment-pool account (TestAMemberDispatchNeverSelectsAnAdminPoolAccount
// in internal/credentials). The one exception is the marks of a SHARED pool's
// accounts ([SharedAccounts]): the admin and the teams listed on the server
// pool run on the same accounts, so their marks are kept once, under the
// pool's marks owner, and every owner on the pool sees them.
//
// # Selection
//
// [Choose] is the whole algorithm, and it is a pure function so a test can pin
// it without a database:
//
//  1. Drop every account whose CURRENT token is marked auth_failed (the mark's
//     fingerprint equals [Fingerprint] of the token configured now). Replacing
//     the token in the secret therefore clears the mark by itself; nothing else
//     does. If that leaves nothing, refuse — dispatching onto a token that
//     already failed authentication builds a pod that cannot answer one turn.
//  2. An account still COOLING DOWN after a rate_limited mark (see "The
//     cooldown" below) ranks after every account that is not. It is NOT
//     dropped: dispatch never refuses for a cooldown, because an agent keeps
//     its account for life and outlives any one rate-limit window.
//  3. Least-recently-rate-limited first: never rate-limited sorts before any
//     timestamp, then oldest timestamp first.
//  4. Then fewest LIVE agents (kind claude-code, status pending, provisioning or
//     running) on the account.
//  5. Then the account name, so the answer is deterministic.
//
// # The cooldown and the per-account cap (M4)
//
// After a `rate_limited` mark an account cools down until [CooldownUntil]: the
// reset time the CLI's message states ("resets 9:45pm (UTC)", "resets Oct 9,
// 9am (UTC)") when one can be read, else the mark's time plus
// [Limits.Cooldown] (default [DefaultCooldown]). It is over at exactly that
// instant — or sooner, when THE LIVE READING SAYS THE ACCOUNT IS USABLE: the
// usage poller (internal/ccusage) keeps reading a cooling account, and when
// what the account reports right now shows it usable it clears the mark
// ([Pool.ClearRateLimit]), guarded so a NEWER mark written while it was
// reading is never cleared. A stated reset is the CLI's estimate; the live
// reading is the account's own word. With the poller off nothing ends a
// cooldown early.
//
// A rate_limited mark is about the SUBSCRIPTION, not the token: limits are
// counted per subscription, so a re-issued token on the same one is still
// limited, and replacing the token does NOT end the cooldown. A changed token
// on a cooling account is read by the live probe on the poller's next pass,
// and that reading decides.
//
// 🔴 ONLY THE JOB API AND THE FAILOVER REFUSE FOR IT. The job API's
// worker-pool choice (internal/api chooseWorker, reading
// [AccountState.CoolingUntil]) skips a worker on a cooling account — and when
// that leaves no worker it fails one such worker over to a usable account
// (internal/ccfailover) and places the job there, answering 429 only when no
// account is usable — and [ChooseFailover] never moves an agent onto a
// cooling account. Agent dispatch ([Choose]) only ranks a cooling
// account last and still picks it when it is all there is: an agent outlives a
// rate-limit window, so refusing to create it for a window of hours would buy
// nothing.
//
// [Limits.MaxConcurrentJobs] caps how many JOBS one account runs at once, and
// is OPT-IN: zero (the default) is no cap, because an account's real capacity
// is unmeasured. This package does not count jobs; it carries the cap to the
// job API's worker choice in [AccountState], and that choice enforces it.
// Agent dispatch has no cap: an agent is not a job.
//
// An explicit pin at dispatch bypasses all of these — it is the operator's override,
// including for an account the pool would skip — but must name a configured
// account.
//
// 🔴 AN AGENT KEEPS ITS ACCOUNT UNTIL AN ACCOUNT FAILOVER MOVES IT. Select runs
// once, before the agent's row exists; the row stores the NAME
// (agents.cc_account) and migration 0003's trigger refuses any later change
// except the failover's (internal/ccfailover, migration 0066), which moves an
// agent off an account that hit its usage limit or answered auth_failed onto one
// [ChooseFailover] picks. A restart, a re-provision, a model of the pool
// changing under it — none of them re-select.
//
// 🔴 [Pool.Token] IS THE ONE PLACE A TOKEN LEAVES THIS PACKAGE: the pod-spec
// path (internal/agentprovision, through internal/credentials.Plane) asks it
// for the token of the agent's OWN owner and account, and agentspec places it
// in exactly one place — the agent's own per-agent Secret, as
// CLAUDE_CODE_OAUTH_TOKEN. No error, log line or stored mark here carries it;
// a mark stores a truncated sha256 fingerprint.
//
// ⚠ ROTATING A TOKEN DOES NOT REACH A RUNNING POD. The new value is in muster's
// environment; an existing agent's Secret still holds the old one until its spec
// is re-applied — Stop then Start a claude-code agent does that
// (agentprovision.Adapter.Start). Until then its turns keep failing and keep
// re-marking the account, now under the NEW token's fingerprint.
package ccpool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/authz"
)

// The failure types ccd reports that mark an account. They are ccd's own
// spellings (cmd/ccd/failure.go: failRateLimited / failAuthFailed) — the wire's
// `error.type` — and nothing else marks.
const (
	FailureRateLimited = "rate_limited"
	FailureAuthFailed  = "auth_failed"
)

// AccountFailure is what a runtime's typed failure says about the Claude
// ACCOUNT its turn ran on: [FailureAuthFailed], [FailureRateLimited] when it
// is the account's USAGE LIMIT, or "" when it says nothing about the account.
// failureType, usageLimit and status are the typed failure's own fields
// (agents.RuntimeError Type, UsageLimit, Status), never its text.
//
// 🔴 THIS IS THE ONE PLACE THAT DECIDES WHICH rate_limited IS A USAGE LIMIT.
// Its answer, never the raw type, is what marks an account ([Pool.MarkFailure],
// from internal/agentgateway), fails an agent over (internal/ccfailover, from
// the gateway and the job runner) and tells an officer chat its accounts are
// at their limit (internal/api chatTurnUsageLimit).
//
// A rate_limited is the usage limit when:
//
//   - ccd marked it `usage_limit` (the CLI's own text said a limit was hit,
//     cctranscript.IsLimitText, or the pane shows the usage-limit menu); or
//   - its status is 409: ccd answers rate_limited with a 409 only for a pane
//     parked on the usage-limit menu (cmd/ccd limitMenuFailure), and has done
//     so since before the mark existed.
//
// Any other rate_limited — a transient request rate limit (a 429 the CLI codes
// rate_limit exactly like a usage limit) — is "": it marks nothing and moves
// no agent, and the turn's caller retries on the same account as for any other
// failed turn. Marking it would cool the account down and fail every agent on
// it over, and with one account left with room, one passing 429 would leave
// every agent with nowhere to run until another account reset.
//
// ⚠ A ccd FROM BEFORE `usage_limit` SENDS NO MARK, so its 429 reads as
// transient here even when it was a usage limit. The account is marked only
// when a turn meets the CLI's usage-limit menu: the CLI raises it in the
// session that hit the limit, and that session's next turn is refused with the
// 409 above, which every ccd that fails over sends. A turn in a FRESH session
// (each job's, a new chat's) has no menu on its pane and may 429 unmarked, but
// its retries go to the same session (internal/api runJobTurn, the officer
// chat runner), so they meet the menu if the CLI raised it. ccd stops an idle
// lane parked on the menu (cmd/ccd lanes.release) only after refusing a turn
// with that 409, so the mark is already written. Nothing else marks an account
// (the usage poller, internal/ccusage, only clears a mark): under such a ccd,
// an account whose CLI raises no menu stays unmarked until its image is
// updated.
func AccountFailure(failureType string, usageLimit bool, status int) string {
	switch failureType {
	case FailureAuthFailed:
		return FailureAuthFailed
	case FailureRateLimited:
		if usageLimit || status == http.StatusConflict {
			return FailureRateLimited
		}
	}
	return ""
}

// ErrNoUsableAccount is returned by Select when automatic selection has nothing
// to choose from. It is a dispatch REFUSAL, surfaced to the operator.
var ErrNoUsableAccount = errors.New("ccpool: no usable Claude account")

// ErrUnknownAccount is returned for a pin (or a lookup) naming an account that
// is not configured.
var ErrUnknownAccount = errors.New("ccpool: unknown Claude account")

// maxNameLen bounds an account name. It becomes part of an environment variable
// name and is shown on a card; it is not an identity anything else keys on.
const maxNameLen = 32

// ValidName reports whether name is a legal account name: 1-32 of [a-z0-9-],
// not starting or ending with '-'.
func ValidName(name string) bool {
	if name == "" || len(name) > maxNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// EnvSuffix is the part of an account's token variable after the prefix:
// "work-2" -> "WORK_2". It is the one place the name-to-variable mapping lives.
func EnvSuffix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// Fingerprint is a short, one-way identifier for a token: the first 12 hex
// characters of its sha256. It is what an auth_failed mark records so the mark
// can expire when the token is replaced, without the database holding anything
// that authenticates.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

// Mark is what the pool remembers about one account. A zero time means "never".
type Mark struct {
	Account           string
	RateLimitedAt     time.Time
	RateLimitedDetail string
	AuthFailedAt      time.Time
	AuthFailedDetail  string
	// AuthFailedToken is the [Fingerprint] of the token that failed.
	AuthFailedToken string
}

// Store persists marks and counts live agents per account — PER OWNER.
//
// 🔴 AN ACCOUNT IS (owner, name), NOT A NAME. Members bring their own accounts
// (internal/credentials), and two owners may each call one "work": they are
// different subscriptions, so a rate-limit mark on one must not push the other
// down the order, and one owner's live agents must not count against the
// other's. Every method takes the owner and every query names it (migration
// 0011 keys cc_account_marks on (owner_id, account)).
type Store interface {
	Marks(ctx context.Context, owner int64) (map[string]Mark, error)
	LiveCounts(ctx context.Context, owner int64) (map[string]int, error)
	MarkRateLimited(ctx context.Context, owner int64, account, detail string, at time.Time) error
	MarkAuthFailed(ctx context.Context, owner int64, account, fingerprint, detail string, at time.Time) error
	// ClearRateLimited clears owner's account's rate_limited mark ONLY while
	// its timestamp is still markedAt (a compare-and-clear), and reports
	// whether it did. A newer mark, written after markedAt was read, stands.
	ClearRateLimited(ctx context.Context, owner int64, account string, markedAt time.Time) (bool, error)
}

// Source answers which Claude accounts an OWNER's agents may run on: name →
// setup-token. An empty map is "this owner has none", which Select refuses
// with [ErrNoUsableAccount]. internal/credentials.Plane is the production
// Source (the environment pool for the admin, the member's own tokens for a
// member); [Static] is a fixed map.
type Source interface {
	Accounts(ctx context.Context, owner int64) (map[string]string, error)
}

// SharedAccounts is implemented by a [Source] under which several owners run on
// ONE set of accounts (internal/credentials.Plane: the admin and every team in
// MUSTER_TEAMS_ON_SERVER_POOL share the environment pool).
//
// A rate limit or an auth failure is a property of the ACCOUNT, not of whoever
// hit it: a team's first agent on the server pool must not be placed on an
// account the admin's agents already found exhausted. So the marks of a shared
// pool's accounts are kept under ONE owner, the one MarksOwner names, and every
// owner on that pool reads and writes them there. Live counts stay per owner.
type SharedAccounts interface {
	// MarksOwner reports the owner whose marks stand for owner's accounts, and
	// true, when owner's accounts are a shared pool's. false: owner's accounts
	// are its own, and so are its marks.
	MarksOwner(ctx context.Context, owner int64) (int64, bool)
}

// markScope is the context and owner the marks of owner's accounts are read
// and written under: owner itself under ctx, or — for an owner on a shared
// pool ([SharedAccounts]) — the pool's marks owner under authz.System.
//
// 🔴 A SYSTEM-LEDGER SITE, AND IT READS ACCOUNT HEALTH ONLY. cc_account_marks
// is under row-level security, so a team acting as itself cannot read the
// marks the admin's agents recorded on the same pool account. Every Store
// query names the owner (PGStore), so System here reaches exactly the pool's
// marks rows: per account a timestamp, the CLI's limit message and a token
// fingerprint — nothing of any owner's muster data. It is taken only when the
// Source says owner runs on the shared pool; an owner with accounts of its own
// (a member's bring-your-own) keeps its own principal and its own marks.
func (p *Pool) markScope(ctx context.Context, owner int64) (context.Context, int64) {
	sh, ok := p.src.(SharedAccounts)
	if !ok {
		return ctx, owner
	}
	to, shared := sh.MarksOwner(ctx, owner)
	if !shared || to <= 0 {
		return ctx, owner
	}
	return authz.System(ctx), to
}

// marks is the marks of owner's accounts, read where [Pool.markScope] says.
func (p *Pool) marks(ctx context.Context, owner int64) (map[string]Mark, error) {
	mctx, mowner := p.markScope(ctx, owner)
	return p.store.Marks(mctx, mowner)
}

// Static is a fixed account map, the same for every owner: the environment
// pool. Build it with [NewStatic], which validates it.
type Static struct{ accounts map[string]string }

// NewStatic validates an account map. It REFUSES an empty pool, an invalid
// name and an empty token, so a misconfiguration stops the process at boot
// instead of building a pod with no credential at the first dispatch.
func NewStatic(accounts map[string]string) (Static, error) {
	if len(accounts) == 0 {
		return Static{}, fmt.Errorf("%w: the pool is empty", ErrNoUsableAccount)
	}
	out := make(map[string]string, len(accounts))
	for name, tok := range accounts {
		if !ValidName(name) {
			return Static{}, fmt.Errorf("ccpool: account name %q is not 1-%d of [a-z0-9-] (not starting or ending with '-')", name, maxNameLen)
		}
		if strings.TrimSpace(tok) == "" {
			return Static{}, fmt.Errorf("ccpool: account %q has an empty token", name)
		}
		out[name] = tok
	}
	return Static{accounts: out}, nil
}

// Accounts implements [Source]: a copy, whoever asks.
func (s Static) Accounts(context.Context, int64) (map[string]string, error) {
	out := make(map[string]string, len(s.accounts))
	for k, v := range s.accounts {
		out[k] = v
	}
	return out, nil
}

// DefaultCooldown is how long an account rests after a `rate_limited` whose
// message states no reset time this package can read.
//
// ⚠ THERE IS DELIBERATELY NO DEFAULT JOB CAP. The per-account cap is opt-in
// ([Limits.MaxConcurrentJobs] zero is none): what one account sustains is not
// known until it is measured, and a guessed default would throttle silently.
const DefaultCooldown = time.Hour

// Limits are the pool's two knobs: the cooldown after a rate limit, and the
// per-account job concurrency cap.
type Limits struct {
	// Cooldown is the rest after a `rate_limited` whose message carries no
	// readable reset time. Positive.
	Cooldown time.Duration
	// MaxConcurrentJobs caps the jobs one account runs at once. ZERO IS NO CAP
	// (the default); a cap is at least 1.
	MaxConcurrentJobs int
}

// DefaultLimits is [DefaultCooldown] and no job cap.
func DefaultLimits() Limits {
	return Limits{Cooldown: DefaultCooldown}
}

// Pool is selection and marking over a per-owner [Source] and its [Store].
type Pool struct {
	src    Source
	store  Store
	now    func() time.Time
	limits Limits
}

// New builds a pool with [DefaultLimits].
func New(src Source, store Store) (*Pool, error) {
	return NewWithLimits(src, store, DefaultLimits())
}

// NewWithLimits builds a pool with explicit limits. It refuses a non-positive
// cooldown (a knob that would silently disable the cooldown) and a negative
// cap; a zero cap is "no cap".
func NewWithLimits(src Source, store Store, l Limits) (*Pool, error) {
	if src == nil {
		return nil, errors.New("ccpool: a Source is required (it says whose accounts an owner may use)")
	}
	if store == nil {
		return nil, errors.New("ccpool: a Store is required (marks and live counts are what selection reads)")
	}
	if l.Cooldown <= 0 {
		return nil, fmt.Errorf("ccpool: the rate-limit cooldown must be positive, got %s", l.Cooldown)
	}
	if l.MaxConcurrentJobs < 0 {
		return nil, fmt.Errorf("ccpool: the per-account job cap must be 0 (no cap) or at least 1, got %d", l.MaxConcurrentJobs)
	}
	return &Pool{src: src, store: store, now: time.Now, limits: l}, nil
}

func (p *Pool) accounts(ctx context.Context, owner int64) (map[string]string, []string, error) {
	acc, err := p.src.Accounts(ctx, owner)
	if err != nil {
		return nil, nil, fmt.Errorf("ccpool: read the Claude accounts of user %d: %w", owner, err)
	}
	names := make([]string, 0, len(acc))
	for n := range acc {
		names = append(names, n)
	}
	sort.Strings(names)
	return acc, names, nil
}

// Names returns owner's account names, sorted. Never the tokens.
func (p *Pool) Names(ctx context.Context, owner int64) ([]string, error) {
	_, names, err := p.accounts(ctx, owner)
	return names, err
}

// Token returns the setup-token of owner's account. It is how a pod spec gets
// its token; [ErrUnknownAccount] when owner has no such account — including
// when ANOTHER owner does.
func (p *Pool) Token(ctx context.Context, owner int64, account string) (string, error) {
	acc, _, err := p.accounts(ctx, owner)
	if err != nil {
		return "", err
	}
	tok, ok := acc[account]
	if !ok || tok == "" {
		return "", fmt.Errorf("%w %q for user %d", ErrUnknownAccount, account, owner)
	}
	return tok, nil
}

// Select picks the account for owner's new claude-code agent, from OWNER'S
// accounts only. A non-empty pin is returned as-is when it names one of them,
// and refused with [ErrUnknownAccount] otherwise.
func (p *Pool) Select(ctx context.Context, owner int64, pin string) (string, error) {
	tokens, names, err := p.accounts(ctx, owner)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("%w: you have no Claude account", ErrNoUsableAccount)
	}
	if pin != "" {
		if _, ok := tokens[pin]; !ok {
			return "", fmt.Errorf("%w %q (yours: %s)", ErrUnknownAccount, pin, strings.Join(names, ", "))
		}
		return pin, nil
	}
	marks, err := p.marks(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("ccpool: read account marks: %w", err)
	}
	live, err := p.store.LiveCounts(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("ccpool: count live agents per account: %w", err)
	}
	return Choose(p.now(), p.limits.Cooldown, names, tokens, marks, live)
}

// AuthFailure reports why owner's account cannot authenticate: a non-empty
// sentence when its CURRENT token is marked auth-failed (the mark's fingerprint
// matches it, exactly as [Choose] skips it), "" when it is usable. An unknown
// account is [ErrUnknownAccount].
//
// ⚠ Select's PIN PATH DELIBERATELY DOES NOT CALL THIS: a pin is the operator's
// override (TestSelectHonoursAPinOverEverySelectionRule). A caller that must not
// spend an irreversible step on a dead account — the chief replacement, which
// deletes the old chief first — asks here itself.
func (p *Pool) AuthFailure(ctx context.Context, owner int64, account string) (string, error) {
	tokens, _, err := p.accounts(ctx, owner)
	if err != nil {
		return "", err
	}
	tok, ok := tokens[account]
	if !ok || tok == "" {
		return "", fmt.Errorf("%w %q for user %d", ErrUnknownAccount, account, owner)
	}
	marks, err := p.marks(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("ccpool: read account marks: %w", err)
	}
	m := marks[account]
	if !authFailedNow(m, tok) {
		return "", nil
	}
	return fmt.Sprintf("Claude account %q failed authentication at %s (%s); replace its token first",
		account, m.AuthFailedAt.UTC().Format(time.RFC3339), m.AuthFailedDetail), nil
}

// AccountState is what selection knows about one of an owner's accounts.
type AccountState struct {
	// AuthFailed: the account's CURRENT token is marked auth_failed (the
	// mark's fingerprint is that token's), exactly the case [Choose] skips.
	AuthFailed bool
	// RateLimitedAt is the account's latest rate_limited mark; zero for never.
	RateLimitedAt time.Time
	// CoolingUntil is when the account's rate-limit cooldown ends ([CooldownUntil]),
	// set ONLY while it is still cooling down at the pool's clock when States
	// read it; zero once it has ended. A non-zero value means "not selectable".
	CoolingUntil time.Time
	// ResetStated: CoolingUntil is the reset time the account's limit message
	// stated ([statedReset]), not the configured cooldown after its mark — a
	// time the account's own limit named, rather than the pool's guess. False
	// whenever CoolingUntil is zero.
	ResetStated bool
	// MaxConcurrentJobs is the per-account job cap ([Limits.MaxConcurrentJobs]),
	// the same for every account. ZERO IS NO CAP, which is the default.
	MaxConcurrentJobs int
}

// States is AccountState for every one of owner's configured accounts, by
// name. An account that is not configured for owner is absent. It is what the
// job API's worker-pool selection reads (internal/api createPoolJob): the same
// marks, under the same "current token" and cooldown rules, as [Choose], plus
// the per-account job cap.
func (p *Pool) States(ctx context.Context, owner int64) (map[string]AccountState, error) {
	tokens, names, err := p.accounts(ctx, owner)
	if err != nil {
		return nil, err
	}
	marks, err := p.marks(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("ccpool: read account marks: %w", err)
	}
	now := p.now()
	out := make(map[string]AccountState, len(names))
	for _, n := range names {
		m := marks[n]
		until, stated := cooldownOf(m, p.limits.Cooldown)
		if !now.Before(until) {
			until, stated = time.Time{}, false
		}
		out[n] = AccountState{
			AuthFailed:        authFailedNow(m, tokens[n]),
			RateLimitedAt:     m.RateLimitedAt,
			CoolingUntil:      until,
			ResetStated:       stated,
			MaxConcurrentJobs: p.limits.MaxConcurrentJobs,
		}
	}
	return out, nil
}

// authFailedNow reports whether m marks token — the account's current one —
// as having failed authentication.
func authFailedNow(m Mark, token string) bool {
	return !m.AuthFailedAt.IsZero() && m.AuthFailedToken != "" && m.AuthFailedToken == Fingerprint(token)
}

// maxResetAhead bounds how far after its mark a stated reset time is believed.
// The CLI's limits reset within a session window (hours) or a week; a reset
// read as further away than a week and a day is a misreading (a date resolved
// into the wrong year), and the configured cooldown is used instead.
const maxResetAhead = 8 * 24 * time.Hour

// maxClockResetAhead bounds a reset stated as a CLOCK TIME alone ("resets
// 9:45pm (UTC)"), which is how the CLI states a session limit. A session window
// is about 5h, so its reset is never more than that ahead of the mark; 12h
// leaves room for clock skew between the pod and muster and for a mark written
// late, while still refusing the one misreading a clock time invites — a mark
// AT or just AFTER the stated time (an agent re-hitting the limit right at
// reset, or skew) resolves to the same time TOMORROW, ~24h ahead, which would
// bench the account (and possibly the whole pool, as 429s) for a day. Such a
// reset is not believed, and the configured cooldown is used instead.
//
// ⚠ THE ROLL TO TOMORROW STAYS, BOUNDED: a clock time earlier than the mark's
// can still be a real reset just after midnight ("resets 1am" marked at 23:00
// is 2h ahead), which only the roll finds.
const maxClockResetAhead = 12 * time.Hour

// resetRe matches the reset the CLI states in a rate-limit message:
// "· resets 9:45pm (UTC)", "· resets 9pm (Europe/Berlin)", "· resets Oct 9,
// 9am (UTC)". Groups: month, day, hour, minute, am/pm, zone.
var resetRe = regexp.MustCompile(`resets\s+(?:([A-Z][a-z]{2})\s+(\d{1,2}),\s+)?(\d{1,2})(?::(\d{2}))?\s*([aApP][mM])\s*\(([^)\s]+)\)`)

// ResetPhrase is the reset detail states as the CLI wrote it ("resets Nov 23,
// 1am (UTC)"), "" when it states none [statedReset] could read the shape of:
// what an officer chat tells the member it waits for (internal/officers
// ChatRateLimitedReply), with the same one pattern the pool reads it by.
func ResetPhrase(detail string) string {
	return strings.TrimSpace(resetRe.FindString(detail))
}

// statedReset is the reset time detail states, resolved against at (when the
// account was marked): the next such clock time (or date and time) after at,
// in the stated zone. ok is false when detail states none this can read — no
// match, an hour or minute out of range, a zone the system does not know, a
// date that does not exist, or a result more than [maxResetAhead] after at
// (a clock time alone: more than [maxClockResetAhead]).
//
// ⚠ A RESET WITH NO ZONE IS NOT READ. "resets 9pm" is 9pm somewhere; guessing
// UTC would end the window up to a day early or late, so it falls back to the
// configured cooldown like any other unreadable message.
func statedReset(detail string, at time.Time) (time.Time, bool) {
	g := resetRe.FindStringSubmatch(detail)
	if g == nil {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(g[3])
	if err != nil || hour < 1 || hour > 12 {
		return time.Time{}, false
	}
	minute := 0
	if g[4] != "" {
		if minute, err = strconv.Atoi(g[4]); err != nil || minute > 59 {
			return time.Time{}, false
		}
	}
	hour %= 12
	if strings.EqualFold(g[5], "pm") {
		hour += 12
	}
	loc, err := time.LoadLocation(g[6])
	if err != nil {
		return time.Time{}, false
	}
	local := at.In(loc)
	var reset time.Time
	if g[1] != "" {
		mon, err := time.Parse("Jan", g[1])
		if err != nil {
			return time.Time{}, false
		}
		day, err := strconv.Atoi(g[2])
		if err != nil {
			return time.Time{}, false
		}
		year := local.Year()
		for i := 0; i < 2 && !reset.After(at); i++ {
			reset = time.Date(year+i, mon.Month(), day, hour, minute, 0, 0, loc)
			if reset.Month() != mon.Month() || reset.Day() != day { // Feb 30 normalises into March
				return time.Time{}, false
			}
		}
	} else {
		reset = time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		if !reset.After(at) {
			reset = reset.AddDate(0, 0, 1)
		}
		if reset.Sub(at) > maxClockResetAhead {
			return time.Time{}, false
		}
	}
	if !reset.After(at) || reset.Sub(at) > maxResetAhead {
		return time.Time{}, false
	}
	return reset, true
}

// CooldownUntil is when m's rate-limit cooldown ends: the reset time its
// message states ([statedReset]), else its mark's time plus cooldown. Zero for
// an account never rate-limited. The account is selectable again AT that
// instant: cooling down is now.Before(CooldownUntil(...)).
//
// It is computed from the stored mark at every read, not stored itself, so a
// changed default cooldown applies to marks already written.
func CooldownUntil(m Mark, cooldown time.Duration) time.Time {
	until, _ := cooldownOf(m, cooldown)
	return until
}

// cooldownOf is [CooldownUntil], and whether that time is the reset m's
// message stated (true) rather than the mark's time plus cooldown (false).
func cooldownOf(m Mark, cooldown time.Duration) (time.Time, bool) {
	if m.RateLimitedAt.IsZero() {
		return time.Time{}, false
	}
	if reset, ok := statedReset(m.RateLimitedDetail, m.RateLimitedAt); ok {
		return reset, true
	}
	return m.RateLimitedAt.Add(cooldown), false
}

// EarliestReset is the earliest time one of an owner's accounts stops being at
// its usage limit, read from the pool's own record of them (states, as
// [Pool.States] answered it, at now): what an officer chat tells the member it
// waits for (internal/api, internal/officers ChatRateLimitedReplyFor). ok is
// false when the record names no such time, and the caller keeps what it had.
//
// It is the earliest [AccountState.CoolingUntil] that is still after now, and
// ok, ONLY when:
//
//   - every account that is not auth_failed is cooling at now. An account that
//     is neither is not at its limit by the pool's record — usable now, or
//     unmarked because the runtime never reported its limit — so the pool is not
//     exhausted and holds no reset to name. A cooldown already over at now (a
//     reset in the past) is such an account. An auth_failed account is skipped:
//     it answers when its token is replaced, never at a reset;
//   - and that earliest cooldown is a stated reset ([AccountState.ResetStated]).
//     The configured cooldown after an unreadable message is when the pool
//     tries the account again, not a reset its limit named, and is not quoted
//     as one.
//
// 🔴 THIS IS THE TIME THE SELECTION AND THE FAILOVER READ. [ChooseFailover]
// moves an agent onto an account exactly when its cooldown is over (or the
// usage poller cleared its mark on a live reading, which makes it not cooling
// here too), so the earliest cooldown is the earliest the officer's turn can
// land on an account again.
func EarliestReset(states map[string]AccountState, now time.Time) (time.Time, bool) {
	var earliest time.Time
	stated := false
	for _, st := range states {
		if st.AuthFailed {
			continue
		}
		if !st.CoolingUntil.After(now) {
			return time.Time{}, false
		}
		if earliest.IsZero() || st.CoolingUntil.Before(earliest) {
			earliest, stated = st.CoolingUntil, st.ResetStated
		} else if st.CoolingUntil.Equal(earliest) && st.ResetStated {
			stated = true
		}
	}
	if earliest.IsZero() || !stated {
		return time.Time{}, false
	}
	return earliest, true
}

// ResetPhraseAt is t as the CLI states a reset, in UTC and always with its
// date: "resets Nov 23, 1am (UTC)", "resets Nov 23, 9:45pm (UTC)" — the shape
// [ResetPhrase] reads. A time between minutes is rounded UP to the next one,
// so the member is never told an account is back before it is.
func ResetPhraseAt(t time.Time) string {
	t = t.UTC()
	if r := t.Truncate(time.Minute); !r.Equal(t) {
		t = r.Add(time.Minute)
	}
	layout := "Jan 2, 3:04pm"
	if t.Minute() == 0 {
		layout = "Jan 2, 3pm"
	}
	return "resets " + t.Format(layout) + " (UTC)"
}

// coolingUntil is CooldownUntil when the cooldown is still running at now, and
// zero when it has ended (or never began).
func coolingUntil(m Mark, cooldown time.Duration, now time.Time) time.Time {
	if until := CooldownUntil(m, cooldown); now.Before(until) {
		return until
	}
	return time.Time{}
}

// Choose is the selection algorithm (see the package doc), pure. now and
// cooldown decide which rate-limited accounts are still cooling down; such an
// account ranks last but is never refused.
func Choose(now time.Time, cooldown time.Duration, names []string, tokens map[string]string, marks map[string]Mark, live map[string]int) (string, error) {
	type cand struct {
		name    string
		cooling bool
		rl      time.Time
		live    int
	}
	var cands []cand
	var skipped []string
	for _, n := range names {
		m := marks[n]
		if authFailedNow(m, tokens[n]) {
			skipped = append(skipped, fmt.Sprintf("%s (auth failed %s: %s)", n, m.AuthFailedAt.UTC().Format(time.RFC3339), m.AuthFailedDetail))
			continue
		}
		cooling := !coolingUntil(m, cooldown, now).IsZero()
		cands = append(cands, cand{name: n, cooling: cooling, rl: m.RateLimitedAt, live: live[n]})
	}
	if len(cands) == 0 {
		if len(skipped) == 0 {
			return "", fmt.Errorf("%w: the pool is empty", ErrNoUsableAccount)
		}
		return "", fmt.Errorf("%w: every account's current token failed authentication — %s. Replace the "+
			"token (claude setup-token) to clear the mark, or pin an account explicitly",
			ErrNoUsableAccount, strings.Join(skipped, "; "))
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.cooling != b.cooling {
			return !a.cooling
		}
		if !a.rl.Equal(b.rl) {
			if a.rl.IsZero() || b.rl.IsZero() {
				return a.rl.IsZero()
			}
			return a.rl.Before(b.rl)
		}
		if a.live != b.live {
			return a.live < b.live
		}
		return a.name < b.name
	})
	return cands[0].name, nil
}

// ChooseFailover is the account a claude-code agent on account from is REBOUND
// to after from hit its limit or failed authentication (internal/ccfailover):
// pure, like [Choose], over the same marks, the same "current token" rule and
// the same cooldown.
//
// 🔴 UNLIKE [Choose] IT REFUSES RATHER THAN RANKING LAST. Choose still picks a
// cooling account when it is all there is, because a new agent outlives a
// rate-limit window; a failover exists only to move OFF a limited account, so
// moving onto another one that is still cooling (or whose token failed) would
// restart the agent's pod to buy nothing. ok is false then, and the agent stays
// where it is.
//
// Of the usable accounts other than from — not auth_failed for its current
// token, not cooling down at now — it prefers the order the pool and the job
// API use: never rate-limited first, then the one rate-limited longest ago,
// then the name (deterministic).
func ChooseFailover(now time.Time, cooldown time.Duration, names []string, tokens map[string]string, marks map[string]Mark, from string) (string, bool) {
	best, found := "", false
	var bestRL time.Time
	for _, n := range names {
		if n == from {
			continue
		}
		m := marks[n]
		if authFailedNow(m, tokens[n]) || !coolingUntil(m, cooldown, now).IsZero() {
			continue
		}
		rl := m.RateLimitedAt
		better := !found
		if found {
			switch {
			case rl.Equal(bestRL):
				better = n < best
			case rl.IsZero() || bestRL.IsZero():
				better = rl.IsZero()
			default:
				better = rl.Before(bestRL)
			}
		}
		if better {
			best, bestRL, found = n, rl, true
		}
	}
	return best, found
}

// FailoverTarget is [ChooseFailover] over owner's accounts and marks, read now.
// ok is false when no other account of owner's is usable.
func (p *Pool) FailoverTarget(ctx context.Context, owner int64, from string) (string, bool, error) {
	tokens, names, err := p.accounts(ctx, owner)
	if err != nil {
		return "", false, err
	}
	marks, err := p.marks(ctx, owner)
	if err != nil {
		return "", false, fmt.Errorf("ccpool: read account marks: %w", err)
	}
	to, ok := ChooseFailover(p.now(), p.limits.Cooldown, names, tokens, marks, from)
	return to, ok, nil
}

// Cooldown is one account's rate-limit cooldown as [Pool.Cooldown] read it.
type Cooldown struct {
	// Until is when the cooldown ends ([CooldownUntil]); zero when the account
	// is not cooling now.
	Until time.Time
	// MarkedAt is the timestamp of the rate_limited mark Until was computed
	// from (zero with Until). It is what [Pool.ClearRateLimit] compares
	// against, so a clear decided on this reading can never erase a newer mark.
	MarkedAt time.Time
	// Detail is that mark's stored detail (the CLI's limit message; "" with
	// Until). It names WHICH limit was hit, which the live reading must answer
	// for before the mark is cleared (internal/ccusage LiveUsable).
	Detail string
}

// Cooldown is owner's account's rate-limit cooldown.
func (p *Pool) Cooldown(ctx context.Context, owner int64, account string) (Cooldown, error) {
	marks, err := p.marks(ctx, owner)
	if err != nil {
		return Cooldown{}, fmt.Errorf("ccpool: read account marks: %w", err)
	}
	m := marks[account]
	until := coolingUntil(m, p.limits.Cooldown, p.now())
	if until.IsZero() {
		return Cooldown{}, nil
	}
	return Cooldown{Until: until, MarkedAt: m.RateLimitedAt, Detail: m.RateLimitedDetail}, nil
}

// CoolingUntil is [Pool.Cooldown]'s Until: when owner's account stops cooling
// down, or zero when it is not cooling now.
func (p *Pool) CoolingUntil(ctx context.Context, owner int64, account string) (time.Time, error) {
	c, err := p.Cooldown(ctx, owner, account)
	return c.Until, err
}

// ClearRateLimit clears owner's account's rate_limited mark because a LIVE
// reading of the account shows it usable (internal/ccusage), and reports
// whether it did. markedAt is [Cooldown.MarkedAt] as read BEFORE that reading
// was taken: the mark is cleared only while it is still that one, so a turn
// that hits the limit again while the reading is in flight keeps its mark.
// A zero markedAt clears nothing.
func (p *Pool) ClearRateLimit(ctx context.Context, owner int64, account string, markedAt time.Time) (bool, error) {
	if markedAt.IsZero() {
		return false, nil
	}
	mctx, mowner := p.markScope(ctx, owner)
	ok, err := p.store.ClearRateLimited(mctx, mowner, account, markedAt)
	if err != nil {
		return false, fmt.Errorf("ccpool: clear the rate-limit mark of account %q: %w", account, err)
	}
	return ok, nil
}

// maxDetail bounds the stored detail. ccd's message carries the CLI's "resets …"
// text, which is what is worth keeping; a runtime body is not.
const maxDetail = 300

// MarkFailure records a typed ccd failure against owner's account. Types other
// than [FailureRateLimited] and [FailureAuthFailed] are ignored (nil): only
// those two say something about the ACCOUNT rather than about the turn.
//
// 🔴 failure IS [AccountFailure]'s ANSWER, NOT THE RUNTIME'S RAW TYPE: a
// transient 429 is typed rate_limited too, and must not cool the account down.
func (p *Pool) MarkFailure(ctx context.Context, owner int64, account, failure, detail string) error {
	if failure != FailureRateLimited && failure != FailureAuthFailed {
		return nil
	}
	tok, err := p.Token(ctx, owner, account)
	if err != nil {
		return err
	}
	if len(detail) > maxDetail {
		// Cut on a rune boundary: a byte cut through "·" or "…" is invalid UTF-8,
		// which Postgres refuses for TEXT (22021) — losing the mark.
		cut := maxDetail
		for cut > 0 && !utf8.RuneStart(detail[cut]) {
			cut--
		}
		detail = detail[:cut] + "…"
	}
	mctx, mowner := p.markScope(ctx, owner)
	if failure == FailureRateLimited {
		return p.store.MarkRateLimited(mctx, mowner, account, detail, p.now())
	}
	return p.store.MarkAuthFailed(mctx, mowner, account, Fingerprint(tok), detail, p.now())
}
