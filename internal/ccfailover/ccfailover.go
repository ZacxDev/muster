// Package ccfailover is per-agent Claude ACCOUNT FAILOVER: when a turn on a
// claude-code agent fails `rate_limited` or `auth_failed`, the agent is REBOUND
// to another of its owner's Claude accounts that is usable, instead of staying
// on the exhausted one until its limit resets.
//
// # Why
//
// A claude-code agent runs on one account: its pod's CLAUDE_CODE_OAUTH_TOKEN.
// The account cooldown (internal/ccpool) only steers which worker a NEW pool
// job goes to, so a deployment with ONE worker (or one officer instance) on an
// exhausted account stalled even with a second account free.
//
// # What happens
//
// internal/agentgateway calls [Service.AccountFailed] after it marks the
// account (ccpool.Pool.MarkFailure), for every turn shape — a chat, a kickoff,
// an officer re-prompt, a job. AccountFailed:
//
//  1. ignores every failure but rate_limited and auth_failed;
//  2. does nothing when failover is off (MUSTER_AGENT_CC_FAILOVER=off), or the
//     agent is PINNED (its account was named at creation — a dispatch pin, the
//     chief's, an officer's `cc_account`) and has not opted in (cc_failover);
//  3. picks the target with ccpool.ChooseFailover — another account of the
//     owner's that is neither auth_failed for its current token nor cooling
//     down; never rate-limited first, then rate-limited longest ago, then the
//     name. None usable (every account cooling): nothing happens, as before;
//  4. moves the agent (agentprovision.Adapter.Rebind): the row's cc_account
//     changes (conditionally on the old one) and its spec is re-applied, so its
//     pod restarts on the new token, keeping its volume, namespace and identity;
//  5. records an event (cc_account_failovers) and logs one line — account
//     NAMES, the reason and the reset time, never a token.
//
// # Idempotent on (agent, from account)
//
// Two turns on one agent failing at once produce ONE rebind: in this process a
// per-agent lock and a re-read of the row under it (an agent no longer on the
// failed account was already moved); across processes the row's conditional
// move (WHERE cc_account = from). A later caller for the same (agent, from)
// gets Rebound false and Moved true, with the move's details from a short memo
// — which is how the job runner, which also asks after the gateway already
// did, learns where its agent went and which pod it replaced.
//
// # No fail-back
//
// Once moved, an agent stays on its new account when the old one resets: a
// move back would restart its pod again for nothing. It moves again only when
// the new account in turn fails.
//
// # Why a pod restart, not a token re-read
//
// The token is an environment variable the CLI (and each job lane's CLI) is
// started with. A cheaper path — ccd re-reading a mounted Secret file and
// restarting its CLIs — would need new ccd code, a Secret volume the kubelet
// refreshes on its own schedule (up to a minute late, and never for a subPath
// mount), and would still restart every CLI the pod runs. The pod restart is
// the path muster already uses for a rotated token (Stop → Start), costs a
// cold start (the running CLI session restarts; the volume, transcripts and
// checkout are kept), and leaves exactly one place the token is written
// (agentspec, the per-agent Secret).
package ccfailover

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/provision"
)

// Event is one recorded rebind.
type Event struct {
	ID        int64     `json:"id"`
	OwnerID   int64     `json:"-"`
	AgentID   int64     `json:"agent_id"`
	AgentName string    `json:"agent"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Reason    string    `json:"reason"`
	ResetsAt  time.Time `json:"resets_at,omitzero"`
	At        time.Time `json:"at"`
}

// String is the event's one log line: names, reason and reset — never a token
// (an Event holds none).
func (e Event) String() string {
	resets := "unknown"
	if !e.ResetsAt.IsZero() {
		resets = e.ResetsAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("account_failover from %s to %s, reason %s, resets %s", e.From, e.To, e.Reason, resets)
}

// Result is what [Service.AccountFailed] found or did.
type Result struct {
	// AgentName is the agent's name ("" when it could not be read).
	AgentName string
	// From is the account the failure was reported for.
	From string
	// To is the account the agent is on after the call, when it is not From.
	To string
	// Moved: the agent is on To, not From — by this call or an earlier one.
	Moved bool
	// Rebound: THIS call moved it.
	Rebound bool
	// PrevInstance is the instance that ran before the move ("" unknown).
	PrevInstance string
	// Event is the recorded rebind, when Moved by a call this service made.
	Event Event
	// Why is why the agent was not moved ("" when it was).
	Why string
}

// Pool is the part of ccpool.Pool a failover reads.
type Pool interface {
	FailoverTarget(ctx context.Context, owner int64, from string) (string, bool, error)
	CoolingUntil(ctx context.Context, owner int64, account string) (time.Time, error)
}

// Agents reads an agent's row.
type Agents interface {
	Get(ctx context.Context, id int64) (agents.Agent, error)
}

// Rebinder is the lifecycle half (agentprovision.Adapter.Rebind).
type Rebinder interface {
	Rebind(ctx context.Context, agentID int64, from, to string, prepare func(context.Context, agents.Agent) error) (agentprovision.RebindResult, error)
}

// Store records rebinds.
type Store interface {
	Record(ctx context.Context, e Event) (Event, error)
	ForAgent(ctx context.Context, agentID int64, limit int) ([]Event, error)
}

// Config builds a [Service].
type Config struct {
	// Enabled is MUSTER_AGENT_CC_FAILOVER (default on). Off: nothing is moved.
	Enabled  bool
	Pool     Pool
	Agents   Agents
	Rebinder Rebinder
	// Store is optional: without one a rebind is logged but not recorded.
	Store Store
	// Instances lists the backend's instances, for AwaitRebound. Optional:
	// without it AwaitRebound returns at once.
	Instances func(ctx context.Context) ([]provision.Instance, error)
	Logger    *log.Logger
	// Now is a test seam (time.Now when nil).
	Now func() time.Time
	// ReadyPoll and ReadyWait bound AwaitRebound (defaults 3s and 5m).
	ReadyPoll, ReadyWait time.Duration
}

// Policy reports, for an agent, whether its account is pinned and whether
// it opted in to failover anyway, beyond what its row says (the officer
// runtime answers for an officer's configured `cc_account` / `cc_failover`).
type Policy func(agents.Agent) (pinned, optIn bool)

// Prepare runs after a move and before the re-apply (agentprovision.Adapter.Rebind).
type Prepare func(context.Context, agents.Agent) error

// Service is the failover.
type Service struct {
	cfg Config
	log *log.Logger

	hookMu  sync.RWMutex
	policy  Policy
	prepare Prepare

	mu    sync.Mutex
	lanes map[int64]*sync.Mutex
	memo  map[memoKey]memoEntry
}

type memoKey struct {
	agent int64
	from  string
}

type memoEntry struct {
	res Result
	at  time.Time
}

// memoTTL is how long a (agent, from) rebind's details (the replaced instance,
// the event) are kept for a later caller that finds the agent already moved.
// It only has to outlive the turns that failed together.
const memoTTL = 15 * time.Minute

// New builds a service. Pool, Agents and Rebinder are required.
func New(cfg Config) (*Service, error) {
	if cfg.Pool == nil || cfg.Agents == nil || cfg.Rebinder == nil {
		return nil, fmt.Errorf("ccfailover: a pool, an agents store and a rebinder are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReadyPoll <= 0 {
		cfg.ReadyPoll = 3 * time.Second
	}
	if cfg.ReadyWait <= 0 {
		cfg.ReadyWait = 5 * time.Minute
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	return &Service{cfg: cfg, log: lg, lanes: map[int64]*sync.Mutex{}, memo: map[memoKey]memoEntry{}}, nil
}

// Enabled reports MUSTER_AGENT_CC_FAILOVER.
func (s *Service) Enabled() bool { return s.cfg.Enabled }

// UseHooks installs the pin policy and the pre-apply hook (internal/api's
// officer runtime). Either may be nil.
func (s *Service) UseHooks(policy Policy, prepare Prepare) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.policy, s.prepare = policy, prepare
}

func (s *Service) hooks() (Policy, Prepare) {
	s.hookMu.RLock()
	defer s.hookMu.RUnlock()
	return s.policy, s.prepare
}

func (s *Service) lane(agentID int64) func() {
	s.mu.Lock()
	m, ok := s.lanes[agentID]
	if !ok {
		m = &sync.Mutex{}
		s.lanes[agentID] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Allowed reports whether ag may be failed over, and why not.
func (s *Service) Allowed(ag agents.Agent) (bool, string) {
	if !s.cfg.Enabled {
		return false, "account failover is off (MUSTER_AGENT_CC_FAILOVER=off)"
	}
	pinned, optIn := ag.CCAccountPinned, ag.CCFailover
	if policy, _ := s.hooks(); policy != nil {
		p, o := policy(ag)
		pinned, optIn = pinned || p, optIn || o
	}
	if pinned && !optIn {
		return false, fmt.Sprintf("the agent is pinned to account %q and has not opted in to failover", ag.CCAccount)
	}
	return true, ""
}

// AccountFailed is the failover's entry point: a turn on agentID, running on
// account, failed with failure (ccd's typed error) and detail (its message).
// It never returns an error: what it could not do is in Result.Why and the log.
func (s *Service) AccountFailed(ctx context.Context, agentID int64, account, failure, detail string) Result {
	res := Result{From: account}
	if failure != ccpool.FailureRateLimited && failure != ccpool.FailureAuthFailed {
		res.Why = "not an account failure"
		return res
	}
	if account == "" || agentID <= 0 {
		res.Why = "no agent or account"
		return res
	}
	defer s.lane(agentID)()
	key := memoKey{agentID, account}
	now := s.cfg.Now()

	ag, err := s.cfg.Agents.Get(ctx, agentID)
	if err != nil {
		res.Why = "the agent could not be read: " + err.Error()
		return res
	}
	res.AgentName = ag.Name
	if agents.ResolveKind(ag.Kind) != agents.KindClaudeCode || ag.CCAccount == "" {
		res.Why = "not a claude-code agent"
		return res
	}
	// 🔴 THE IDEMPOTENCY KEY IS (agent, from): an agent no longer on the account
	// that failed was already moved off it — by a concurrent turn's call that
	// held this lane first, or by another process — so this call moves nothing
	// and answers where it went (with that move's details when this process
	// made it).
	if ag.CCAccount != account {
		res.To, res.Moved = ag.CCAccount, true
		s.mu.Lock()
		m, ok := s.memo[key]
		s.mu.Unlock()
		if ok && now.Sub(m.at) < memoTTL && m.res.To == ag.CCAccount {
			res.PrevInstance, res.Event = m.res.PrevInstance, m.res.Event
		}
		return res
	}
	if ok, why := s.Allowed(ag); !ok {
		res.Why = why
		return res
	}
	to, ok, err := s.cfg.Pool.FailoverTarget(ctx, ag.OwnerID, account)
	if err != nil {
		res.Why = "the accounts could not be read: " + err.Error()
		s.log.Printf("ccfailover: agent %s: %s", ag.Name, res.Why)
		return res
	}
	if !ok {
		res.Why = "no other Claude account is usable (every other one is cooling down or failed authentication)"
		s.log.Printf("ccfailover: agent %s stays on account %s after %s: %s", ag.Name, account, failure, res.Why)
		return res
	}
	_, prepare := s.hooks()
	rr, err := s.cfg.Rebinder.Rebind(ctx, agentID, account, to, prepare)
	if !rr.Rebound {
		if rr.Current != "" && rr.Current != account {
			res.To, res.Moved = rr.Current, true
			return res
		}
		res.Why = rr.Skipped
		if err != nil {
			res.Why = err.Error()
			s.log.Printf("ccfailover: agent %s: rebind from %s to %s: %v", ag.Name, account, to, err)
		}
		return res
	}
	res.To, res.Moved, res.Rebound, res.PrevInstance = to, true, true, rr.PrevInstance
	ev := Event{OwnerID: ag.OwnerID, AgentID: ag.ID, AgentName: ag.Name, From: account, To: to, Reason: failure, At: now}
	if failure == ccpool.FailureRateLimited {
		if until, cerr := s.cfg.Pool.CoolingUntil(ctx, ag.OwnerID, account); cerr == nil {
			ev.ResetsAt = until
		}
	}
	if s.cfg.Store != nil {
		if rec, rerr := s.cfg.Store.Record(context.WithoutCancel(ctx), ev); rerr != nil {
			s.log.Printf("ccfailover: agent %s: record the rebind: %v", ag.Name, rerr)
		} else {
			ev = rec
		}
	}
	res.Event = ev
	s.log.Printf("ccfailover: agent %s (id %d): %s", ag.Name, ag.ID, ev)
	if err != nil {
		s.log.Printf("ccfailover: agent %s: moved to %s, but re-applying its spec failed: %v", ag.Name, to, err)
	}
	s.mu.Lock()
	s.memo[key] = memoEntry{res: res, at: now}
	s.mu.Unlock()
	return res
}

// NoteAccountFailure is AccountFailed for a caller that needs no answer
// (internal/agentgateway, after it marks the account).
func (s *Service) NoteAccountFailure(ctx context.Context, agentID int64, account, failure, detail string) {
	s.AccountFailed(ctx, agentID, account, failure, detail)
}

// AwaitRebound waits until a moved agent's replacement instance is ready: an
// instance of r.AgentName that is Ready and is not r.PrevInstance. It returns
// nil at once when the service cannot list instances, and an error when ctx
// ends or ReadyWait passes first.
func (s *Service) AwaitRebound(ctx context.Context, r Result) error {
	if !r.Moved || r.AgentName == "" {
		return fmt.Errorf("ccfailover: the agent was not moved")
	}
	if s.cfg.Instances == nil {
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, s.cfg.ReadyWait)
	defer cancel()
	for {
		if list, err := s.cfg.Instances(wctx); err == nil {
			for _, in := range list {
				if in.Ref.Name == r.AgentName && in.Ready && (r.PrevInstance == "" || in.InstanceID != r.PrevInstance) {
					return nil
				}
			}
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("ccfailover: agent %s's instance on account %s was not ready in time: %w", r.AgentName, r.To, wctx.Err())
		case <-time.After(s.cfg.ReadyPoll):
		}
	}
}

// Events is agentID's recorded rebinds, newest first (none without a store).
func (s *Service) Events(ctx context.Context, agentID int64, limit int) ([]Event, error) {
	if s.cfg.Store == nil {
		return nil, nil
	}
	return s.cfg.Store.ForAgent(ctx, agentID, limit)
}
