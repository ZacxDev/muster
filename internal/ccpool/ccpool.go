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
//   - Token: what is the token for the account an EXISTING agent was given?
//   - MarkFailure: ccd reported `rate_limited` or `auth_failed` for an agent on
//     account X — remember it, with a timestamp, in Postgres.
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
//  2. Least-recently-rate-limited first: never rate-limited sorts before any
//     timestamp, then oldest timestamp first.
//  3. Then fewest LIVE agents (kind claude-code, status pending, provisioning or
//     running) on the account.
//  4. Then the account name, so the answer is deterministic.
//
// An explicit pin at dispatch bypasses all four — it is the operator's override,
// including for an account the pool would skip — but must name a configured
// account.
//
// 🔴 AN AGENT KEEPS ITS ACCOUNT FOR LIFE. Select runs once, before the agent's
// row exists; the row stores the NAME (agents.cc_account) and migration 0003
// refuses any later change. A restart, a re-provision, a model of the pool
// changing under it — none of them re-select.
//
// 🔴 THE TOKEN NEVER LEAVES THIS PACKAGE EXCEPT INTO ONE PLACE: the claude-code
// agent's own per-agent Secret, as CLAUDE_CODE_OAUTH_TOKEN, via
// internal/agentspec. No error, log line or stored mark carries it — the mark
// stores a truncated sha256 fingerprint.
package ccpool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The failure types ccd reports that mark an account. They are ccd's own
// spellings (cmd/ccd/failure.go: failRateLimited / failAuthFailed) — the wire's
// `error.type` — and nothing else marks.
const (
	FailureRateLimited = "rate_limited"
	FailureAuthFailed  = "auth_failed"
)

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

// Store persists marks and counts live agents per account.
type Store interface {
	Marks(ctx context.Context) (map[string]Mark, error)
	LiveCounts(ctx context.Context) (map[string]int, error)
	MarkRateLimited(ctx context.Context, account, detail string, at time.Time) error
	MarkAuthFailed(ctx context.Context, account, fingerprint, detail string, at time.Time) error
}

// Pool is the configured accounts plus their store.
type Pool struct {
	names  []string
	tokens map[string]string
	store  Store
	now    func() time.Time
}

// New builds a pool. It REFUSES an empty pool, an invalid name and an empty
// token, so a misconfiguration stops the process at boot instead of building a
// pod with no credential at the first dispatch.
func New(accounts map[string]string, store Store) (*Pool, error) {
	if len(accounts) == 0 {
		return nil, fmt.Errorf("%w: the pool is empty", ErrNoUsableAccount)
	}
	if store == nil {
		return nil, errors.New("ccpool: a Store is required (marks and live counts are what selection reads)")
	}
	p := &Pool{tokens: make(map[string]string, len(accounts)), store: store, now: time.Now}
	for name, tok := range accounts {
		if !ValidName(name) {
			return nil, fmt.Errorf("ccpool: account name %q is not 1-%d of [a-z0-9-] (not starting or ending with '-')", name, maxNameLen)
		}
		if strings.TrimSpace(tok) == "" {
			return nil, fmt.Errorf("ccpool: account %q has an empty token", name)
		}
		p.names = append(p.names, name)
		p.tokens[name] = tok
	}
	sort.Strings(p.names)
	return p, nil
}

// Names returns the configured account names, sorted. Never the tokens.
func (p *Pool) Names() []string {
	out := make([]string, len(p.names))
	copy(out, p.names)
	return out
}

// Token returns an account's setup-token.
func (p *Pool) Token(name string) (string, bool) {
	t, ok := p.tokens[name]
	return t, ok
}

// Tokens returns a copy of the name->token map, for the one consumer that must
// place a token (internal/agentspec's Config). Every other caller wants Names.
func (p *Pool) Tokens() map[string]string {
	out := make(map[string]string, len(p.tokens))
	for k, v := range p.tokens {
		out[k] = v
	}
	return out
}

// Select picks the account for a new claude-code agent. A non-empty pin is
// returned as-is when it names a configured account (the operator's override),
// and refused with [ErrUnknownAccount] otherwise.
func (p *Pool) Select(ctx context.Context, pin string) (string, error) {
	if pin != "" {
		if _, ok := p.tokens[pin]; !ok {
			return "", fmt.Errorf("%w %q (configured: %s)", ErrUnknownAccount, pin, strings.Join(p.names, ", "))
		}
		return pin, nil
	}
	marks, err := p.store.Marks(ctx)
	if err != nil {
		return "", fmt.Errorf("ccpool: read account marks: %w", err)
	}
	live, err := p.store.LiveCounts(ctx)
	if err != nil {
		return "", fmt.Errorf("ccpool: count live agents per account: %w", err)
	}
	return Choose(p.names, p.tokens, marks, live)
}

// Choose is the selection algorithm (see the package doc), pure.
func Choose(names []string, tokens map[string]string, marks map[string]Mark, live map[string]int) (string, error) {
	type cand struct {
		name string
		rl   time.Time
		live int
	}
	var cands []cand
	var skipped []string
	for _, n := range names {
		m := marks[n]
		if !m.AuthFailedAt.IsZero() && m.AuthFailedToken != "" && m.AuthFailedToken == Fingerprint(tokens[n]) {
			skipped = append(skipped, fmt.Sprintf("%s (auth failed %s: %s)", n, m.AuthFailedAt.UTC().Format(time.RFC3339), m.AuthFailedDetail))
			continue
		}
		cands = append(cands, cand{name: n, rl: m.RateLimitedAt, live: live[n]})
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

// maxDetail bounds the stored detail. ccd's message carries the CLI's "resets …"
// text, which is what is worth keeping; a runtime body is not.
const maxDetail = 300

// MarkFailure records a typed ccd failure against an account. Types other than
// [FailureRateLimited] and [FailureAuthFailed] are ignored (nil): only those two
// say something about the ACCOUNT rather than about the turn.
func (p *Pool) MarkFailure(ctx context.Context, account, failure, detail string) error {
	tok, ok := p.tokens[account]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownAccount, account)
	}
	if len(detail) > maxDetail {
		detail = detail[:maxDetail] + "…"
	}
	switch failure {
	case FailureRateLimited:
		return p.store.MarkRateLimited(ctx, account, detail, p.now())
	case FailureAuthFailed:
		return p.store.MarkAuthFailed(ctx, account, Fingerprint(tok), detail, p.now())
	}
	return nil
}
