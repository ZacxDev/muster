package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/askimage"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/officers/transcript"
	"github.com/ZacxDev/muster/internal/officers/usage"
	"github.com/ZacxDev/muster/internal/officership"
	"github.com/ZacxDev/muster/internal/podagent"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// THE OFFICER RUNTIME, BOUND TO muster (docs/officers.md)
//
// internal/officers holds the policy (which instances to run, when to stop
// them) and the HQ client. This file binds it to muster:
//
//   - officerFleet: the wake loop's view of an officer's instances, over the
//     agents store and the provisioner — create, start (re-arming the first
//     turn), stop;
//   - the officer MCP tools (officer_brief … officer_release), offered to an
//     officer's instance agents ONLY and proxied to HQ with that officer's
//     token, which never leaves this process;
//   - delete protection for a declared officer's instance agents.
//
// 🔴 THE BINDING IS THE AGENT'S NAME, AND THE NAME IS THE SERVER'S. An
// officer tool acts for officers.Registry.ForAgentName(a.Name), where a is the
// agent requireConnectorAgent resolved from the caller's own per-agent token.
// The tools take no officer argument; an argument naming another officer is
// refused, not ignored. An agent cannot rename itself (the slug is immutable,
// agents.Store.SetDisplayName), and no muster path mints a name starting
// `officer-` except this runtime (dispatchParams.Name has no user-facing
// door), so an agent cannot come to act for an officer it was not created as.
// ---------------------------------------------------------------------------

// officerRuntime is the officer runtime's state on the server; nil when no
// officer is declared and there is no database to declare one in.
type officerRuntime struct {
	// reg is the LIVE roster: env officers plus the runnable DB-declared ones,
	// swapped atomically by reloadDeclaredOfficers (officer_declared.go).
	reg *officers.Registry
	// env is MUSTER_OFFICERS' officers, fixed at boot.
	env []officers.Officer
	// store is the built-in store DB-declared officers are read from; nil:
	// none are (no database).
	store *builtin.Store
	// sessionInstalled: the provisioner takes the officer session settings
	// (OfficersConfig.InstallSession was given), which a pod officer needs.
	sessionInstalled bool
	// reloadMu serialises roster loads; declMu guards declared and loaded:
	// the last load's DB-declared officers by declKey, and whether a load has
	// run. (Where a "runtime": "auto" officer is placed is the database's,
	// DeclaredRow.AutoHost — never this process's memory: round 2.)
	reloadMu sync.Mutex
	declMu   sync.Mutex
	declared map[string]declaredState
	loaded   bool
	hq       *officers.Backends
	rec      *officers.Reconciler
	// claimOwner names this process on an instance's session claim while it
	// sends a re-prompt.
	claimOwner string
	// wg counts re-prompt sends in flight.
	wg sync.WaitGroup
	// ships are the shipping officers' runtimes, by officer id
	// (officer_ship.go).
	ships map[string]*shipRuntime
	// fwd forwards instances' transcripts to HQ (internal/officers/transcript);
	// nil when no declared officer enables `transcript`.
	fwd *transcript.Forwarder
	// usage pushes every instance's Claude usage to HQ
	// (internal/officers/usage, officer_usage.go); nil when the agent gateway
	// cannot read it.
	usage *usage.Collector
	// pools pushes every declared officer's pools to its HQ at boot and on
	// the usage collector's cadence (officers.PoolsReporter, "Pools to HQ").
	pools *officers.PoolsReporter
	// llm is the officer LLM proxy (officer_llm.go, internal/llmproxy); nil
	// when no declared officer has an `llm` block.
	llm *llmproxy.Proxy
	// judgement counts each tiered officer's judgement sessions per UTC day
	// (officers.JudgementLedger): officer_claim reserves in it, and the wake
	// loop reads it. now is the clock both use (OfficersConfig.Now).
	judgement *officers.JudgementLedger
	now       func() time.Time
	// imagePullSecret is OfficersConfig.ImagePullSecret.
	imagePullSecret string
	// claimLocks serialises officer_claim per instance name (*sync.Mutex) for
	// a capped judgement pool's one-claim rule (oneClaimRefusal) and a fresh
	// pool's one item per conversation (freshClaimRefusal).
	claimLocks sync.Map
	// kube is officer_kube_read's state (officer_kube.go); nil when no
	// declared officer has a `kube` block.
	kube *kubeRuntime
	// reads is THE per-item read budget, one counter per (officer, item)
	// shared by every tool officers.CountsRead names (officer_reads.go).
	reads *officers.ReadBudget
	// worked is, per instance name of a fresh_session_per_item pool, the item
	// its CURRENT conversation claimed — marked when the claim SUCCEEDS
	// (claimedInConversation), so however the hold then ends (complete,
	// release, ask, a lapsed lease, a complete HQ refuses) officer_claim
	// refuses that conversation any other item (freshClaimRefusal). Cleared
	// ONLY when ccd confirms a new conversation took a turn muster sent
	// (conversationConfirmed, from OfficerTurnContext). Guarded by workedMu.
	workedMu sync.Mutex
	worked   map[string]officers.ItemID
	// memLoops is the review-loop record when no store is configured
	// (prLoopsFor), made once.
	memLoopsOnce sync.Once
	memLoops     *officers.MemPRLoopStore
	// memPublicBranches is the in-memory record of draft officers' public
	// branches when no store is configured (publicBranchesFor), made once.
	memPublicBranchesOnce sync.Once
	memPublicBranches     *officers.MemPublicBranchStore
	// blocks is OfficersConfig.ConnectorBlocks: which items each officer
	// instance held while a pending grant refused it (connectorBlocks;
	// officer_connector_rerun.go); memBlocks the in-memory one when none is
	// configured, made once. blockWork counts the refusals' records and the
	// approvals' re-runs in flight; blockNotes is, per "agent grant", the
	// last refusal that read HQ (the debounce), guarded by blockNotesMu.
	blocks        officers.ConnectorBlockStore
	memBlocksOnce sync.Once
	memBlocks     *officers.MemConnectorBlockStore
	blockWork     sync.WaitGroup
	blockNotesMu  sync.Mutex
	blockNotes    map[string]time.Time
	// shadow is the shepherd's shadow ledger (officer_shadow.go): the writes
	// refused in shadow, per (officer, item), appended to that item's
	// officer_complete summary. Guarded by shadowMu.
	shadowMu sync.Mutex
	shadow   map[string]*shadowLedger
	// hostSpawns is OfficersConfig.HostSpawns; host is the host fleet's
	// in-process state (officer_host.go).
	hostSpawns officers.HostSpawnStore
	// unclaimedAfter is OfficersConfig.UnclaimedAfter (0: the default).
	unclaimedAfter time.Duration
	host           officerHostState
	// chats runs the officers' chat turns (officer_chat.go, internal/officers
	// chat.go); nil when the agents store cannot keep a chat's session.
	chats *officers.ChatRunner
	// stale is, per instance name of a fresh_session_per_item pool, true once
	// its ccd accepted a turn asking for a new conversation WITHOUT naming one
	// (an image that predates agents.HeaderCCNewConversation): muster sends it
	// no further turn (Reprompt) until it is started again (Start). Guarded by
	// workedMu.
	stale map[string]bool
}

// claimedInConversation marks instance's current conversation as having
// claimed item, for a fresh_session_per_item pool o; any other pool records
// nothing.
func (rt *officerRuntime) claimedInConversation(o officers.Officer, instance string, item officers.ItemID) {
	if !o.FreshSessionPerItem() || item == "" {
		return
	}
	rt.workedMu.Lock()
	defer rt.workedMu.Unlock()
	if rt.worked == nil {
		rt.worked = map[string]officers.ItemID{}
	}
	rt.worked[instance] = item
}

// workedItem is the item instance's current conversation has claimed ("" when
// none).
func (rt *officerRuntime) workedItem(instance string) officers.ItemID {
	rt.workedMu.Lock()
	defer rt.workedMu.Unlock()
	return rt.worked[instance]
}

// conversationConfirmed forgets the item instance's conversation claimed:
// its ccd has confirmed that a turn muster sent was typed into a NEW
// conversation (agents.TurnAcceptance.NewConversation).
func (rt *officerRuntime) conversationConfirmed(instance string) {
	rt.workedMu.Lock()
	defer rt.workedMu.Unlock()
	delete(rt.worked, instance)
	delete(rt.stale, instance)
}

// markStale records that instance's ccd accepted a turn asking for a new
// conversation without naming one.
func (rt *officerRuntime) markStale(instance string) {
	rt.workedMu.Lock()
	defer rt.workedMu.Unlock()
	if rt.stale == nil {
		rt.stale = map[string]bool{}
	}
	rt.stale[instance] = true
}

// isStale reports markStale's record for instance.
func (rt *officerRuntime) isStale(instance string) bool {
	rt.workedMu.Lock()
	defer rt.workedMu.Unlock()
	return rt.stale[instance]
}

// forgetStale drops markStale's record for instance: it is being started
// again, perhaps onto a newer image.
func (rt *officerRuntime) forgetStale(instance string) {
	rt.workedMu.Lock()
	defer rt.workedMu.Unlock()
	delete(rt.stale, instance)
}

// spent reports, for an instance of a fresh_session_per_item pool o, that its
// current conversation has claimed its one item (officers.InstanceState.Spent)
// — and its ccd is not known to be stale, which muster sends nothing.
func (rt *officerRuntime) spent(o officers.Officer, instance string) bool {
	if !o.FreshSessionPerItem() {
		return false
	}
	rt.workedMu.Lock()
	defer rt.workedMu.Unlock()
	return rt.worked[instance] != "" && !rt.stale[instance]
}

// clock is the runtime's now (time.Now when unset).
func (rt *officerRuntime) clock() time.Time {
	if rt.now == nil {
		return time.Now()
	}
	return rt.now()
}

// OfficersConfig is EnableOfficers' input.
type OfficersConfig struct {
	Officers []officers.Officer
	// HTTPClient is the HQ client's transport; nil uses officers.NewClient's.
	HTTPClient *http.Client
	// Backend builds each officer's backend (officers.Backend); nil is, per
	// officer, HQ over HTTPClient (officers.NewClient) or — for a builtin
	// officer — the built-in backend over Builtin. For tests.
	Backend func(officers.Officer) officers.Backend
	// Builtin is the built-in backend's store (internal/officers/builtin,
	// migration 0118) in muster's own database. REQUIRED when an officer
	// declares "backend": "builtin": EnableOfficers refuses one without it.
	Builtin *builtin.Store
	// Now is the wake loop's clock; nil is time.Now. For tests.
	Now func() time.Time
	// InstallSession installs the officer runtime's session source on the
	// provisioner (agentprovision.Adapter.UseClaudeCodeSession). REQUIRED:
	// without it an instance's Claude Code session would stop on a permission
	// prompt for its first tool call (it runs with every tool allowed only
	// through this source) and never run its model, and nothing would say so —
	// EnableOfficers refuses instead.
	InstallSession func(source func(agents.Agent) (agentspec.ClaudeCodeSession, bool))
	// ShipGitHub, ShipHosts and ShipHTTP replace a shipping officer's GitHub
	// side (its App, the REST client and the hosts the push gate serves) and
	// the rollout watch's HTTP client. For tests; nil/zero is production.
	ShipGitHub func(officers.Officer) officership.GitHub
	// ShipGitHubRepo, when set with ShipGitHub, is the GitHub of each of a
	// draft officer's ship repositories by name (tests); unset, ShipGitHub's
	// answers for every one.
	ShipGitHubRepo func(o officers.Officer, repo string) officership.GitHub
	ShipHosts      [2]string
	ShipHTTP       *http.Client
	// ShipCI replaces a ci_logs officer's cluster reader (tests); nil is the
	// in-cluster API with a token minted per call for its service_account.
	ShipCI func(officers.Officer) officership.CIReader
	// NoShipResume skips resuming rollout watches and starting the daily
	// orphan-branch report at boot (tests).
	NoShipResume bool
	// TranscriptSource replaces where a transcript-enabled officer's
	// instances' transcripts are read (tests); nil is each instance's ccd,
	// through the agent gateway (agentgateway.Gateway.Transcript).
	TranscriptSource transcript.Source
	// TranscriptInterval replaces the forwarder's cadence (tests); 0 is
	// transcript.DefaultInterval. NoTranscriptRun skips running its loop
	// (tests drive Tick).
	TranscriptInterval time.Duration
	NoTranscriptRun    bool
	// UsageSource replaces where an instance's Claude usage is read (tests);
	// nil is each instance's ccd through the agent gateway
	// (agentgateway.Gateway.Usage). UsageInterval replaces the collector's
	// cadence (tests); 0 is usage.DefaultInterval. NoUsageRun skips running
	// its loop (tests drive Tick).
	UsageSource   usage.Source
	UsageInterval time.Duration
	NoUsageRun    bool
	// PoolsBackoff replaces the pools reporter's retry waits (tests).
	PoolsBackoff []time.Duration
	// LLMStore keeps the LLM proxy's per-day usage (the budget); REQUIRED
	// when an officer declares `llm`. LLMUpstream replaces the provider's API
	// base and LLMHTTP its transport (tests); ""/nil is production.
	LLMStore llmproxy.Store
	// Sessions keeps the judgement session cap's count
	// (officers.PGSessionStore); REQUIRED when an officer declares
	// tiers.judgement.max_sessions_per_day.
	Sessions officers.SessionStore
	// Restarts is the auto-restart's record (officers.PGRestartStore,
	// migration 0161); nil keeps it in memory, per process.
	Restarts officers.RestartStore
	// PRLoops keeps draft officers' review-loop announcements
	// (officers.PGPRLoopStore); nil keeps them in this process's memory,
	// which a restart forgets.
	PRLoops officers.PRLoopStore
	// PublicBranches keeps draft officers' public branches and the item each
	// is for (officers.PGPublicBranchStore, migration 0138); nil keeps them in
	// this process's memory, which a restart forgets.
	PublicBranches officers.PublicBranchStore
	// ConnectorBlocks keeps which items each officer instance held while a
	// pending connector grant refused it, so approving that grant re-runs
	// them (officers.PGConnectorBlockStore, migration 0148); nil keeps them
	// in this process's memory, which a restart forgets.
	ConnectorBlocks officers.ConnectorBlockStore
	LLMUpstream     string
	LLMHTTP         *http.Client
	// HostSpawns keeps the host officers' spawned sessions
	// (officers.PGHostSpawnStore, migration 0125); REQUIRED when an officer
	// declares "runtime": "host" (docs/design/officers-on-hosts.md).
	HostSpawns officers.HostSpawnStore
	// AutoRestart is the wake loop's bounded restart of an instance in error
	// (officers.AutoRestart; MUSTER_OFFICER_AUTO_RESTART_AFTER and
	// MUSTER_OFFICER_AUTO_RESTART_MAX_PER_DAY). The zero value is off.
	AutoRestart officers.AutoRestart
	// UnclaimedAfter is how long a host instance may run without claiming an
	// item before its owner is told (officer_unclaimed.go); 0 is
	// DefaultOfficerUnclaimedAfter.
	UnclaimedAfter time.Duration
	// RefreshEvery is how often the wake loop re-reads the DB-declared
	// officers (officers.Config.RefreshEvery); 0 is the default. For tests.
	RefreshEvery time.Duration
	// ChatHeartbeatEvery and ChatReplyRetries replace the chat runner's
	// heartbeat cadence and reply retry waits (tests); zero/nil are the
	// defaults.
	ChatHeartbeatEvery time.Duration
	ChatReplyRetries   []time.Duration
	// ImagePullSecret is MUSTER_OFFICER_IMAGE_PULL_SECRET: the Secret an
	// officer pool's OWN runner image (`image`) is pulled with
	// (agentspec.ClaudeCodeSession.ImagePullSecret, which reaches the pod spec
	// only beside that image). "" is none.
	ImagePullSecret string
}

// transcriptReader is the agent gateway's transcript read
// (agentgateway.Gateway.Transcript), asserted rather than added to Gateway:
// it is the officer transcript forwarder's alone.
type transcriptReader interface {
	Transcript(ctx context.Context, a agents.Agent, session string, after int64, backfill int) (cctranscript.Page, error)
}

var _ transcriptReader = (*agentgateway.Gateway)(nil)

// gatewayTranscripts is transcript.Source over the agent gateway: the
// instance agent by name (as its officer's owner), then its ccd's
// GET /v1/transcript.
type gatewayTranscripts struct {
	s *Server
	r transcriptReader
}

func (g gatewayTranscripts) Read(ctx context.Context, o officers.Officer, instance string, q transcript.Query) (cctranscript.Page, error) {
	octx := g.s.officerPrincipal(ctx, o.Owner)
	a, err := g.s.ext.Agents.GetByName(octx, instance)
	if err != nil {
		return cctranscript.Page{}, fmt.Errorf("officers: instance %s: %w", instance, err)
	}
	return g.r.Transcript(octx, a, q.Session, q.After, q.Backfill)
}

// newTranscriptForwarder builds the forwarder when a declared officer enables
// `transcript`, and nil otherwise. An enabled officer with no way to read a
// transcript is a boot refusal, not a silent no-op.
func (s *Server) newTranscriptForwarder(cfg OfficersConfig, hq *officers.Backends) (*transcript.Forwarder, error) {
	var on []string
	for _, o := range cfg.Officers {
		if o.TranscriptEnabled() {
			on = append(on, o.ID)
		}
	}
	if len(on) == 0 {
		return nil, nil
	}
	src := cfg.TranscriptSource
	if src == nil {
		r, ok := s.ext.Gateway.(transcriptReader)
		if !ok {
			return nil, fmt.Errorf("officers: %s enable transcript forwarding, but the agent gateway cannot read an instance's transcript", strings.Join(on, ", "))
		}
		src = gatewayTranscripts{s: s, r: r}
	}
	f, err := transcript.New(transcript.Config{Source: src, Poster: transcript.NewBackendPoster(hq), Logger: s.logger, Now: cfg.Now, Interval: cfg.TranscriptInterval})
	if err != nil {
		return nil, err
	}
	s.logger.Printf("officers: forwarding instance transcripts to HQ for %s while an instance holds a claimed item", strings.Join(on, ", "))
	return f, nil
}

// officersServing is the officers of list whose declared backend serves
// capability (officers.DeclaredCapabilities): the pools and usage pushes run
// for those only, so an officer whose backend keeps no such ledger (a builtin
// one) is never pushed to — and never logged as refusing — every cadence.
func officersServing(list []officers.Officer, capability string) []officers.Officer {
	var out []officers.Officer
	for _, o := range list {
		if officers.DeclaredCapabilities(o).Has(capability) {
			out = append(out, o)
		}
	}
	return out
}

// officerBackendBuilder is how EnableOfficers builds each officer's backend:
// cfg.Backend when set (tests); otherwise HQ over cfg.HTTPClient for an HQ
// officer, and the built-in backend over cfg.Builtin for a builtin one — its
// every statement run AS THE OFFICER'S OWNER (officerPrincipal), so row-level
// security confines it to that owner's types, officers and items. A builtin
// officer with no store is refused here, at boot: never run against nothing.
func (s *Server) officerBackendBuilder(cfg OfficersConfig) (func(officers.Officer) officers.Backend, error) {
	if cfg.Backend != nil {
		return cfg.Backend, nil
	}
	var builtins []string
	for _, o := range cfg.Officers {
		if o.IsBuiltin() {
			builtins = append(builtins, o.ID)
		}
	}
	if len(builtins) > 0 && cfg.Builtin == nil {
		return nil, fmt.Errorf("officers: %s declare \"backend\": \"builtin\", whose queue lives in muster's database, and there is none (set DATABASE_URL)",
			strings.Join(builtins, ", "))
	}
	hc, store, now := cfg.HTTPClient, cfg.Builtin, cfg.Now
	return func(o officers.Officer) officers.Backend {
		if o.IsBuiltin() {
			owner := o.Owner
			return builtin.NewBackend(store, o, now, func(ctx context.Context) context.Context { return s.officerPrincipal(ctx, owner) }).
				WithAsker(officerAsker{s: s})
		}
		return officers.NewClient(o, hc)
	}, nil
}

// EnableOfficers turns the officer runtime on and returns its wake loop for the
// caller to Run. It needs the agents store and the provisioner (an officer is
// nothing without instances), and the claude-code kind.
func (s *Server) EnableOfficers(cfg OfficersConfig) (*officers.Reconciler, error) {
	// With the built-in store an empty MUSTER_OFFICERS is a runtime for the
	// officers declared in the database (officer_declared.go); without it,
	// there is nothing to run.
	if len(cfg.Officers) == 0 && cfg.Builtin == nil {
		return nil, errors.New("officers: none declared")
	}
	// 🔴 WHAT EACH RUNTIME NEEDS, REFUSED AT BOOT BY RUNTIME
	// (docs/design/officers-on-hosts.md): a pod officer needs the agents store,
	// a provisioner and the claude-code kind; a host officer needs a mirror hub
	// and the host spawn store, and none of the pod's — so a Compose stack
	// with host officers only boots without a cluster.
	var pods, hosts []string
	for _, o := range cfg.Officers {
		if o.OnHost() {
			hosts = append(hosts, o.ID)
		} else {
			pods = append(pods, o.ID)
		}
	}
	if len(pods) > 0 {
		if s.ext.Agents == nil || s.ext.Provisioner == nil {
			return nil, fmt.Errorf("officers: %s run in pods, which need the agents store and an agent provisioner (or declare \"runtime\": \"host\")", strings.Join(pods, ", "))
		}
		if cfg.InstallSession == nil {
			return nil, errors.New("officers: no way to install the instances' session settings on the provisioner (every tool allowed, officer tools, model)")
		}
		if !s.ext.kindEnabled(agents.KindClaudeCode) {
			return nil, fmt.Errorf("officers: instances run on the %s kind, which MUSTER_AGENT_KINDS does not enable", agents.KindClaudeCode)
		}
	}
	if len(hosts) > 0 {
		if s.ext.Mirror == nil {
			return nil, fmt.Errorf(officerHostNoMirror, strings.Join(hosts, ", "))
		}
		if cfg.HostSpawns == nil {
			return nil, fmt.Errorf(officerHostNoStore, strings.Join(hosts, ", "))
		}
		if s.ext.Agents == nil {
			return nil, fmt.Errorf(officerHostNoAgents, strings.Join(hosts, ", "))
		}
	}
	// 🔴 AN OWNER IS A PERSON OR A TEAM, AND IT EXISTS (team sharing phase 3,
	// officers.CheckOwners): a team owns a TEAM OFFICER, whose runtime acts as
	// the team — in pods, or on a team host (2b.5); an owner naming nobody, or
	// a team's host officer that declares connectors (G3), is refused here
	// rather than on its first pass.
	if err := officers.CheckOwners(cfg.Officers, s.officerOwnerKind); err != nil {
		return nil, err
	}
	reg := officers.NewRegistry(cfg.Officers)
	build, err := s.officerBackendBuilder(cfg)
	if err != nil {
		return nil, err
	}
	hq := officers.NewBackends(build)
	fwd, err := s.newTranscriptForwarder(cfg, hq)
	if err != nil {
		return nil, err
	}
	px, err := s.newLLMProxy(cfg)
	if err != nil {
		return nil, err
	}
	uc, err := s.newUsageCollector(cfg, hq)
	if err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	// The judgement session cap's ledger is persisted (officers.
	// PGSessionStore, migration 0082), read and written as the officer's
	// owner. An officer that declares a cap with no store is refused: a cap
	// that cannot be kept is no cap.
	var judgement *officers.JudgementLedger
	if cfg.Sessions != nil {
		judgement = officers.NewJudgementLedger(cfg.Sessions, func(ctx context.Context, o officers.Officer) context.Context {
			return s.officerPrincipal(ctx, o.Owner)
		}, s.logger.Printf)
	} else {
		for _, o := range cfg.Officers {
			if o.HasSessionCap() {
				return nil, fmt.Errorf("officers: %s declares tiers.judgement.max_sessions_per_day, but there is no session store to keep the count in (it needs the database)", o.ID)
			}
		}
	}
	// The declared pools, pushed to each officer's HQ with its token: at boot
	// (MUSTER_OFFICERS is read only then, so a change is a restart) and on
	// the usage collector's cadence, sessions_today read through the SAME
	// ledger that enforces the cap.
	pools, err := officers.NewPoolsReporter(officers.PoolsReporterConfig{Officers: officersServing(cfg.Officers, officers.CapPools), Poster: officers.NewBackendPoolsPoster(hq),
		Ledger: judgement, Logger: s.logger, Now: now, Interval: cfg.UsageInterval, Backoff: cfg.PoolsBackoff})
	if err != nil {
		return nil, err
	}
	// The chat turns (officer_chat.go): run when the agents store can keep a
	// chat's session (the database); a turn ends with the server's streams.
	var chats *officers.ChatRunner
	if _, ok := s.ext.Agents.(agents.OfficerChatStore); ok {
		chats, err = officers.NewChatRunner(officers.ChatRunnerConfig{Lanes: officerChatLanes{s: s}, Backend: hq.Backend, Logger: s.logger,
			Context: s.streams.context(), HeartbeatEvery: cfg.ChatHeartbeatEvery, ReplyRetries: cfg.ChatReplyRetries,
			// The wake loop's clock: a chat's backoff is read on the same one
			// Dispatch is handed (nil is time.Now).
			Now: cfg.Now})
		if err != nil {
			return nil, err
		}
	}
	rcfg := officers.Config{Registry: reg, HQ: hq, Fleet: officerFleet{s: s}, Logger: s.logger, Now: now, Judgement: judgement,
		CapHold: s.officerCapHoldNotice, BudgetAlert: s.officerBudgetNotice, RefreshEvery: cfg.RefreshEvery, Kickoff: s.officerKickoff, Chats: chats,
		AutoRestart: cfg.AutoRestart, AutoRestartNotice: s.officerAutoRestartNotice,
		Restarts: cfg.Restarts, RestartContext: func(ctx context.Context, o officers.Officer) context.Context { return s.officerPrincipal(ctx, o.Owner) }}
	if cfg.Builtin != nil {
		// routine_kinds: the store defers a routine item to its officer's next
		// routine_wake_at, reading the LIVE roster (officers.Registry.ForOwner).
		cfg.Builtin.SetRoutinePolicy(reg.ForOwner)
		// The DB-declared officers (migration 0136) reach every replica's
		// loop on its refresh, whoever wrote them.
		rcfg.Refresh = s.refreshDeclaredOfficers
	}
	switch {
	case fwd != nil && px != nil:
		sync := llmSync(px, reg)
		rcfg.Observed = func(o officers.Officer, claimed []officers.Item, at time.Time) {
			fwd.Sync(o, claimed, at)
			sync(o, claimed, at)
		}
	case fwd != nil:
		rcfg.Observed = fwd.Sync
	case px != nil:
		rcfg.Observed = llmSync(px, reg)
	}
	rec, err := officers.NewReconciler(rcfg)
	if err != nil {
		return nil, err
	}
	kube, err := newKubeRuntime(cfg.Officers, now)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	s.officerRT = &officerRuntime{reg: reg, env: append([]officers.Officer(nil), cfg.Officers...), store: cfg.Builtin,
		sessionInstalled: cfg.InstallSession != nil,
		hq:               hq, rec: rec, fwd: fwd, usage: uc, pools: pools, llm: px, judgement: judgement, now: now, kube: kube, chats: chats,
		reads:           officers.NewReadBudget(officers.ReadsPerItem, now),
		hostSpawns:      cfg.HostSpawns,
		blocks:          cfg.ConnectorBlocks,
		unclaimedAfter:  cfg.UnclaimedAfter,
		claimOwner:      fmt.Sprintf("officer-reprompt/%s/%d", host, os.Getpid()),
		imagePullSecret: cfg.ImagePullSecret}
	if err := s.enableShips(cfg); err != nil {
		s.officerRT = nil
		return nil, err
	}
	if cfg.InstallSession != nil {
		cfg.InstallSession(s.OfficerClaudeCodeSession)
	}
	if fwd != nil && !cfg.NoTranscriptRun {
		// Ends with the server's streams (EndStreams, at shutdown).
		ctx := s.streams.context()
		safeGo(s.logger, "officer transcript forwarder", func() { fwd.Run(ctx) })
	}
	if uc != nil && !cfg.NoUsageRun {
		// Ends with the server's streams (EndStreams, at shutdown).
		ctx := s.streams.context()
		safeGo(s.logger, "officer usage collector", func() { uc.Run(ctx) })
	}
	for _, o := range reg.All() {
		if o.OnHost() {
			s.logger.Print(officerHostLine(o))
			continue
		}
		repo := "none"
		if o.Repo != "" {
			repo = o.Repo + " (github connector, read through the proxy; no checkout)"
		}
		if o.Tiers == nil {
			s.logger.Printf("officers: %s on: instances %s..%s, idle scale-to-zero after %s, poll %s, repo %s",
				o.ID, o.InstanceName(1), o.InstanceName(o.MaxInstances), o.IdleAfter, o.PollInterval, repo)
			continue
		}
		var pools []string
		for _, p := range o.Pools() {
			pools = append(pools, fmt.Sprintf("%s instances %s..%s (model %s)", p.Tier, p.InstanceName(1), p.InstanceName(p.MaxInstances), p.Model))
		}
		s.logger.Printf("officers: %s on: %s, idle scale-to-zero after %s per pool, poll %s, repo %s",
			o.ID, strings.Join(pools, "; "), o.IdleAfter, o.PollInterval, repo)
	}
	for _, o := range reg.All() {
		s.logOfficerBackend(o)
	}
	if cfg.Builtin != nil {
		// The officers declared in the database, at boot: a failure leaves the
		// env roster running and the wake loop's refresh tries again.
		ctx, cancel := context.WithTimeout(context.Background(), officerBootReadTimeout)
		if err := s.reloadDeclaredOfficers(ctx); err != nil {
			s.logger.Printf("officers: reading the officers declared in the database at boot: %v; the wake loop retries", err)
		}
		cancel()
	}
	return rec, nil
}

// officerBootReadTimeout bounds the boot read of a builtin officer's switch.
const officerBootReadTimeout = 5 * time.Second

// logOfficerBackend is officer o's boot line about its backend: which one,
// what it serves, and the officer tools each pool is offered — so a tool a
// backend does not serve is visibly absent, never silently (the design's
// "hidden tools" risk). For a builtin officer, also whether the built-in store
// defines it (and its switch), and the backup duty its queue brings.
func (s *Server) logOfficerBackend(o officers.Officer) {
	b := s.officerRT.hq.Backend(o)
	caps := b.Capabilities()
	var offered []string
	for _, p := range o.Pools() {
		var tools []string
		for _, d := range officerToolDefs() {
			if officers.ToolServed(p, caps, d.Name) {
				tools = append(tools, d.Name)
			}
		}
		label := "tools"
		if p.Tier != "" {
			label = p.Tier + " tools"
		}
		offered = append(offered, label+" "+strings.Join(tools, ","))
	}
	s.logger.Printf("officers: %s backend %s (%s; serves %s); %s", o.ID, o.BackendKind(), caps.Contract, strings.Join(caps.Verbs, ","), strings.Join(offered, "; "))
	if !o.IsBuiltin() {
		return
	}
	if rr := o.StandingReadRepos(); len(rr) > 0 {
		at := "the default branch"
		if o.ReadAt == officers.ReadAtRelease {
			at = "muster's release tag " + o.ReleaseTag
			if o.ReleaseTag == "" {
				at = "the default branch (this build is not a release, so it has no tag)"
			}
		}
		s.logger.Printf("officers: %s: standing public read of %s at %s (no approval; anonymous, reads only, each checked public with GitHub)",
			o.ID, strings.Join(rr, ", "), at)
	}
	ctx, cancel := context.WithTimeout(context.Background(), officerBootReadTimeout)
	defer cancel()
	on, err := b.Enabled(ctx)
	switch {
	case err != nil:
		s.logger.Printf("officers: %s: the built-in store does not answer for it (%v): define it with `muster officer def put %s` (docs/officers-builtin.md); until then its wake loop fails every pass", o.ID, err, o.ID)
	case !on:
		s.logger.Printf("officers: %s: defined in the built-in store and switched OFF: no instance runs until it is switched on", o.ID)
	default:
		s.logger.Printf("officers: %s: defined in the built-in store and switched on", o.ID)
	}
	s.logger.Printf("officers: %s: its type, charter and queue live in muster's database — back that database up", o.ID)
}

// NoClaudeAccount is THE rule for whether claude-code agent a runs on none of
// the shared Claude accounts although its row names one, because its model
// calls go through muster's LLM proxy:
//
//   - an instance of a declared officer's pool on the proxy
//     (instanceOnNoClaudeAccount);
//   - the chief on the LLM route (the spec builder's own predicate,
//     ChiefLLM.Routed, which places no Claude token in that chief's pod).
//
// 🔴 SUCH AN AGENT'S rate_limited / auth_failed MARKS NO ACCOUNT AND MOVES THE
// AGENT NOWHERE: the proxy passes an upstream provider's 429 back as it is, so
// it is not the account's. cmd/muster-server installs this on the agent
// gateway (agentgateway.Gateway.UseNoAccount), and failoverJob asks it, so a
// turn's marks and a job's failover follow one rule.
//
// ⚠ A ROUTE THAT CANNOT BE DECIDED COUNTS AS "NO ACCOUNT" (logged). The two
// wrong answers are not equal: "no account" for a chief that is in fact on its
// account skips one mark and one failover for that chief alone; "its account"
// for a chief on the proxy cools, or fails over from, an account every other
// agent on it shares, on a fault that was not the account's.
func (s *Server) NoClaudeAccount(ctx context.Context, a agents.Agent) bool {
	if s.instanceOnNoClaudeAccount(a) {
		return true
	}
	if s.chiefLLM == nil || s.chiefLLM.routed == nil {
		return false
	}
	routed, err := s.chiefLLM.routed(ctx, a)
	if err != nil {
		s.logger.Printf("agents: %s: deciding whether it is on the LLM route: %v; marking no Claude account for its failure", a.Name, err)
		return true
	}
	return routed
}

// instanceOnNoClaudeAccount is true for an instance of a declared officer's
// pool on the LLM proxy, false for every other agent. It is the accounts'
// attribution answer (officerPoolOf, officers.ProxyAgent), so a turn's
// account marks and the usage credited to an account follow one rule.
func (s *Server) instanceOnNoClaudeAccount(a agents.Agent) bool {
	_, _, onProxy, ok := s.officerPoolOf(a.Name)
	return ok && onProxy
}

// officerOf is THE binding rule: the declared officer agent a is an instance
// of, and which instance. Not an officer instance → ok false. The officer is
// the instance's pool (officers.Registry.ForAgentName): a tiered officer's
// routine instance gets the routine Tier, Model and MaxInstances.
//
// 🔴 THE NAME AND THE OWNER (round 0's F2). An agent binds to the officer its
// name parses as ONLY when the agent is that officer's owner's: a name alone
// is not enough once officers are created and deleted at runtime, because an
// agent row can outlive its officer (a teardown that failed, a replica that
// started an instance after the delete) and a later officer of another owner
// could come to parse the same name. The host door has always held this
// (officerHostCaller); this is the same rule for every other door.
func (s *Server) officerOf(a agents.Agent) (officers.Officer, int, bool) {
	if s.officerRT == nil {
		return officers.Officer{}, 0, false
	}
	o, n, ok := s.officerRT.reg.ForAgentName(a.Name)
	if !ok || a.OwnerID != o.Owner {
		return officers.Officer{}, 0, false
	}
	return o, n, true
}

// agentRepo is THE repository agent a acts on for its connectors: the one
// connectors.Caller.Repo carries (callerOf), so the GitHub App's token is
// minted for it and only it (githubapp.App.Credential), and the one every
// operator-facing description of a `github` grant, the shared-approval key and
// the App-problem line read.
//
//   - An instance of a declared officer: the officer's configured repo
//     (MUSTER_OFFICERS `repo`, officers.Officer.Repo), "" when it has none.
//     Read from the registry, NEVER the row: the instance row carries no
//     repository, because a row repository is also what makes ccd clone it
//     and start the session pre-trusted in that checkout (podRepo,
//     agents.NeedsCheckout) — an officer reads its repository through the
//     proxy instead. So the configured value is the only copy, and a config
//     change applies to existing instances without touching their rows.
//   - Every other agent: its row's repository, as before.
//
// 🔴 ONE RULE, ONE PLACE. A site that read a.Repo directly would describe an
// officer's approved grant as "NOTHING usable" while the proxy served it, or
// key its shared approval on no repository.
func (s *Server) agentRepo(a agents.Agent) string {
	if o, _, ok := s.officerOf(a); ok {
		return o.Repo
	}
	return a.Repo
}

// agentReadRepos are the further repositories agent a may READ through the
// GitHub App connector (connectors.Caller.ReadRepos): an officer instance's
// officer's `read_repos` (officers.Officer.ApprovalReadRepos, read from the
// registry like agentRepo) — none when they are a standing read
// (agentPublicReadRepos) — nil for every other agent.
func (s *Server) agentReadRepos(a agents.Agent) []string {
	if o, _, ok := s.officerOf(a); ok {
		return o.ApprovalReadRepos()
	}
	return nil
}

// agentPublicReadRepos are the PUBLIC repositories agent a reads through the
// standing public read (connectors.Caller.PublicReadRepos): an instance of a
// builtin officer declaring read_grant "standing"
// (officers.Officer.StandingReadRepos, the one rule), nil for every other
// agent — an HQ officer's, a shipping officer's and every task agent's
// included.
func (s *Server) agentPublicReadRepos(a agents.Agent) []string {
	if o, _, ok := s.officerOf(a); ok {
		return o.StandingReadRepos()
	}
	return nil
}

// agentRepoScope is the repository set a's grant is approved for and
// reconciled against: connectors.RepoScope of agentRepo and agentReadRepos —
// exactly agentRepo for every agent without read repositories.
func (s *Server) agentRepoScope(a agents.Agent) string {
	return connectors.RepoScope(s.agentRepo(a), s.agentReadRepos(a))
}

// officerConnectorRefusal is why officer instance a may not use connector
// name, or nil. 🔴 AN OFFICER USES THE `github` CONNECTOR, AND ONLY THE
// CONNECTORS ITS OFFICER DECLARES (operator decision, PR #337 round 1, widened
// by the officer's `connectors` field: officers.MayRequestConnector): a
// request for any other connector — a stored chat, mail or cloud credential
// its owner happens to hold — is refused at every door that would file or
// serve one (AgentRequestConnector, the proxy, postgres_query, and the owner's
// approval), not left for the owner to notice. Every other agent: nil.
func (s *Server) officerConnectorRefusal(a agents.Agent, name string) error {
	o, _, ok := s.officerOf(a)
	if !ok || officers.MayRequestConnector(o, name) {
		return nil
	}
	return errOfficerConnector{agent: a.Name, officer: o.ID, connector: name, declared: o.Connectors, onHost: o.OnHost()}
}

// errOfficerConnector is officerConnectorRefusal's refusal. It is
// connectors.ErrInvalid (errors.Is), so the agent's request route answers 400
// and its connector_request tool shows it verbatim, as any invalid request.
type errOfficerConnector struct {
	agent, officer, connector string
	declared                  []string
	// onHost: a host officer, which may use only the connectors it declares
	// (officers.MayRequestConnector refuses it github).
	onHost bool
}

func (e errOfficerConnector) Error() string {
	if e.onHost {
		return fmt.Sprintf("%s is an instance of host officer %s, which may use only the connectors it declares (%s); %q was refused",
			e.agent, e.officer, strings.Join(e.declared, ", "), e.connector)
	}
	if len(e.declared) == 0 {
		return fmt.Sprintf("%s is an instance of officer %s, and an officer may use only the %q connector (to read its "+
			"repository); %q was refused", e.agent, e.officer, officers.GitHubConnector, e.connector)
	}
	return fmt.Sprintf("%s is an instance of officer %s, which may use only the %q connector and the connectors it "+
		"declares (%s); %q was refused", e.agent, e.officer, officers.GitHubConnector, strings.Join(e.declared, ", "), e.connector)
}

func (e errOfficerConnector) Is(target error) bool { return target == connectors.ErrInvalid }

// officerOwnerKind is how an officer's owner id reads in users, for
// officers.CheckOwners at boot (read under System: users is self-read).
func (s *Server) officerOwnerKind(owner int64) (officers.OwnerKind, error) {
	a, err := s.accountOfChecked(context.Background(), owner)
	switch {
	case errors.Is(err, users.ErrNotFound):
		return officers.OwnerMissing, nil
	case err != nil:
		return officers.OwnerMissing, err
	case a.Team:
		return officers.OwnerTeam, nil
	}
	return officers.OwnerPerson, nil
}

// officerPrincipal is ctx acting as owner, through authz.ViaOfficer: how the
// wake loop reads and moves one officer's instance agents, so row-level
// security confines it to that owner's rows. A TEAM owner (a team officer,
// team sharing phase 3) is the team acting principal: muster.uid = the team,
// no memberships, never scope all.
func (s *Server) officerPrincipal(ctx context.Context, owner int64) context.Context {
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaOfficer))
}

// --- the fleet ---------------------------------------------------------------

// officerInstancesWithin bounds the fleet's read of the provisioner's
// instances, as the stopped-agent reaper's is bounded.
const officerInstancesWithin = 30 * time.Second

// officerFleet is officers.Fleet over this server's agents and provisioner.
type officerFleet struct{ s *Server }

// kickoffRearmer is the agents store's re-arm (agents.PGStore.RearmKickoff),
// asserted rather than added to agents.Store: it is the officer runtime's
// alone.
type kickoffRearmer interface {
	RearmKickoff(ctx context.Context, id int64, note string) error
}

var _ kickoffRearmer = (*agents.PGStore)(nil)

// instanceUp is whether the wake loop counts instance agent a as occupying
// its slot.
//
//   - Its live status (liveStatusIndexed, the one composer the cards use) is
//     running, provisioning or pending: up.
//   - It is `error` (crash-looping, image pull failing, a failed apply): ALSO
//     up. Counting it down would have the loop re-Start it every poll — a
//     restart storm that re-arms its first turn each time. It holds its slot
//     until it recovers, an operator acts, or — for an error class a restart
//     fixes (classifyInstanceError) — the wake loop's bounded auto-restart
//     restarts it in place (internal/officers autorestart.go: backoff and a
//     per-officer daily cap). Any claim it held lapses with its lease, or is
//     released by that restart.
//   - Its ROW says provisioning or pending but no instance is visible yet: up.
//     That is a create or a Start in flight (the row is written before the
//     backend shows the Deployment), and counting it down would start it twice.
func instanceUp(a agents.Agent, idx map[string]*provision.Instance) bool {
	switch liveStatusIndexed(a, idx) {
	case agents.StatusRunning, agents.StatusProvisioning, agents.StatusPending, agents.StatusError:
		return true
	}
	return idx[a.Name] == nil && (a.Status == agents.StatusProvisioning || a.Status == agents.StatusPending)
}

func (f officerFleet) byName(ctx context.Context, o officers.Officer) (map[string]agents.Agent, error) {
	list, err := f.s.ext.Agents.List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]agents.Agent{}
	for _, a := range list {
		if bo, _, ok := f.s.officerRT.reg.ForAgentName(a.Name); ok && bo.Key() == o.Key() && bo.Tier == o.Tier {
			out[a.Name] = a
		}
	}
	return out, nil
}

// Instances implements officers.Fleet for pool o (an untiered officer, or one
// pool of a tiered one). It FAILS BLIND: an unreadable agent table or
// provisioner is an error, never "nothing is up".
func (f officerFleet) Instances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	if o.OnHost() {
		return f.hostInstances(ctx, o)
	}
	octx := f.s.officerPrincipal(ctx, o.Owner)
	rows, err := f.byName(octx, o)
	if err != nil {
		return nil, fmt.Errorf("officers: read agents: %w", err)
	}
	// 🔴 ONLY THIS POOL'S INSTANCES ARE READ, and only those with an agent row
	// (InstanceReader): a slot with no row has no instance to ask about. The
	// whole-fleet read this replaced cost every agent namespace in the cluster
	// per officer per pass, and with several officers checked at once it ran
	// the provisioner's client out of request budget — every pass then failed
	// blind and no officer started anything.
	have := make([]agents.Agent, 0, o.Slots())
	for n := 1; n <= o.Slots(); n++ {
		if a, ok := rows[o.InstanceName(n)]; ok {
			have = append(have, a)
		}
	}
	idx := f.s.instancesOfWithin(octx, have, officerInstancesWithin)
	if idx == nil {
		// ErrBlind: the provisioner did not answer (an error, or no answer
		// within the bound — both logged by the read). The officer runtime
		// retries exactly this at the poll interval rather than backing off.
		return nil, fmt.Errorf("officers: the provisioner's instances could not be read (%w)", provision.ErrBlind)
	}
	// 🔴 EVERY SLOT, chat-only instances included (Officer.Slots): an
	// instance the loop started for chats and this list hid would read as
	// down, and be started again every pass.
	out := make([]officers.InstanceState, 0, o.Slots())
	for n := 1; n <= o.Slots(); n++ {
		name := o.InstanceName(n)
		a, ok := rows[name]
		in := officers.InstanceState{N: n, Name: name, Exists: ok, Up: ok && instanceUp(a, idx),
			Error:         ok && liveStatusIndexed(a, idx) == agents.StatusError,
			PromptPending: ok && promptPending(a),
			Spent:         f.s.officerRT.spent(o, name)}
		if in.Error {
			in.ErrorClass = classifyInstanceError(a, idx[name])
			in.ErrorText = instanceErrorText(a, idx[name])
		}
		switch {
		case in.Up:
		case !ok:
			in.Down = "first start (it has no agent yet)"
		default:
			in.Down = "restart: its agent is " + string(liveStatusIndexed(a, idx))
		}
		out = append(out, in)
	}
	return out, nil
}

// Start implements officers.Fleet: create instance n with kickoff as its first
// turn, or — when its agent exists — re-arm its first turn and start it.
func (f officerFleet) Start(ctx context.Context, o officers.Officer, n int, kickoff string) error {
	// 🔴 NOT FOR AN OFFICER THAT IS GONE. A pass reads the roster once; a
	// DB-declared officer deleted meanwhile has had its instances torn down,
	// and a start now would leave an agent no officer owns.
	if !f.stillDeclared(ctx, o) {
		return fmt.Errorf("officers: %s is no longer declared; not starting %s", o.ID, o.InstanceName(n))
	}
	err := f.start(ctx, o, n, kickoff)
	// 🔴 AND ASKED AGAIN AFTER (round 0's F2). The check above and the create
	// are not one step: a delete landing between them swaps the roster and
	// runs its teardown before this create — which then leaves exactly the
	// orphan the check exists to prevent. So the officer is asked about again
	// once the start is over — whether it succeeded or not, since a host
	// start that refused to send its spawn has already created its row — and
	// the instance removed if the officer is gone. Every delete's teardown
	// runs after its swap, so either the teardown sees this instance or this
	// check sees the delete.
	//
	// 🔴 THE TEARDOWN HERE IS THE DATABASE'S, NOT THE ROSTER'S (round 2's
	// first finding). A delete made on ANOTHER replica is seen here only in
	// the database: this replica's roster still holds the officer until its
	// next refresh, so the delete protection (isDeclaredInstanceRow, which
	// reads the roster) refused the teardown and the orphan stayed — while
	// the start answered "removed it". teardownGoneInstance passes the
	// officer to destroyAgent as gone (destroyHooks.goneOfficer), which lifts
	// the officer half of the protection for that officer's own row, and only
	// once the database says it is gone.
	if !f.stillDeclared(ctx, o) {
		name := o.InstanceName(n)
		switch f.s.teardownGoneInstance(ctx, o, n) {
		case teardownRemoved:
			f.s.logger.Printf("officers: %s was deleted while %s started; removed it", o.ID, name)
			return fmt.Errorf("officers: %s was deleted while %s started; removed it", o.ID, name)
		case teardownFailed:
			f.s.logger.Printf("officers: %s was deleted while %s started; could NOT remove it", o.ID, name)
			return fmt.Errorf("officers: %s was deleted while %s started; could not remove it (see the log; `muster agent rm %s` removes it)", o.ID, name, name)
		}
		return fmt.Errorf("officers: %s was deleted while %s started; it has no agent to remove", o.ID, name)
	}
	return err
}

// stillDeclared reports whether o is still the officer it was: in the live
// roster under its key, the same owner and id — and, for a DB-declared
// officer, still in the database with the same slug, so a delete made on
// ANOTHER replica (whose roster this one reads up to a refresh later) is
// seen too. An unreadable database is not "still there".
func (f officerFleet) stillDeclared(ctx context.Context, o officers.Officer) bool {
	cur, err := f.s.officerRT.reg.Get(o.Key())
	if err != nil || cur.Owner != o.Owner || cur.ID != o.ID {
		return false
	}
	if !o.FromDB() || f.s.officerRT.store == nil {
		return true
	}
	row, err := f.s.officerRT.store.GetDeclared(f.s.officerPrincipal(ctx, o.Owner), o.Owner, o.ID)
	return err == nil && row.Fields.InstanceSlug == o.InstanceSlug
}

// start is Start once the officer is known to be declared.
func (f officerFleet) start(ctx context.Context, o officers.Officer, n int, kickoff string) error {
	if o.OnHost() {
		return f.hostStart(ctx, o, n, kickoff)
	}
	octx := f.s.officerPrincipal(ctx, o.Owner)
	name := o.InstanceName(n)
	if o.FreshSessionPerItem() {
		// A start may bring a newer image: whether its ccd starts new
		// conversations is learnt again from its first turn
		// (OfficerTurnContext). What its conversation claimed is NOT
		// forgotten here: only ccd's confirmation of a new one does that.
		f.s.officerRT.forgetStale(name)
	}
	a, err := f.s.ext.Agents.GetByName(octx, name)
	if err == nil && !a.Provisioned() {
		// A host row left from when the officer ran on a host: never handed to
		// the provisioner (which refuses it anyway) — say how to clear it.
		return fmt.Errorf("officers: %s: an agent row of that name is host %s's (from when %s ran there); remove it with `muster agent rm %s` "+
			"(or agent_destroy from one of your sessions) and %s starts in a pod at its next pass", name, a.RunsOnHost, o.ID, name, o.ID)
	}
	if err != nil {
		_, err := f.s.createAndDispatchAgent(octx, dispatchParams{
			Name:              name,
			DisplayName:       fmt.Sprintf("officer %s #%s", o.ID, o.InstanceLabel(n)),
			DisplayNameSource: agents.NameSystem,
			Kind:              agents.KindClaudeCode,
			CCAccount:         o.CCAccount,
			CCFailover:        o.CCFailover,
			NoteText:          kickoff,
			Kickoff:           true,
		})
		return err
	}
	re, ok := f.s.ext.Agents.(kickoffRearmer)
	if !ok {
		// Fail closed: an instance woken with no first turn would sit idle
		// while the loop counts it as working the queue.
		return errors.New("officers: the agents store cannot re-arm a first turn; not starting a promptless instance")
	}
	if err := re.RearmKickoff(octx, a.ID, kickoff); err != nil {
		return fmt.Errorf("officers: re-arm %s: %w", name, err)
	}
	return f.s.ext.Provisioner.Start(octx, a.ID)
}

// OfficerTurnContext is THE context a turn muster gives agent a is sent
// under: for an instance of a fresh_session_per_item pool
// (officers.Officer.FreshSessionPerItem), ctx routed with
// agents.HeaderCCNewConversation "1", on which the pod's ccd starts a NEW
// conversation (the old transcript kept) before it pastes the turn; for every
// other agent, ctx unchanged.
//
// Both of the officer runtime's turns ask it: the wake loop's prompt
// (Reprompt) and the kickoff deliverer's first turn (agentkickoff.Config.
// TurnContext, wired by cmd/muster-server) — so a pod started for an item, a
// pod restarted at the wall-time cap, and an idle instance prompted for its
// next item all begin that item in a new conversation.
//
// 🔴 THE CONVERSATION IS FREED ONLY WHEN ccd CONFIRMS IT. The context also
// asks for ccd's 103 (agents.WithTurnAccepted), sent once the CLI has taken
// the turn's text; ccd's names the new conversation the text went into
// (agents.TurnAcceptance.NewConversation). Only then is what the instance's
// conversation claimed forgotten (officerConversationAccepted) — never before
// the send, when a turn ccd then refused (409 busy: the old turn still
// running) would have let the OLD conversation claim again for one round
// trip, and never for a ccd that took the text without naming one.
// ⚠ The 103 is read on the transport's goroutine as it arrives; the
// instance's first claim needs a model request and a tool call after it, so
// the order is the CLI's, not a lock's.
func (s *Server) OfficerTurnContext(ctx context.Context, a agents.Agent) context.Context {
	o, _, ok := s.officerOf(a)
	if !ok || !o.FreshSessionPerItem() {
		return ctx
	}
	// Set, never a map literal: a literal key is not canonicalised ("CC"), and
	// Header.Get — applyTurnRoute's read — would never find it.
	h := http.Header{}
	h.Set(agents.HeaderCCNewConversation, "1")
	name := a.Name
	ctx = agents.WithTurnRoute(ctx, agents.TurnRoute{Header: h})
	return agents.WithTurnAccepted(ctx, func(acc agents.TurnAcceptance) { s.officerConversationAccepted(name, acc) })
}

// officerConversationAccepted is ccd's acceptance of a turn OfficerTurnContext
// asked a new conversation for, for instance name: a new conversation named
// frees the instance to claim one item in it; none named is a ccd that
// predates the header — the turn runs in the conversation the CLI already
// had, so nothing is freed, the operator is told, and Reprompt sends that
// instance nothing more until it is started again.
func (s *Server) officerConversationAccepted(name string, acc agents.TurnAcceptance) {
	if acc.NewConversation != "" {
		s.officerRT.conversationConfirmed(name)
		return
	}
	s.officerRT.markStale(name)
	s.logger.Printf("officers: %s: 🔴 its ccd took a fresh_session_per_item turn but did not confirm a new conversation (no %s on its answer): "+
		"it predates that header, and the turn runs in the conversation the CLI already had. Its claims stay refused after its current item, and muster "+
		"sends it no further turn until it is started again on a claude-code-agent image from this version", name, agents.HeaderCCNewConversationSession)
}

// promptPending is the kickoff deliverer's own "owed" rule
// (agentkickoff.owesFirstTurn): a note is set and not yet stamped delivered.
func promptPending(a agents.Agent) bool { return !a.KickedOff && a.PendingNote != "" }

// repromptClaimMargin is how much longer than the turn a re-prompt's session
// claim lives, as the memo relay's (claimMargin there).
const repromptClaimMargin = 2 * time.Minute

// repromptSlack is added to the kind's turn timeout for the send's deadline.
const repromptSlack = 2 * time.Minute

// Reprompt implements officers.Fleet: send up instance n text as a NEW turn of
// its own, under its session claim — the memo relay's pattern
// (internal/memorelay deliver), not the kickoff's.
//
// 🔴 IT TOUCHES NO KICKOFF STATE, AND IT USED TO. The first version re-armed
// the instance's first turn (RearmKickoff: kicked_off=false, a fresh
// updated_at). An instance works the queue INSIDE a turn that can run ~37
// minutes, and between items it holds no HQ claim, so the loop re-prompted it
// mid-turn; when that turn ended more than ProvisioningStuckTimeout after the
// re-arm, the kickoff deliverer's dwell check read the row as a first turn that
// never landed and marked it `error` — and an instance in error held its slot
// until an operator acted (TestARepromptDuringARunningFirstTurnNeverStrandsTheInstance;
// the bounded auto-restart now restarts such an instance, at most a few times
// a day, but a re-prompt must still never cause one).
// Now the row is never written: no stuck verdict can be derived from it.
//
//   - The session claim (taken with agents.Store.ClaimKickoffForDelivery, which
//     also loses while the owner's typed turn is in flight) is the one a first turn, a
//     memo and this hold for a whole turn. Lost = a turn is running:
//     officers.ErrInstanceBusy, nothing sent, and the loop tries next pass.
//   - A kickoff still owed (promptPending) is busy too: the first turn goes first.
//   - Won: the send runs in the background (a turn can outlast many passes),
//     holding the claim until it returns, then releases it; the turn is
//     recorded on the agent's session like any other.
func (f officerFleet) Reprompt(ctx context.Context, o officers.Officer, n int, text string) error {
	if o.OnHost() {
		return f.hostReprompt(ctx, o, n, text)
	}
	s := f.s
	if s.ext.Gateway == nil {
		return errors.New("officers: no agent gateway to send a re-prompt through")
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	name := o.InstanceName(n)
	a, err := s.ext.Agents.GetByName(octx, name)
	if err != nil {
		return err
	}
	if promptPending(a) {
		return officers.ErrInstanceBusy
	}
	turn := agents.KindTurnTimeout(a.Kind) + repromptSlack
	owner := s.officerRT.claimOwner
	won, err := s.ext.Agents.ClaimKickoffForDelivery(octx, a.ID, owner, turn+repromptClaimMargin)
	if err != nil {
		return fmt.Errorf("officers: take %s's session claim: %w", name, err)
	}
	if !won {
		return officers.ErrInstanceBusy
	}
	release := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(octx), 10*time.Second)
		defer cancel()
		if err := s.ext.Agents.ReleaseKickoffClaim(rctx, a.ID, owner); err != nil {
			s.logger.Printf("officers: %s: release the re-prompt's session claim: %v", name, err)
		}
	}
	// Re-read under the claim: a first turn may have been re-owed meanwhile.
	fresh, err := s.ext.Agents.Get(octx, a.ID)
	if err != nil || promptPending(fresh) {
		release()
		if err != nil {
			return err
		}
		return officers.ErrInstanceBusy
	}
	sess, err := s.ext.Agents.LatestOrCreateSession(octx, fresh.ID, fresh.Name)
	if err != nil {
		release()
		return fmt.Errorf("officers: %s's session: %w", name, err)
	}
	// A fresh pool's turn is a new conversation (OfficerTurnContext), and its
	// instance may claim again in it once ccd confirms one. An instance whose
	// ccd took such a turn WITHOUT confirming one is sent nothing more: the
	// turn would run in the conversation that already had its item.
	if o.FreshSessionPerItem() && s.officerRT.isStale(name) {
		release()
		return fmt.Errorf("officers: %s: its ccd does not start a new conversation per item (an image that predates %s); not prompting it until it is started again",
			name, agents.HeaderCCNewConversation)
	}
	// 🔴 THE SEND ENDS AT SHUTDOWN, AND RELEASES ITS CLAIM WHEN IT DOES. Its
	// context is cancelled by EndStreams — as a plugin delivery's is — and the
	// shutdown drain waits for it (WaitForOfficerReprompts). Detached from that,
	// a rollout during a re-prompt turn exited with the claim held for the rest
	// of the turn's budget, and a stop+start of the instance inside it ended in
	// error. The release runs on its own short, detached budget (release).
	sendCtx, endSend := s.streamContext(context.WithoutCancel(octx))
	s.officerRT.wg.Add(1)
	safeGo(s.logger, "officer re-prompt "+name, func() {
		defer s.officerRT.wg.Done()
		defer release()
		defer endSend()
		// Shutdown came first (EndStreams before this goroutine ran): send
		// nothing, record nothing; the claim is released on the way out. The
		// streams' own context is read too: streamContext cancels sendCtx
		// through context.AfterFunc, which runs asynchronously, so sendCtx may
		// not read cancelled yet when shutdown has already begun.
		if sendCtx.Err() != nil || s.streams.context().Err() != nil {
			return
		}
		tctx, cancel := context.WithTimeout(s.OfficerTurnContext(sendCtx, fresh), turn)
		defer cancel()
		reply, err := s.ext.Gateway.Chat(tctx, fresh, sess.SessionKey, text, nil)
		if unsent := agentgateway.Unsent(err); unsent != "" {
			// Refused before anything was typed (ccd's 409 busy, 503
			// not_ready): not a turn, so nothing goes on the transcript.
			s.logger.Printf("officers: %s: re-prompt not sent (%s); the next pass tries again", name, unsent)
			return
		}
		bctx, bcancel := context.WithTimeout(context.WithoutCancel(octx), 10*time.Second)
		defer bcancel()
		_, _ = s.ext.Agents.AddChatMessage(bctx, agents.ChatMessage{AgentID: fresh.ID, SessionID: sess.ID, Role: "user", Content: text})
		if reply != "" {
			_, _ = s.ext.Agents.AddChatMessage(bctx, agents.ChatMessage{AgentID: fresh.ID, SessionID: sess.ID, Role: "assistant", Content: reply})
		}
		if err != nil {
			s.logger.Printf("officers: %s: re-prompt turn: %s", name, failureKind(err))
		}
	})
	return nil
}

// failureKind names a send failure for the log without quoting it (a runtime's
// error can echo the request), as the memo relay does.
func failureKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || agents.IsClientTimeout(err) {
		return " (the turn outlasted its deadline)"
	}
	return " (failed; see the agent's transcript)"
}

// Stop implements officers.Fleet: scale instance n to zero (agent and volume
// kept). A claim it still holds is released by the loop's next pass, which
// sees its holder down.
//
// Its Claude usage is pushed to its backend first (usage.Collector.Flush;
// only when its backend serves usage: HQ, and the built-in backend, which
// books it into officer_llm_usage), while its ccd can still answer: the
// collector reads only up instances, so without it the hour the instance
// stopped in would sit partial until it next runs. Best-effort and bounded (usage.FlushTimeout): the stop goes ahead
// whatever the push does.
func (f officerFleet) Stop(ctx context.Context, o officers.Officer, n int) error {
	if o.OnHost() {
		return f.hostStop(ctx, o, n)
	}
	octx := f.s.officerPrincipal(ctx, o.Owner)
	a, err := f.s.ext.Agents.GetByName(octx, o.InstanceName(n))
	if err != nil {
		return err
	}
	if rt := f.s.officerRT; rt != nil && rt.usage != nil && officers.DeclaredCapabilities(o).Has(officers.CapUsage) {
		rt.usage.Flush(ctx, o, a.Name)
	}
	f.s.clearAgentWork(a.ID)
	return f.s.ext.Provisioner.Stop(octx, a.ID)
}

// OfficerPoolsReporter is the declared officers' pools push
// (officers.PoolsReporter), nil before EnableOfficers. It is NOT started
// here: cmd/muster-server runs it with the other background loops, and its
// first push is at once (boot).
func (s *Server) OfficerPoolsReporter() *officers.PoolsReporter {
	if s.officerRT == nil {
		return nil
	}
	return s.officerRT.pools
}

// WaitForOfficerReprompts blocks until every in-flight re-prompt send has
// ended and released its session claim. Called by the shutdown drain after
// EndStreams (which cancels them), and by tests.
func (s *Server) WaitForOfficerReprompts() {
	if s.officerRT != nil {
		s.officerRT.wg.Wait()
		if s.officerRT.chats != nil {
			// The chat turns too: each ends with the streams, and releases
			// (or completes) its item on the way out.
			s.officerRT.chats.Wait()
		}
	}
}

// OfficerClaudeCodeSession is the provisioner's session source
// (agentprovision.Adapter.UseClaudeCodeSession): for a declared officer's
// instance, its session settings; every other agent: ok=false, unchanged.
//
//   - BypassPermissions: true. 🔴 AN OFFICER INSTANCE RUNS WITH EVERY TOOL
//     ALLOWED — no permission prompt at all (operator decision, PR #337 round
//     1). An officer deployment may have no approval gate anyone watches, and
//     an officer reads its repository with Bash (git, curl) through muster's
//     proxy; a prompt there stalls the instance until its turn times out. The
//     pod's ccd seed starts every session in bypassPermissions
//     (podagent.EnvPermissionMode). What bounds an officer is what it can
//     REACH, not what it may run: its token (an officer-only MCP toolset, a
//     `github` grant the operator decides explicitly, minted read-only —
//     callerOf), and its pod's network policy.
//   - AllowMCPTools: the officer tools it is offered, in registry order, then —
//     when the connector store is configured (so the instance is offered
//     them) — connectors_list, connector_request, connector_status, and
//     postgres_query and internal_http_request when the officer declares
//     `connectors`. Redundant
//     under bypass, and kept: it names exactly the muster tools the instance
//     is for, and holds if bypass is ever lifted.
//   - Model: the officer's configured model (its pool's, for a tiered officer).
//   - LLMRoute, with no Model: an instance of a pool on the LLM proxy — a
//     routine instance beside `llm`, a judgement one with tiers.judgement.llm
//     (below).
//   - DisableAutoMemory: every officer instance (below).
func (s *Server) OfficerClaudeCodeSession(a agents.Agent) (agentspec.ClaudeCodeSession, bool) {
	o, _, ok := s.officerOf(a)
	if !ok {
		return agentspec.ClaudeCodeSession{}, false
	}
	var allow []string
	for _, d := range officerToolDefs() {
		if s.officerToolServed(o, d.Name) {
			allow = append(allow, d.Name)
		}
	}
	if s.ext.Connectors != nil {
		for _, d := range connectorToolDefs() {
			// postgres_query and internal_http_request only for an officer
			// that declares a connector they could serve; every officer still
			// reaches github.
			if (d.Name == postgresQueryToolName || d.Name == internalHTTPToolName) && len(o.Connectors) == 0 {
				continue
			}
			allow = append(allow, d.Name)
		}
	}
	// 🔴 AN INSTANCE OF A POOL ON THE PROXY IS ON THE LLM ROUTE, AND ONLY IT
	// (officers.ProxyAgent, the proxy's own membership rule): a routine
	// instance beside the officer's `llm`, or a judgement instance with
	// tiers.judgement.llm. Its pod
	// reaches its model through muster's proxy with its own muster token and
	// carries no subscription token. It takes no session model: the proxy
	// pins its pool's model, and the pod spec refuses a provider id. Its
	// network policy is every officer instance's (egress-locked, from
	// BypassPermissions): the route changes no rule.
	//
	// 🔴 EVERY OFFICER INSTANCE RUNS WITH THE CLI'S AUTO-MEMORY OFF
	// (DisableAutoMemory; operator decision 2026-11-06), whatever its pool and
	// whatever fresh_session_per_item says: what an officer remembers of
	// earlier items is HQ's (the claim-time history), never a file on its
	// volume.
	// Image: the pool's own runner image when it declares one (checked against
	// MUSTER_OFFICER_IMAGE_REGISTRIES at boot), else the deployment's.
	// ImagePullSecret: MUSTER_OFFICER_IMAGE_PULL_SECRET, passed for every
	// instance; agentspec places it in the spec only beside Image
	// (ccImagePullSecret), so a pool on the deployment's image never gets it.
	sess := agentspec.ClaudeCodeSession{AllowMCPTools: allow, BypassPermissions: true, DisableAutoMemory: true, Image: o.Image,
		ImagePullSecret: s.officerRT.imagePullSecret}
	// MaxSessions: the officer's declared chat_sessions (the pod's ccd lanes,
	// CCD_MAX_SESSIONS), for the pool that runs its chats; 0 — none declared —
	// leaves the pod's spec, and ccd's own default, as they were.
	if o.ChatsPool() {
		sess.MaxSessions = o.DeclaredChatSessions()
	}
	if _, _, proxied := officers.ProxyAgent(o, a.Name); proxied {
		sess.LLMRoute = true
		return sess, true
	}
	sess.Model = o.Model
	return sess, true
}

// --- delete protection -------------------------------------------------------

// deleteProtection is THE delete-protection predicate, bound to one read of
// the agent list: the chief (chiefPredicate, unchanged) OR an instance of a
// declared officer (officerOf). Every destroy path asks it through
// agentIsDeleteProtected; the card menu reads it to hide Delete.
//
// ⚠ IT IS NOT THE CHIEF PREDICATE AND MUST NOT BE FOLDED INTO IT. chiefPredicate
// also decides who is offered the chief-only tools; an officer instance must
// be undeletable without becoming the chief.
//
// An officer removed from MUSTER_OFFICERS stops protecting its instances, so
// the operator can delete them once the officer is gone. One still declared
// is protected absolutely, like the chief's reserved slug: the wake loop would
// only recreate it.
func (s *Server) deleteProtection(list []agents.Agent) func(agents.Agent) bool {
	chief := chiefPredicate(list)
	return func(a agents.Agent) bool {
		if chief(a) {
			return true
		}
		return s.isDeclaredInstanceRow(a)
	}
}

// isDeclaredInstanceRow reports whether a is a declared officer's instance row
// FOR ITS CURRENT RUNTIME: a pod officer's provisioned row, or a host
// officer's row on its declared host. Only such a row is delete-protected; a
// row left from before the officer moved (a pod row of a host officer, a host
// row of a pod officer or of another host) is not, so it can be removed
// (destroyAgent's host-row branch) and the officer can run where it now is.
func (s *Server) isDeclaredInstanceRow(a agents.Agent) bool {
	_, ok := s.officerOfRow(a)
	return ok
}

// officerOfRow is the declared officer a is a CURRENT instance row of
// (isDeclaredInstanceRow's rule: officerOf, and the row is its officer's
// current runtime's), ok false for every other agent. The delete protection
// and the sidebar's Officers group (AgentCardView.Officer, officerKeyOf) read
// it, so the group never claims a row the server would let be deleted as an
// ordinary agent, nor the reverse.
func (s *Server) officerOfRow(a agents.Agent) (officers.Officer, bool) {
	o, _, ok := s.officerOf(a)
	if !ok {
		return officers.Officer{}, false
	}
	if o.OnHost() {
		return o, a.RunsOnHost == o.Host
	}
	return o, a.Provisioned()
}

// officerKeyOf is AgentCardView.Officer for a: its officer's registry key
// (officers.Officer.Key) when officerOfRow binds it, else "".
func (s *Server) officerKeyOf(a agents.Agent) string {
	if o, ok := s.officerOfRow(a); ok {
		return o.Key()
	}
	return ""
}

// agentIsDeleteProtected reads the agent table and answers deleteProtection
// for id, returning the row. It FAILS CLOSED like agentIsChief: a read error
// is returned and every destroy path refuses on it.
func (s *Server) agentIsDeleteProtected(ctx context.Context, id int64) (agents.Agent, bool, error) {
	if s.ext.Agents == nil {
		return agents.Agent{}, false, nil
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return agents.Agent{}, false, err
	}
	for _, a := range list {
		if a.ID == id {
			return a, s.deleteProtection(list)(a), nil
		}
	}
	return agents.Agent{}, false, nil
}

// protectedRefusal is the text a refused delete of a protected agent carries.
func (s *Server) protectedRefusal(a agents.Agent) string {
	if o, _, ok := s.officerOf(a); ok {
		return "this agent is an instance of officer " + o.ID + " and cannot be deleted while the officer is declared in MUSTER_OFFICERS; " +
			"it is scaled to zero when idle (remove the officer from MUSTER_OFFICERS first if it is really meant to go)"
	}
	return chiefRefusal(a)
}

// --- the MCP tools -----------------------------------------------------------

// officerToolDefs are the tools an officer instance is offered. None takes an
// officer: each acts for the caller's own (officerOf).
func officerToolDefs() []agents.ToolDef {
	defs := append(append(append(queueToolDefs(), shipToolDefs()...), kubeReadToolDef(), obsRawToolDef()), taskReadToolDefs()...)
	defs = append(append(append(defs, itemAskToolDefs()...), prContextToolDef(), gapTaskToolDef(), rolloutEvidenceToolDef(), postDraftToolDef()), contactToolDefs()...)
	return append(defs, ownStateToolDefs()...)
}

// queueToolDefs are every officer's queue tools and obs tools (and the
// release officer's captain tool).
func queueToolDefs() []agents.ToolDef {
	item := strProp("The item's id, as officer_items or officer_brief returned it.")
	lease := intProp(fmt.Sprintf("How long the lease runs, in seconds (default %d, %d to %d).", officers.DefaultLeaseSeconds, officers.MinLeaseSeconds, officers.MaxLeaseSeconds),
		officers.MinLeaseSeconds, officers.MaxLeaseSeconds)
	return []agents.ToolDef{
		{Type: "function", Name: officers.ToolBrief,
			Description: "Load your officer's brief from HQ: your charter (your standing instructions, versioned), your open items, recent outcomes and your memory scopes. Call it first when you wake, instead of looking for an earlier conversation.",
			Parameters:  objSchema(map[string]any{})},
		{Type: "function", Name: officers.ToolItems,
			Description: "List your officer's items in HQ. status: open (default: what you can claim), claimed, waiting (an ask on it is open: see officer_ask_person), done or escalated.",
			Parameters: objSchema(map[string]any{
				"status": map[string]any{"type": "string", "enum": officers.ItemStatuses},
				"limit":  intProp("At most this many (default HQ's).", 1, 100),
				"cursor": strProp("The `next` value of the previous page (HQ returns it with each page; null on the last)."),
			})},
		{Type: "function", Name: officers.ToolClaim,
			Description: "Claim one item so no other instance works it. With NO item_id, HQ picks the next item for you (highest priority first, then the earliest due, then the oldest; never a blocked one) and the answer is that item — or item null when nothing is claimable: stop and wait. " +
				"Give an item_id only when your charter says to claim a particular item. If YOU already hold it (e.g. you were restarted mid-item), this succeeds, renews your lease and says \"already yours\" — carry on with it. A conflict answer (refused, status 409) means ANOTHER instance holds it: pick another item. Heartbeat before the lease runs out.",
			Parameters: objSchema(map[string]any{"item_id": strProp("Optional: the item's id, as officer_items or officer_brief returned it. Omit it to take the next item."), "lease_seconds": lease})},
		{Type: "function", Name: officers.ToolHeartbeat,
			Description: "Extend your lease on an item you hold. A conflict answer means you lost the lease: stop working the item.",
			Parameters:  objSchema(map[string]any{"item_id": item, "lease_seconds": lease}, "item_id")},
		completeToolDef(officers.Officer{}),
		{Type: "function", Name: officers.ToolRelease,
			Description: "Give an item back before its lease ends, so another instance can take it.",
			Parameters:  objSchema(map[string]any{"item_id": item}, "item_id")},
		{Type: "function", Name: officers.ToolAskPerson,
			Description: "Ask a named HQ member a question about an item you hold, when you need a human's decision to finish it (for example a design review with screenshots). " +
				"HQ notifies them. 🔴 Asking ENDS your lease on the item: HQ sets it to waiting with no holder, so stop working it — " +
				"do not heartbeat, complete or release it. HQ re-opens it (claimable) with the answers on it once every open ask on it is answered or withdrawn, " +
				"and muster wakes an instance for it; if the deadline passes unanswered, HQ escalates the item to its owner. " +
				"A second ask on a waiting item is allowed (the same item_id).",
			Parameters: objSchema(map[string]any{
				"item_id":  item,
				"to":       strProp("The HQ member's GitHub login."),
				"question": strProp("The question. Say what you need decided and why."),
				"options": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Optional answers to pick from: short, unique labels."},
				"image_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Optional images, as the ids `ccd ask-image <path>` prints: run it in your shell on a PNG, JPEG or WebP file in your workspace " +
						"(at most 5 MiB) and pass the \"id\" it prints. For an image on the web, download it into your workspace first. The ask cites them in order."},
				"context_url":      strProp("Optional http(s) URL with the context (a pull request, a preview, a dashboard)."),
				"deadline_seconds": map[string]any{"type": "integer", "description": "Optional: how long they have to answer, in seconds (HQ's default is a day; HQ refuses a value outside its range)."},
			}, "item_id", "to", "question")},
		{Type: "function", Name: officers.ToolReleaseEvaluation,
			Description: "Post to HQ's release captain route (POST /releases/captain): your risk evaluation of a release and/or your post-release report. " +
				"service and tag come from the item's payload. Give evaluation (level low|medium|high|severe, summary, optional monitors) for a release.evaluate item, " +
				"report (text) for a release.post_report item, or both. muster adds your instance; HQ records this officer and instance as the author. " +
				"Then finish the item with officer_complete. This records advice; it does not approve, deploy or release anything. " +
				"A refusal with status 409 and code officer_shadow means you are in SHADOW mode: do not retry — put your full evaluation " +
				"(or report) in officer_complete's summary instead.",
			Parameters: objSchema(map[string]any{
				"service": strProp("The release's service, from the item's payload."),
				"tag":     strProp("The release's tag, from the item's payload."),
				"evaluation": objSchema(map[string]any{
					"level":   map[string]any{"type": "string", "enum": officers.CaptainLevels, "description": "The release's risk. severe means only an admin can approve it."},
					"summary": strProp(fmt.Sprintf("Why that level: what changed, what could break (1-%d characters).", officers.MaxCaptainSummary)),
					"monitors": map[string]any{"type": "array", "maxItems": officers.MaxCaptainMonitors,
						"description": "What to watch after rollout.",
						"items": objSchema(map[string]any{
							"name": strProp("What it is, e.g. \"Billing 5xx\"."),
							"url":  strProp("An absolute http(s) URL (a dashboard, a query)."),
						}, "name", "url")},
				}, "level", "summary"),
				"report": strProp(fmt.Sprintf("The post-release report, as text (1-%d characters).", officers.MaxCaptainReport)),
			}, "service", "tag")},
		{Type: "function", Name: officers.ToolObsQueries,
			Description: "List the observability queries HQ lets your officer run (GET /officers/{id}/obs/queries): each query's id, what its answer may be used to claim, " +
				"the arguments it takes (`args`), and HQ's caps (window, series, lines, queries per minute). Call it before " + officers.ToolObsQuery + ". Read-only.",
			Parameters: objSchema(map[string]any{})},
		{Type: "function", Name: officers.ToolObsQuery,
			Description: "Run ONE of HQ's named observability queries (POST /officers/{id}/obs/query) over a window, with the arguments that query takes — " +
				"the `args` " + officers.ToolObsQueries + " lists for it, and no others. There is no raw PromQL or LogQL: HQ builds the query from its reviewed registry. " +
				"The answer says what it `claims`; quote only that. `empty: true` is NOT a zero: run the positive control its `hint` names before you report none. " +
				"Log lines come back redacted, a sample, never a count. HQ allows a few dozen queries a minute per officer (a 429 means wait a minute); " +
				"an error carrying `retryable` (502 upstream: the backend read failed) may be retried in a minute; any other refusal names the argument or limit to fix and the same call will not succeed. " +
				"On a judgement instance each call is one read of the item you hold (item_id picks it when you hold several; muster removes it before HQ sees the call). Read budget: " +
				officers.ReadBudgetNote + ". Read-only.",
			// 🔴 AN OPEN OBJECT DESCRIBING query_id ONLY: HQ's decoder is the
			// one schema (officers.ObsQueryArgs), and the arguments each query
			// takes are the `args` officer_obs_queries lists — a closed schema
			// repeating them here would drift from HQ's and hide a new one.
			Parameters: map[string]any{"type": "object", "additionalProperties": true, "required": []string{"query_id"},
				"properties": map[string]any{
					"query_id": strProp("The query's id, as " + officers.ToolObsQueries + " lists it. Send with it exactly the arguments that query's `args` list " +
						"(start and end are RFC 3339 in whole seconds, e.g. 2006-01-02T15:04:05Z; the window is (start, end]); HQ checks every one."),
				}}},
	}
}

// completeToolDef is officer_complete's definition as officer o's instances
// are offered it (officerToolDefFor): its outcomes are officers.
// CompleteOutcomes(o) — needs_judgement for a routine pool only — and it
// takes `investigation` when officers.CompleteTakesInvestigation(o). The
// registry's copy is an untiered officer's.
func completeToolDef(o officers.Officer) agents.ToolDef {
	desc := "Finish an item you hold: outcome done, escalated (a human must act; say who and why in the summary) or no_action"
	if slices.Contains(officers.CompleteOutcomes(o), officers.OutcomeNeedsJudgement) {
		desc += ", or " + officers.OutcomeNeedsJudgement + " (hand a routine item to the judgement pool: HQ makes it a judgement item, open again with your summary kept for " +
			"the judgement instance that takes it, and your hold ends; HQ keeps no links on it — put them in the summary)"
	}
	desc += ", with a summary (at most 24,576 characters — in shadow mode your full evaluation or report goes here) and optional links."
	props := map[string]any{
		"item_id": strProp("The item's id, as officer_items or officer_brief returned it."),
		"outcome": map[string]any{"type": "string", "enum": officers.CompleteOutcomes(o)},
		"summary": strProp("What you found and did, at most 24,576 characters."),
		"links":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": officers.MaxLinks, "description": "Optional http(s) URLs (a PR, a dashboard, a report), at most 10."},
	}
	// Every officer is offered it: HQ decides which items take a hand-off.
	desc += " With outcome done, `handoff: {head_sha}` hands the item's pull request on at that head (the review loop's next round); HQ says when it is required."
	handoffProps := map[string]any{"head_sha": strProp("40 lower-case hex.")}
	handoffDesc := "Optional, outcome done only: {head_sha: the pull request's head you hand on, 40 lower-case hex}. HQ requires it on some loop items " +
		"(400 handoff_required) and refuses it where it does not fit; a refused complete leaves the item held."
	if o.Ship.IsShepherd() {
		// A shepherd's only: completeHandoff refuses `to` from any other.
		handoffProps["to"] = map[string]any{"type": "string", "enum": []string{officers.HandoffToAuthor},
			"description": "\"author\": hand a clean round's fixes to the pull request's authoring officer (HQ checks the ladder stopped clean at head_sha)."}
		handoffDesc += " Add to: \"author\" to hand a clean round's 🟡/🟢 fixes to the pull request's authoring officer."
	}
	props["handoff"] = map[string]any{"type": "object", "additionalProperties": false, "description": handoffDesc,
		"properties": handoffProps, "required": []string{"head_sha"}}
	// 🔴 OFFERED TO EVERY POOL OF AN HQ OFFICER, AND OPEN: HQ decides which
	// items and outcomes take each, and muster forwards each to HQ as sent
	// (officers.Completion.AuthorsJSON and siblings) — a closed schema here
	// would be a second copy that drifts from HQ's decoder. HQ's refusal is a
	// 400 and the item stays held; it names the field only when HQ knows the
	// field. An HQ that predates one (its decoder disallows unknown fields)
	// answers a generic `body must be one JSON object {…}` naming none — so
	// until HQ accepts `tickets`, a complete carrying it is refused, the item
	// stays held, and the same complete without it succeeds.
	// A builtin officer is not offered them (completeTakesHQFields): its
	// backend has no author mail, attribution or tickets.
	if completeTakesHQFields(o) {
		desc += " When your item's payload names the people whose work it is about (payload.authors) or HQ's charter asks for them: " +
			"`authors` writes each author a mail, `attributed_prs` + `attribution` tell the authors of the PRs an escalation is attributed to, " +
			"and `tickets` links support tickets to PRs. Send only what applies; muster passes each to HQ as you send it, and HQ checks it."
		props["authors"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
			"description": "Optional, outcome done or escalated: {\"<GitHub login>\": \"the text HQ mails that author\"}, each login one of " +
				"your item's payload.authors. HQ mails each author only their own text. HQ validates it and may refuse with 400; a refused complete leaves the item held."}
		props["attributed_prs"] = map[string]any{"type": "array", "items": map[string]any{"type": "integer"},
			"description": "Optional, outcome escalated: the PR numbers your finding is attributed to, from your item's payload.authors PRs. " +
				"HQ sends each attributed PR's author the escalation card. HQ validates it and may refuse with 400."}
		props["attribution"] = map[string]any{"type": "string",
			"description": "Optional, with attributed_prs: \"likely\" or \"confirmed\". HQ validates it and may refuse with 400."}
		props["tickets"] = map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": true},
			"description": "Optional, a release-ticket item (your charter says when): the support tickets you link to a PR, " +
				"[{ticket_id, pr, attribution: \"likely\"|\"confirmed\", gist: your own short paraphrase, never a quote of the ticket}], " +
				"each ticket_id one of your item's payload tickets. HQ validates it and may refuse with 400."}
	}
	if officers.CompleteTakesInvestigation(o) {
		desc += " Escalating an item of an officer type that investigates needs `investigation`: HQ refuses the escalation without it, and its refusal names what to fix."
		// 🔴 AN OPEN OBJECT, as officer_obs_query's arguments: HQ's decoder
		// is the one schema for the block, and muster forwards it to HQ as
		// sent (officers.Completion.InvestigationJSON) — a closed schema here
		// would be a second copy that drifts from HQ's.
		props["investigation"] = map[string]any{"type": "object", "additionalProperties": true,
			"description": "Your investigation, for an escalation (HQ keeps it on outcome escalated only): {hypotheses: [text], " +
				"eliminated: [{hypothesis, evidence, audit_ids: [the audit_id of each read the evidence came from]}], root_cause, " +
				"confidence: low|medium|high, fix: {pr_urls: [your draft pull requests]} OR {operator_steps: [text]} (exactly one; low confidence gives operator_steps), " +
				"not_checked: [text]}. Every list present, empty when there is nothing. muster passes it to HQ as you send it; HQ checks every field."}
	}
	return agents.ToolDef{Type: "function", Name: officers.ToolComplete, Description: desc,
		Parameters: objSchema(props, "item_id", "outcome", "summary")}
}

// completeTakesHQFields is THE rule for whether officer o's instances are
// offered officer_complete's authors, attributed_prs, attribution and
// tickets (the features only HQ serves), and may send them: an HQ officer's only. The
// builtin backend has none of those features, and would complete the item
// while dropping them.
func completeTakesHQFields(o officers.Officer) bool { return !o.IsBuiltin() }

// completeHandoff checks officer_complete's `handoff` (raw as sent; absent or
// null is none): {"head_sha": <40 lower-case hex>}, plus — a shepherd's only
// (shepherd) — "to": "author"; and only on outcome done (HQ refuses it on any
// other). It returns the hand-off to send, or why it is refused — before
// anything reaches HQ.
func completeHandoff(raw json.RawMessage, outcome string, shepherd bool) (*officers.Handoff, string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var h struct {
		HeadSHA *string `json:"head_sha"`
		To      *string `json:"to"`
	}
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Sprintf("handoff must be exactly {\"head_sha\": <40 lower-case hex>}: %v", err)
	}
	if h.HeadSHA == nil || !officers.ValidHandoffHead(*h.HeadSHA) {
		return nil, "handoff.head_sha must be the full commit sha, 40 lower-case hex"
	}
	out := &officers.Handoff{HeadSHA: *h.HeadSHA}
	if h.To != nil {
		if !shepherd {
			return nil, "handoff.to is a shepherd's only (it hands a clean round's fixes to the pull request's author)"
		}
		if *h.To != officers.HandoffToAuthor {
			return nil, fmt.Sprintf("handoff.to must be %q", officers.HandoffToAuthor)
		}
		out.To = *h.To
	}
	if outcome != "done" {
		return nil, fmt.Sprintf("handoff goes with outcome done only (HQ refuses it on %s)", outcome)
	}
	return out, ""
}

// officerToolDefFor is tool definition d as officer o's instances are
// offered it: officer_complete's per-pool definition, every other tool as
// the registry declares it.
func officerToolDefFor(o officers.Officer, d agents.ToolDef) agents.ToolDef {
	if o.IsBuiltin() {
		// muster's own queue, worded as such (officer_builtin_tools.go).
		return builtinToolDef(o, d)
	}
	if d.Name == officers.ToolComplete {
		return completeToolDef(o)
	}
	return d
}

// isOfficerTool reports whether name is one of officerToolDefs.
func isOfficerTool(name string) bool {
	for _, d := range officerToolDefs() {
		if d.Name == name {
			return true
		}
	}
	return false
}

// dispatchOfficerTool runs an officer tool for agent a, reporting whether name
// was one.
func (s *Server) dispatchOfficerTool(ctx context.Context, a agents.Agent, name, arguments string) (string, bool) {
	if !isOfficerTool(name) {
		return "", false
	}
	o, n, ok := s.officerOf(a)
	if !ok {
		return toolErr("%s is available to officer instances only", name), true
	}
	if !s.officerToolServed(o, name) {
		return toolErr("%s is not one of officer %s's tools", name, o.ID), true
	}
	// 🔴 EVERYTHING AN OFFICER TOOL REACHES IS AN OFFICER'S CALL: a decision
	// door refuses it whatever principal it runs under (officerDeciderRefusal,
	// officer_own_state.go).
	ctx = withOfficerCall(ctx, o)
	// 🔴 THE RUNTIME BINDS THE DOOR (officer_host.go): a host officer's tools
	// are its host instances' (no agent row), a pod officer's its pods'.
	if why := officerRuntimeRefusal(o, a, name); why != "" {
		return toolErr("%s", why), true
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &raw); err != nil {
		return toolErr("arguments must be a JSON object"), true
	}
	// 🔴 NAMING ANOTHER OFFICER IS REFUSED, NOT IGNORED. No schema offers an
	// officer argument, but a runtime can send any keys; silently acting for
	// the caller's own officer would hide an attempt to act for another.
	// The key is matched case-insensitively, as HQ's Go decoder matches field
	// names: `Officer_ID` names an officer as surely as `officer_id`.
	for k, v := range raw {
		if !strings.EqualFold(k, "officer") && !strings.EqualFold(k, "officer_id") {
			continue
		}
		var named string
		_ = json.Unmarshal(v, &named)
		if named != o.ID {
			return toolErr("you act for officer %s only; %s cannot name another officer", o.ID, name), true
		}
	}
	// 🔴 A CHAT LANE'S CALL (officer_chat.go): it acts as its chat claim's
	// holder (officerCallerName), so its reads are its chat item's and its
	// item verbs reach that item only; and it takes no queue work.
	caller := officerCallerName(ctx, a)
	if chat, lane := chatLaneCall(ctx); lane {
		switch name {
		case officers.ToolClaim, officers.ToolHeartbeat, officers.ToolComplete, officers.ToolRelease,
			// these end the holder's lease: the chat's item is the runner's
			officers.ToolAskPerson, officers.ToolTaskAsk, officers.ToolFloodBlockPropose,
			// rollout evidence is the sweep's: HQ shows the NEWEST for a tag to
			// the person starting a rollout, so a chat's attach would replace it
			officers.ToolRolloutEvidenceAttach,
			// a ship action is bound to the item the primary holds, or writes
			// to GitHub on the officer's behalf: never a chat session's
			officers.ToolMergePR, officers.ToolTagRelease, officers.ToolMarkReady, officers.ToolShepherdReady,
			officers.ToolShepherdMergeAsk, officers.ToolPublicBranch:
			return chatLaneQueueRefusal(name, chat), true
		}
	} else if name == officers.ToolClaim && o.ChatOnly(n) {
		// 🔴 A CHAT-ONLY INSTANCE'S PRIMARY TAKES NO QUEUE WORK: it is numbered
		// past max_instances, which keeps bounding the queue; muster started
		// it for chats (officers.Officer.ChatOnly).
		return toolErr("%s is refused: instance %s serves chat sessions only (it is past max_instances %d, which bounds queue work); end your turn",
			name, o.InstanceLabel(n), o.MaxInstances), true
	}
	if isShipTool(name) {
		return s.dispatchShipTool(ctx, a, o, name, arguments), true
	}
	c := s.officerRT.hq.Backend(o)
	switch name {
	case officers.ToolKubeRead:
		return s.officerKubeRead(ctx, a, o, c, arguments), true
	case officers.ToolObsRaw:
		return s.officerObsRaw(ctx, a, o, c, arguments), true
	case officers.ToolTaskList, officers.ToolTaskGet:
		return officerTaskRead(ctx, c, name, arguments), true // officer_tasks.go
	case officers.ToolTaskAsk, officers.ToolFloodBlockPropose:
		return s.officerItemAsk(ctx, c, o, caller, name, arguments), true // officer_item_asks.go
	case officers.ToolRolloutEvidenceAttach:
		return officerRolloutEvidence(ctx, c, caller, arguments), true // officer_rollout_evidence.go
	case officers.ToolPRContext:
		return officerPRContext(ctx, c, arguments), true // officer_pr_context.go
	case officers.ToolGapTask:
		return officerGapTask(ctx, c, caller, arguments), true // officer_gap_tasks.go
	case officers.ToolPostDraft:
		return s.officerPostDraft(ctx, a, o, c, arguments), true // plugin_outbound.go
	case officers.ToolContactGet, officers.ToolContactsList, officers.ToolContactNote:
		return s.officerContactTool(ctx, a, o, name, arguments), true // officer_contacts.go
	case officers.ToolMyState, officers.ToolConfirmCard, officers.ToolRefusalRefile, officers.ToolRefusalDrop:
		return s.officerOwnStateTool(ctx, a, o, name, arguments), true // officer_own_state.go
	}
	// The obs tools pass their arguments to HQ unchanged (officers.
	// ObsQueryArgs) — but for a judgement instance's item_id, which is
	// muster's: they are not decoded into the queue tools' struct below,
	// whose field types would otherwise be a second, local schema.
	switch name {
	case officers.ToolObsQueries:
		ob, serr := officers.Serves[officers.ObsBackend](c, officers.CapObs)
		if serr != nil {
			return notServedResult(name, serr), true
		}
		body, err := ob.ObsQueries(ctx)
		return officerToolResult(name, body, err), true
	case officers.ToolObsQuery:
		ob, serr := officers.Serves[officers.ObsBackend](c, officers.CapObs)
		if serr != nil {
			return notServedResult(name, serr), true
		}
		args, perr := officers.ObsQueryArgs([]byte(orEmptyJSON(arguments)))
		if perr != nil {
			return toolErr("%v", perr), true
		}
		if officers.CountsRead(o, name) {
			// a judgement instance's named query is one of its item's reads
			// (officer_reads.go): item_id, muster's and never HQ's, is split
			// off, and the read is charged to the item it holds — none
			// held, none charged
			rest, named, present, err := splitItemID(args)
			if err != nil {
				return toolErr("%v", err), true
			}
			if present {
				args = rest
			}
			item, err := s.heldItem(ctx, c, caller, named, false)
			if err != nil {
				return heldItemRefusal(name, err), true
			}
			if item != "" {
				// reserved now, given back by settleRead when HQ's answer
				// does not count (officers.ReadCounted)
				if err := s.spendRead(o, item); err != nil {
					return toolErr("%v", err), true
				}
				body, err := ob.ObsQuery(ctx, args)
				s.settleRead(o, item, err)
				return officerToolResult(name, body, err), true
			}
		}
		body, err := ob.ObsQuery(ctx, args)
		return officerToolResult(name, body, err), true
	}
	instance := caller
	var in struct {
		ItemID       officers.ItemID `json:"item_id"`
		LeaseSeconds int             `json:"lease_seconds"`
		Status       string          `json:"status"`
		Limit        int             `json:"limit"`
		Cursor       string          `json:"cursor"`
		Outcome      string          `json:"outcome"`
		Summary      string          `json:"summary"`
		Links        []string        `json:"links"`
		// officer_complete's investigation block, kept as sent: HQ's decoder
		// is its one schema (officers.Completion.InvestigationJSON).
		Investigation json.RawMessage `json:"investigation"`
		// officer_complete's author texts, attributed PRs, attribution and
		// tickets, each kept as sent and forwarded unchanged: HQ's decoder is
		// their one schema (officers.Completion.AuthorsJSON and siblings). A
		// key the instance did not send stays empty and reaches HQ as no key.
		Authors       json.RawMessage `json:"authors"`
		AttributedPRs json.RawMessage `json:"attributed_prs"`
		Attribution   json.RawMessage `json:"attribution"`
		Tickets       json.RawMessage `json:"tickets"`
		// officer_complete's hand-off, checked by completeHandoff.
		Handoff json.RawMessage `json:"handoff"`
		// officer_ask_person's.
		To              string          `json:"to"`
		Question        string          `json:"question"`
		Options         []string        `json:"options"`
		ImageIDs        []string        `json:"image_ids"`
		ContextURL      string          `json:"context_url"`
		DeadlineSeconds json.RawMessage `json:"deadline_seconds"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("invalid arguments for %s: %v", name, err), true
	}
	needItem := func() string {
		if strings.TrimSpace(string(in.ItemID)) == "" || strings.ContainsAny(string(in.ItemID), "/?#") {
			return toolErr("item_id is required (an id officer_items returned)")
		}
		return ""
	}
	var body []byte
	var err error
	switch name {
	case officers.ToolBrief:
		if g, ok := c.(officers.CharterGated); ok {
			// recorded as briefed with the version this answer carries
			body, err = g.BriefFor(ctx, instance)
		} else {
			body, err = c.Brief(ctx)
		}
	case officers.ToolItems:
		status := in.Status
		if status == "" {
			status = officers.StatusOpen
		}
		var page officers.ItemsPage
		// A tiered instance lists its own tier's items only (o.Tier is its
		// pool's; "" — an untiered officer — lists every tier, as before).
		page, err = c.Items(ctx, officers.ItemsQuery{Status: status, Limit: in.Limit, Cursor: in.Cursor, Tier: o.Tier})
		body = page.Raw
	case officers.ToolClaim:
		// 🔴 NO CLAIM ON A STALE CHARTER (charterRefusal), first: nothing is
		// reserved, marked or sent before it.
		if refusal := charterRefusal(ctx, c, name, instance); refusal != "" {
			return refusal, true
		}
		if o.ItemCap() > 0 || o.FreshSessionPerItem() {
			// 🔴 ONE LIVE CLAIM PER CAPPED JUDGEMENT INSTANCE, AND ONE ITEM PER
			// FRESH POOL'S CONVERSATION, before HQ and before the session
			// ledger (a refused claim spends no session). The wake loop
			// enforces max_item_minutes by stopping the holder's whole
			// instance, so a second item it held would lose its work
			// (oneClaimRefusal); a fresh pool's conversation is marked by its
			// first successful claim, below, and refused any other item
			// however that hold ends (freshClaimRefusal). Serialised per
			// instance, so two parallel calls cannot both pass, and the mark
			// is written under the lock.
			unlock := s.officerRT.lockClaims(instance)
			defer unlock()
			if refusal := s.freshClaimRefusal(o, instance, in.ItemID); refusal != "" {
				return refusal, true
			}
			// Still asked for a fresh pool: HQ's claimed list is the one
			// record a RESTARTED muster has of a claim the conversation holds
			// (worked is this process's memory).
			if refusal := s.oneClaimRefusal(ctx, c, o, instance, in.ItemID); refusal != "" {
				return refusal, true
			}
		}
		if strings.TrimSpace(string(in.ItemID)) == "" {
			// no item_id: HQ picks the next item (claim-next)
			var claimed officers.ItemID
			// the claim's tier is the instance's pool's (o.Tier; "" for an
			// untiered officer sends no tier key), exactly what claim-by-id
			// sends: HQ then picks only an item of that tier
			// (officer_claim_next_tier_test.go)
			// The judgement session cap holds for claim-next too: HQ picks
			// the item, so a session is held until HQ says which (or none).
			done, capErr := s.officerRT.judgement.ReserveNext(ctx, o, s.officerRT.clock())
			if capErr != nil {
				return s.sessionCapRefusal(o, name, capErr), true
			}
			body, claimed, err = s.officerClaimNext(ctx, c, instance, in.LeaseSeconds, o.Tier)
			if err != nil {
				claimed = ""
			}
			// An answer HQ lost (outcome unknown) keeps the session.
			done(claimed, claimOutcomeKnown(err))
			if errors.Is(err, errNoClaimNext) {
				return toolErr(noClaimNextNote), true
			}
			if err == nil && claimed != "" && s.officerRT.fwd != nil {
				s.officerRT.fwd.Claimed(o, instance, claimed)
			}
			if err == nil && claimed != "" {
				s.llmClaimed(o, instance, claimed, body)
				s.officerRT.claimedInConversation(o, instance, claimed)
				s.hostInstanceClaimed(ctx, o, instance)
			}
			break
		}
		if bad := needItem(); bad != "" {
			return bad, true
		}
		// 🔴 THE JUDGEMENT SESSION CAP IS CHECKED BEFORE HQ, AND THE SESSION
		// IS RESERVED AT ONCE: the N+1th distinct item of the day never reaches
		// HQ (it stays queued there), and two claims racing for the last
		// session cannot both pass. A claim HQ refuses gives its session back.
		undo, capErr := s.officerRT.judgement.Reserve(ctx, o, in.ItemID, s.officerRT.clock())
		if capErr != nil {
			return s.sessionCapRefusal(o, name, capErr), true
		}
		body, err = s.officerClaim(ctx, c, in.ItemID, instance, in.LeaseSeconds, o.Tier)
		// Undo only a claim that DEFINITELY did not happen: when HQ's answer
		// was lost, HQ may have claimed it, and the session is kept.
		if err != nil && claimOutcomeKnown(err) {
			undo()
		}
		if err == nil && s.officerRT.fwd != nil {
			s.officerRT.fwd.Claimed(o, instance, in.ItemID)
		}
		if err == nil {
			s.llmClaimed(o, instance, in.ItemID, body)
			s.officerRT.claimedInConversation(o, instance, in.ItemID)
			s.hostInstanceClaimed(ctx, o, instance)
		}
	case officers.ToolHeartbeat:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		body, err = c.Heartbeat(ctx, in.ItemID, instance, in.LeaseSeconds)
	case officers.ToolComplete:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		outcomes := officers.CompleteOutcomes(o)
		if !slices.Contains(outcomes, in.Outcome) {
			return toolErr("outcome must be one of %s", strings.Join(outcomes, ", ")), true
		}
		// 🔴 A BLOCK MUSTER DOES NOT FORWARD IS REFUSED, NEVER DROPPED: a
		// routine instance is not offered investigation (its hand-off is
		// needs_judgement), and silently completing without the block would
		// read as accepted. JSON null is no block.
		inv := bytes.TrimSpace(in.Investigation)
		if string(inv) == "null" {
			inv = nil
		}
		if len(inv) > 0 && !officers.CompleteTakesInvestigation(o) {
			return toolErr("investigation is not a routine instance's: an item that needs investigating is the judgement pool's — complete it with outcome %s and what you found in the summary",
				officers.OutcomeNeedsJudgement), true
		}
		// 🔴 REFUSED, NEVER DROPPED, on a builtin officer: it is not offered
		// them, and its backend would complete the item without them.
		if !completeTakesHQFields(o) {
			for f, raw := range map[string]json.RawMessage{"authors": in.Authors, "attributed_prs": in.AttributedPRs, "attribution": in.Attribution, "tickets": in.Tickets} {
				if len(bytes.TrimSpace(raw)) > 0 {
					return toolErr("%s is not served by officer %s's builtin backend (it has no author mail, attribution or tickets): complete without it", f, o.ID), true
				}
			}
		}
		if strings.TrimSpace(in.Summary) == "" || utf8.RuneCountInString(in.Summary) > officers.MaxSummaryChars {
			return toolErr("summary is required and at most %d characters", officers.MaxSummaryChars), true
		}
		if len(in.Links) > officers.MaxLinks {
			return toolErr("at most %d links", officers.MaxLinks), true
		}
		handoff, bad := completeHandoff(in.Handoff, in.Outcome, o.Ship.IsShepherd())
		if bad != "" {
			return toolErr("%s", bad), true
		}
		// A hand-off's head is HQ's to check (409 nothing_pushed /
		// head_not_live): muster keeps no memory of pushes to decide it.
		// 🔴 NO COMPLETE ON A STALE CHARTER (charterRefusal): an item claimed
		// before its type's charter was re-put is not finished against the
		// old one. The lease is kept (heartbeat and release are not gated).
		if refusal := charterRefusal(ctx, c, name, instance); refusal != "" {
			return refusal, true
		}
		// 🔴 THE COST NEVER FAILS A COMPLETE: an unknown one is not sent, and
		// one HQ refuses is dropped and the complete sent again without it
		// (CompleteDroppingCost). It is read here, before officerItemEnded
		// clears the hold. It is the hold's cost only when no request admitted
		// under the hold is still unbooked (the meter says inflight otherwise,
		// and nothing is sent), so a figure that rides this call is complete.
		cost, why := s.llmItemCost(o, instance, in.ItemID, in.Outcome)
		// 🔴 THE SHADOW BLOCK RIDES THE SUMMARY HQ RECEIVES (officer_shadow.go):
		// a shepherd's writes refused in shadow, under this item; cut to
		// HQ's bound, never the instance's own text.
		summary := s.officerRT.shadowSummary(o.ID, in.ItemID, in.Summary, officers.MaxSummaryChars)
		done := officers.Completion{Outcome: in.Outcome, Summary: summary, Links: in.Links, InvestigationJSON: inv, Cost: cost, Handoff: handoff,
			AuthorsJSON: in.Authors, AttributedPRsJSON: in.AttributedPRs, AttributionJSON: in.Attribution, TicketsJSON: in.Tickets}
		var refused *officers.HQError
		body, refused, err = officers.CompleteDroppingCost(ctx, c, in.ItemID, instance, done)
		if refused != nil {
			s.logger.Printf("officers: %s completed item %s without its cost: HQ refused cost_usd %.6f from %s: %s",
				instance, in.ItemID, done.Cost.USD, done.Cost.From, refused.Error())
		}
		if err == nil && cost == nil {
			// One line per complete sent without a cost, with why: the
			// known-vs-unknown ratio is measured from these. No item content.
			s.logger.Printf("officers: item cost omitted officer=%s instance=%s item=%s outcome=%s reason=%s",
				o.ID, instance, in.ItemID, in.Outcome, why)
		}
		if err == nil {
			s.officerRT.forgetShadow(o.ID, in.ItemID)
			s.officerItemEnded(o, instance, in.ItemID)
		}
	case officers.ToolRelease:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		body, err = c.Release(ctx, in.ItemID, instance)
		if err == nil {
			// The hold ended: its shadow lines go with it (a later holder's
			// completion must not carry them).
			s.officerRT.forgetShadow(o.ID, in.ItemID)
			s.officerItemEnded(o, instance, in.ItemID)
		}
	case officers.ToolAskPerson:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		ask := officers.Ask{To: in.To, Question: in.Question, Options: in.Options, Images: in.ImageIDs, ContextURL: in.ContextURL, DeadlineSeconds: in.DeadlineSeconds}
		return s.officerAskPerson(ctx, c, o, instance, in.ItemID, ask), true
	case officers.ToolReleaseEvaluation:
		post, perr := officers.ParseCaptainPost([]byte(orEmptyJSON(arguments)))
		if perr != nil {
			return toolErr("%v", perr), true
		}
		cb, serr := officers.Serves[officers.CaptainBackend](c, officers.CapCaptain)
		if serr != nil {
			return notServedResult(name, serr), true
		}
		body, err = cb.ReleaseCaptain(ctx, instance, post)
	}
	// An item's payload (payload.claims included) is HQ's data, handed on as
	// HQ wrote it: officerToolResult returns HQ's body byte for byte. HQ
	// bounds it (claims at 16 KiB); muster applies no second cap.
	return officerToolResultFor(o, name, body, err), true
}

// askedNote is what a successful officer_ask_person adds to HQ's answer.
const askedNote = "asked. Your lease on this item has ENDED: HQ set it to waiting with no holder. Stop working it now — do not heartbeat, complete or release it. " +
	"HQ re-opens it (open, claimable) with the answer on it (asks[] in officer_items, the claim response and officer_brief's open_items) once every open ask on it is answered or withdrawn, " +
	"and muster wakes an instance for it: claim it then and carry on with the answer. If the deadline passes unanswered, HQ escalates the item to its owner. " +
	"To ask another question on it meanwhile, call officer_ask_person again with the same item_id."

// officerAskPerson is officer_ask_person: the ask, as the instance gave it
// (image_ids become the body's images), sent to HQ. HQ checks every limit.
//
// 🔴 A SUCCESS ENDS THE INSTANCE'S LEASE (HQ's contract), so the transcript
// forwarder and the LLM proxy's item meter stop for the item as on
// complete/release, and the result says so
// in words (askedNote): the instance's next move is to stop working the item.
func (s *Server) officerAskPerson(ctx context.Context, c officers.Backend, o officers.Officer, instance string, item officers.ItemID, ask officers.Ask) string {
	ab, err := officers.Serves[officers.AsksBackend](c, officers.CapAsks)
	if err != nil {
		return notServedResult(officers.ToolAskPerson, err)
	}
	body, err := ab.CreateAsk(ctx, item, instance, ask)
	if err == nil {
		s.officerItemEnded(o, instance, item)
	}
	return askToolResult(o, body, err)
}

// officerItemEnded is THE end of an instance's hold on item, by its own tool
// (officer_complete, officer_release, officer_ask_person): the transcript
// forwarder and the LLM proxy's item meter stop for it. (A
// fresh_session_per_item pool's one-item rule does not wait for this: its
// conversation is marked at the claim, claimedInConversation.)
func (s *Server) officerItemEnded(o officers.Officer, instance string, item officers.ItemID) {
	if s.officerRT.fwd != nil {
		s.officerRT.fwd.Ended(o, instance, item)
	}
	s.llmEnded(o, instance, item)
}

// freshClaimRefusal is officer_claim's refusal for an instance of a
// fresh_session_per_item pool whose current conversation has already claimed
// an item other than item ("" = claim-next, whose item is HQ's to pick, so
// any earlier claim refuses it), or "" otherwise. The conversation is marked
// at its first SUCCESSFUL claim (claimedInConversation), so it stays refused
// however that hold ends — complete, release, ask, a lapsed lease, a complete
// HQ refuses — and while it is still held (one claim at a time, as a capped
// pool's oneClaimRefusal holds it). Re-claiming its OWN item (a lapsed lease
// it still works) is not refused. The instance's next item is muster's to
// start, in a new conversation (OfficerTurnContext), which frees it.
//
// ⚠ What it rests on is this process's memory (officerRuntime.worked): a
// muster restarted while an instance's turn is still running forgets that
// the turn's conversation claimed an item, and that one turn may claim one
// more. Every turn muster sends afterwards starts a new conversation.
func (s *Server) freshClaimRefusal(o officers.Officer, instance string, item officers.ItemID) string {
	if !o.FreshSessionPerItem() {
		return ""
	}
	worked := s.officerRT.workedItem(instance)
	if worked == "" || (item != "" && item == worked) {
		return ""
	}
	return toolErr("this conversation has already claimed item %s, and tiers.%s.fresh_session_per_item gives each item a NEW conversation: "+
		"do not claim another here. Finish item %s (complete or release it) if you still hold it, then stop and end your turn; "+
		"muster starts you a new conversation for the next item", worked, o.Tier, worked)
}

// askToolResult is officer_ask_person's answer: HQ's JSON with askedNote on
// success; HQ's status, code and message on a 4xx (refused.code beside HQ's
// body, so an instance can branch on lease_lost / unknown_recipient /
// rate_limited without parsing it); officerToolResult's shapes otherwise.
func askToolResult(o officers.Officer, body []byte, err error) string {
	note := askedNote
	if o.IsBuiltin() {
		note = builtinAskedNote
	}
	return itemAskToolResultFrom(toolSource(o), officers.ToolAskPerson, note, body, err)
}

// builtinAskedNote is askedNote for a builtin officer: muster holds its queue.
const builtinAskedNote = "asked. Your lease on this item has ENDED: muster set it to waiting with no holder. Stop working it now — do not heartbeat, complete or release it. " +
	"muster re-opens it (open, claimable) with the answer on it (asks[] in officer_items, the claim response and officer_brief's open_items) once every open ask on it is answered or withdrawn, " +
	"and wakes an instance for it: claim it then and carry on with the answer. If the deadline passes unanswered, muster escalates the item to its owner. " +
	"To ask another question on it meanwhile, call officer_ask_person again with the same item_id."

// itemAskToolResult is the answer of a tool whose success creates an HQ ask
// on the item (officer_ask_person, officer_task_ask,
// officer_flood_block_propose) — and of officer_rollout_evidence_attach,
// whose success ends nothing but whose refusals are passed the same way:
// HQ's JSON with note added on success; on a
// 4xx (or a 5xx the route calls a decision) HQ's status, code and message,
// with HQ's body verbatim under refused.hq; officerToolResult's shapes
// otherwise (outage, retryable, too large).
func itemAskToolResult(tool, note string, body []byte, err error) string {
	return itemAskToolResultFrom(srcHQ, tool, note, body, err)
}

// itemAskToolResultFrom is itemAskToolResult with src naming the backend.
func itemAskToolResultFrom(src, tool, note string, body []byte, err error) string {
	if err == nil {
		var out map[string]json.RawMessage
		if json.Unmarshal(body, &out) != nil || out == nil {
			return toolResultFrom(src, tool, body, nil)
		}
		n, _ := json.Marshal(note)
		out["note"] = n
		return toolJSON(out)
	}
	status, msg, code, ok := officers.Refusal(err)
	if !ok {
		return toolResultFrom(src, tool, nil, err)
	}
	head := strconv.Itoa(status)
	if code != "" {
		head += " " + code
	}
	text := fmt.Sprintf("%s refused %s (%s)", src, tool, head)
	if msg != "" {
		text += ": " + msg
	}
	refused := map[string]any{"status": status}
	if code != "" {
		refused["code"] = code
	}
	var hqErr *officers.HQError
	if errors.As(err, &hqErr) && json.Valid(hqErr.Body) {
		refused["hq"] = json.RawMessage(hqErr.Body)
	}
	return toolJSON(map[string]any{"error": text, "refused": refused})
}

// nothingToClaimNote is officer_claim's answer when nothing is claimable.
const nothingToClaimNote = "nothing to claim right now: every open item is held, blocked or not due. Stop and wait — muster wakes you when HQ has new work."

// noClaimNextNote is officer_claim{}'s tool error on an HQ without claim-next
// (its route answers 404): the instance claims by id instead.
const noClaimNextNote = "this HQ has no claim-next; call officer_items and officer_claim{item_id}"

// errNoClaimNext is HQ answering claim-next 404: a route it does not have.
var errNoClaimNext = errors.New("officers: HQ has no claim-next route")

// officerClaimNext is officer_claim with no item_id: HQ's claim-next (POST
// /officers/{id}/claim-next) for instance, sending tier as the claim does.
// It answers HQ's {item, lease_expires_at} and the claimed item's id; when
// nothing is claimable, {item: null, claimed: false, note} and no id. An HQ
// without the route (404) is errNoClaimNext, which the tool answers with
// noClaimNextNote.
func (s *Server) officerClaimNext(ctx context.Context, c officers.Backend, instance string, lease int, tier string) ([]byte, officers.ItemID, error) {
	body, err := c.ClaimNext(ctx, instance, lease, tier)
	var hqErr *officers.HQError
	if errors.As(err, &hqErr) && hqErr.Status == http.StatusNotFound {
		return nil, "", errNoClaimNext
	}
	if err != nil {
		return nil, "", err
	}
	var res struct {
		Item *struct {
			ID officers.ItemID `json:"id"`
		} `json:"item"`
	}
	if json.Unmarshal(body, &res) != nil {
		return body, "", nil // officerToolResult reports what is not JSON
	}
	if res.Item == nil {
		return nothingToClaim(), "", nil
	}
	// 🔴 A CHAT ITEM IS NEVER THE PRIMARY'S (officer_chat.go): HQ's queue order
	// may hand one to claim-next; it is released at once and the instance told
	// to claim by id instead — nothing was claimed.
	if item, chat := s.claimedChatItem(c.Officer(), body); chat {
		s.releaseChatClaim(ctx, c, c.Officer(), instance, item)
		return []byte((&chatItemRefusal{item: item}).result()), "", nil
	}
	return body, res.Item.ID, nil
}

// charterRefusal is THE charter gate of an instance's officer_claim and
// officer_complete (officers.CharterGated): the tool result refusing it when
// the backend says instance was not briefed with its type's current
// charter_version, and "" otherwise — or when the backend keeps no such
// record (HQ's client). An unreadable record refuses as an outage.
func charterRefusal(ctx context.Context, c officers.Backend, tool, instance string) string {
	g, ok := c.(officers.CharterGated)
	if !ok {
		return ""
	}
	if err := g.CharterCurrent(ctx, instance); err != nil {
		return officerToolResult(tool, nil, err)
	}
	return ""
}

// oneClaimRefusal is officer_claim's refusal for an instance of a capped
// judgement pool (o.ItemCap() > 0) or of a fresh_session_per_item pool that
// already holds a live claim on an item
// other than item ("" = claim-next), or "" when it holds none. HQ's claimed
// list is the truth; when it cannot be read the claim is refused (fail
// closed: nothing is claimed without the check).
func (s *Server) oneClaimRefusal(ctx context.Context, c officers.Backend, o officers.Officer, instance string, item officers.ItemID) string {
	claimed, err := c.AllItems(ctx, officers.StatusClaimed)
	if err != nil {
		s.logger.Printf("officers: %s: %s refused for %s, the claimed items could not be read: %v", o.ID, officers.ToolClaim, instance, err)
		return toolErr("muster could not check which item you already hold, so nothing was claimed; try %s again in a minute", officers.ToolClaim)
	}
	now := s.officerRT.clock()
	for _, it := range claimed {
		if it.ClaimedBy == instance && it.ID != item && it.LeaseLive(now) {
			why := "tiers.judgement.max_item_minutes stops your whole instance at one item's cap, which would lose any other"
			if o.ItemCap() == 0 {
				why = "tiers." + o.Tier + ".fresh_session_per_item gives each item a conversation of its own"
			}
			return toolErr("you already hold item %s: an instance of this pool works one item at a time (%s). "+
				"Complete (%s) or release (%s) item %s before claiming another", it.ID, why, officers.ToolComplete, officers.ToolRelease, it.ID)
		}
	}
	return ""
}

// lockClaims serialises officer_claim for one instance; call the returned
// func to unlock.
func (rt *officerRuntime) lockClaims(instance string) func() {
	v, _ := rt.claimLocks.LoadOrStore(instance, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// nothingToClaim is officer_claim's answer when nothing is claimable.
func nothingToClaim() []byte {
	b, _ := json.Marshal(map[string]any{"item": nil, "claimed": false, "note": nothingToClaimNote})
	return b
}

// claimOutcomeKnown reports whether a claim's result says for sure what HQ
// did: success, or HQ's own refusal (a 4xx, a blocked item, no claim-next
// route). A transport failure, a timeout or a 5xx is UNKNOWN — HQ may have
// committed the claim and lost the answer.
func claimOutcomeKnown(err error) bool {
	var hqErr *officers.HQError
	var blocked *claimBlockedError
	var chat *chatItemRefusal
	return err == nil || errors.As(err, &hqErr) || errors.As(err, &blocked) || errors.As(err, &chat) || errors.Is(err, errNoClaimNext)
}

// sessionCapRefusal is a claim the judgement session ledger did not admit:
// the cap's own refusal as it is, or — when the ledger could not be read or
// written — a refusal that fails CLOSED (no claim without the count) and keeps
// the database's error in the log, not in the instance's transcript.
func (s *Server) sessionCapRefusal(o officers.Officer, name string, err error) string {
	if errors.Is(err, officers.ErrSessionCap) {
		return toolErr("%v", err)
	}
	s.logger.Printf("officers: %s: %s refused, the judgement session ledger failed: %v", o.ID, name, err)
	return toolErr("the judgement session count could not be read, so nothing was claimed; try %s again in a minute", name)
}

// alreadyYoursNote is what a claim on an item the caller already holds says.
const alreadyYoursNote = "already yours — lease renewed (you held this item under a live lease; keep working it and heartbeat as usual)"

// officerClaim is officer_claim's HQ call.
//
// 🔴 A 409 NAMING THE CALLER IS NOT A CONFLICT. HQ refuses a claim on ANY live
// lease — the caller's own included ("claiming your own live lease is a 409
// too (use heartbeat)", civ-hq docs/OFFICERS.md). muster's instance id is the
// agent's name, so a restarted pod is the SAME instance, and the first thing
// it does is re-claim the item it was working. Passed through, that 409 tells
// it "another instance has it — move on", and it abandons its own item until
// the lease lapses. So when HQ's held_by is the calling instance, the claim
// becomes a heartbeat with the requested lease and answers as a successful
// claim. If the lease ran out between the two calls (heartbeat 409), the item
// is claimable again and the claim is retried once. A 409 naming anyone else
// is returned unchanged.
//
// tier is the calling instance's pool's (officers.Officer.Tier), sent on
// every claim — the retry too — so HQ takes only an item of that tier (409
// wrong_tier, passed back unchanged); "" (an untiered officer) sends none.
func (s *Server) officerClaim(ctx context.Context, c officers.Backend, item officers.ItemID, instance string, lease int, tier string) ([]byte, error) {
	body, err := s.officerClaimOnce(ctx, c, item, instance, lease, tier)
	// 🔴 A CHAT ITEM IS NEVER THE PRIMARY'S (officer_chat.go): claimed by id
	// (HQ's answer names its kind), it is released at once and refused.
	if err == nil {
		if chat, ok := s.claimedChatItem(c.Officer(), body); ok {
			s.releaseChatClaim(ctx, c, c.Officer(), instance, chat)
			return nil, &chatItemRefusal{item: chat}
		}
	}
	return body, err
}

// officerClaimOnce is officerClaim's claim, with its already-yours handling.
func (s *Server) officerClaimOnce(ctx context.Context, c officers.Backend, item officers.ItemID, instance string, lease int, tier string) ([]byte, error) {
	body, err := c.Claim(ctx, item, instance, lease, tier)
	var hqErr *officers.HQError
	if err == nil || !errors.As(err, &hqErr) || hqErr.Status != http.StatusConflict {
		return body, err
	}
	if b := asClaimBlocked(item, err); b != nil {
		return nil, b
	}
	var held struct {
		HeldBy string `json:"held_by"`
	}
	if json.Unmarshal(hqErr.Body, &held) != nil || held.HeldBy != instance {
		return body, err
	}
	hb, hbErr := c.Heartbeat(ctx, item, instance, lease)
	var hbHQ *officers.HQError
	if errors.As(hbErr, &hbHQ) && hbHQ.Status == http.StatusConflict {
		body, err := c.Claim(ctx, item, instance, lease, tier)
		if b := asClaimBlocked(item, err); b != nil {
			return nil, b
		}
		return body, err
	}
	if hbErr != nil {
		return nil, hbErr
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(hb, &out) != nil || out == nil {
		out = map[string]json.RawMessage{}
	}
	out["already_yours"] = json.RawMessage("true")
	note, _ := json.Marshal(alreadyYoursNote)
	out["note"] = note
	b, mErr := json.Marshal(out)
	if mErr != nil {
		return hb, nil
	}
	return b, nil
}

// claimBlockedError is HQ refusing a claim with 409 {code: blocked,
// blocked_by}: the item's task waits on unfinished tasks (HQ's task ordering).
// blocked_by holds HQ TASK ids, never officer item ids, so they are kept as
// plain strings and the note calls them tasks: an instance must not read them
// as items it could claim.
//
// It stays an error to the code (the claim did not happen: nothing that runs
// on a successful claim may run), but officerToolResult turns it into a
// NON-error result for the instance. It is not a failed call and not "another
// instance holds it": the queue is ordered, and the instance's next step is
// simply another item.
type claimBlockedError struct {
	item officers.ItemID
	// tasks are the HQ task ids the item's task waits on.
	tasks []string
	// heldKind: the built-in store's refusal of an item HELD past its
	// kind's bound (a draft.refused item, builtin.DraftEndedTx): it waits on
	// none of the officer's tasks but on one of its open items of this kind
	// being finished. "" for HQ's task ordering.
	heldKind string
}

// asClaimBlocked is err as a *claimBlockedError when it is HQ's 409
// `code: "blocked"` on a claim of item, and nil otherwise.
func asClaimBlocked(item officers.ItemID, err error) *claimBlockedError {
	var hqErr *officers.HQError
	if !errors.As(err, &hqErr) || hqErr.Status != http.StatusConflict {
		return nil
	}
	var refusal struct {
		Code      string   `json:"code"`
		BlockedBy []string `json:"blocked_by"`
		HeldKind  string   `json:"held_kind"`
	}
	if json.Unmarshal(hqErr.Body, &refusal) != nil || refusal.Code != "blocked" {
		return nil
	}
	return &claimBlockedError{item: item, tasks: refusal.BlockedBy, heldKind: refusal.HeldKind}
}

func (e *claimBlockedError) Error() string {
	return fmt.Sprintf("officers: item %s waits on %s", e.item, e.waitsOn())
}

func (e *claimBlockedError) waitsOn() string {
	if len(e.tasks) == 0 {
		return "unfinished tasks"
	}
	return "tasks " + strings.Join(e.tasks, ", ")
}

// result is the instance-facing answer.
func (e *claimBlockedError) result() string {
	tasks := e.tasks
	if tasks == nil {
		tasks = []string{}
	}
	if e.heldKind != "" {
		return toolJSON(map[string]any{
			"claimed": false, "blocked": true, "held": true, "item_id": e.item, "blocked_by": tasks,
			"note": fmt.Sprintf("not claimed: item %s is held — you already have the most open %s items muster lets you work at once, and it is delivered to you (claimable) once one of them is finished; finish those first, or claim another open item (%s lists them)",
				e.item, e.heldKind, officers.ToolItems),
		})
	}
	return toolJSON(map[string]any{
		"claimed": false, "blocked": true, "item_id": e.item, "blocked_by": tasks,
		"note": fmt.Sprintf("not claimed: item %s waits on %s (not done yet); claim another open item (%s lists them; an item marked blocked cannot be claimed until the tasks it waits on are done)", e.item, e.waitsOn(), officers.ToolItems),
	})
}

// notServedResult is the refusal of tool name when the officer's backend does
// not serve its verb (officers.ErrNotServed). An HQ backend serves every verb,
// so an HQ officer never sees it; the listing (officerToolServed) does not
// offer such a tool in the first place.
func notServedResult(name string, err error) string {
	return toolErr("%s is not available: %v", name, err)
}

// officerToolServed is THE rule for whether officer o's instances get tool
// (officers.ToolServed): o's shape allows it AND o's backend serves its
// capability. The listing (toolsFor), the session allow-list
// (OfficerClaudeCodeSession), dispatch and the ask-image route all ask it.
func (s *Server) officerToolServed(o officers.Officer, tool string) bool {
	return officers.ToolServed(o, s.officerRT.hq.Backend(o).Capabilities(), tool)
}

// officerToolResult is HQ's answer as a tool result: HQ's JSON on success; a
// structured refusal carrying HQ's status and body on a 4xx (a 409 on a claim
// names the holder, which is what the instance needs); a plain retry-later on
// an outage. A claim on a blocked item is the one 4xx that is not a refusal
// (claimBlockedError).
func officerToolResult(name string, body []byte, err error) string {
	return toolResultFrom(srcHQ, name, body, err)
}

// The words a tool result names the officer's backend with (toolSource).
const (
	srcHQ      = "HQ"
	srcBuiltin = "muster"
)

// toolSource is how a tool result names officer o's backend: HQ, or muster
// for a builtin officer — whose instances never read "HQ" (F4).
func toolSource(o officers.Officer) string {
	if o.IsBuiltin() {
		return srcBuiltin
	}
	return srcHQ
}

// officerToolResultFor is officerToolResult worded for officer o's backend.
func officerToolResultFor(o officers.Officer, name string, body []byte, err error) string {
	return toolResultFrom(toolSource(o), name, body, err)
}

// toolResultFrom is officerToolResult with src naming the backend.
func toolResultFrom(src, name string, body []byte, err error) string {
	var hqErr *officers.HQError
	var blocked *claimBlockedError
	var chatRefused *chatItemRefusal
	var retry *officers.RetryableError
	switch {
	case errors.As(err, &retry):
		// Not `refused` (a final answer): `retryable`, still an error to MCP.
		again := map[string]any{"status": retry.Status, "code": retry.Code()}
		if json.Valid(retry.Body) {
			again["hq"] = json.RawMessage(retry.Body)
		}
		return toolJSON(map[string]any{"error": fmt.Sprintf("%s answered %d %s to %s: a transient failure — retry it in a minute", src, retry.Status, retry.Code(), name),
			"retryable": again})
	case errors.As(err, &blocked):
		return blocked.result()
	case errors.As(err, &chatRefused):
		return chatRefused.result()
	case err == nil:
		if !json.Valid(body) {
			return toolErr("%s answered %s with something that is not JSON", src, name)
		}
		return string(body)
	case errors.As(err, &hqErr):
		refused := map[string]any{"status": hqErr.Status}
		if json.Valid(hqErr.Body) {
			refused["hq"] = json.RawMessage(hqErr.Body)
		}
		return toolJSON(map[string]any{"error": fmt.Sprintf("%s refused %s (%d)", src, name, hqErr.Status), "refused": refused})
	case errors.Is(err, officers.ErrHQAnswerTooLarge):
		return toolErr("%s's answer to %s is over muster's 1 MiB cap, so none of it is passed on: ask for less (a smaller limit or page, a narrower filter) — the same call will fail again", src, name)
	case errors.Is(err, officers.ErrHQUnavailable):
		return toolErr("%s is unavailable right now; try %s again in a minute", src, name)
	case errors.Is(err, officers.ErrNotServed):
		return notServedResult(name, err)
	default:
		return toolErr("%s failed", name)
	}
}

// --- the ask-image upload route (POST podagent.AskImagePath) -----------------

// registerOfficerRoutes registers the officer instances' own agent-token
// routes. It runs on every server, so the route set is fixed; without a
// declared officer every call is refused (officerOf).
func (s *Server) registerOfficerRoutes(mux Mux) {
	mux.HandleFunc("POST "+podagent.AskImagePath, s.requireRelayAgent(s.handleOfficerAskImage))
}

// handleOfficerAskImage is POST /agent/officer/ask-images?filename=<name>. An
// officer instance's pod (`ccd ask-image`, podagent.UploadAskImage) sends a
// workspace image as the raw body. muster forwards it to HQ's
// POST /officers/{id}/ask-images with the officer credential, and answers
// HQ's {id, filename, content_type, size, sha256} with 201.
//
//   - 🔴 THE OFFICER IS THE CALLING AGENT'S, from its own token
//     (requireRelayAgent, then officerOf), never a request field. A query
//     naming another officer is refused, not ignored, as the tools refuse it.
//     An agent that is no officer's instance is refused (403).
//   - 🔴 THE BYTES ARE CHECKED HERE, WHATEVER SENT THEM. They are read
//     through a LimitReader of askimage.MaxBytes+1 (more is a 413, before
//     anything is sent to HQ), and typed by sniffing them (askimage.Sniff:
//     PNG, JPEG or WebP, else 415), never by the Content-Type or the name.
//     The pod checks the same, but anything in the pod can call this route
//     with the agent's token.
//   - HQ's refusal comes back with HQ's status and {error, code}, except a
//     401/403: that is HQ refusing muster's officer credential, not the
//     agent's token, so it is a 502 officer_credential_rejected. An outage,
//     503.
func (s *Server) handleOfficerAskImage(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	o, _, ok := s.officerOf(a)
	if !ok || !s.officerToolServed(o, officers.ToolAskPerson) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "only an officer instance may upload an ask image", "code": "not_an_officer"})
		return
	}
	q := r.URL.Query()
	for _, k := range []string{"officer", "officer_id"} {
		if v, named := q[k]; named && (len(v) != 1 || v[0] != o.ID) {
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "you act for officer " + o.ID + " only; an upload cannot name another officer", "code": "another_officer"})
			return
		}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, askimage.MaxBytes+1))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the image could not be read", "code": "invalid"})
		return
	}
	if len(data) > askimage.MaxBytes {
		s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("the image is over %d MiB", askimage.MaxBytes>>20), "code": "too_large"})
		return
	}
	ct, ok := askimage.Sniff(data)
	if !ok {
		s.writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "the bytes are " + ct + ", not a PNG, JPEG or WebP image", "code": "unsupported_media_type"})
		return
	}
	name := officers.AskFilename(path.Base(q.Get("filename")), ct, 1)
	ab, err := officers.Serves[officers.AsksBackend](s.officerRT.hq.Backend(o), officers.CapAsks)
	if err != nil {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "not_served"})
		return
	}
	up, err := ab.UploadAskImage(r.Context(), name, ct, data)
	if err == nil {
		s.writeJSON(w, http.StatusCreated, up)
		return
	}
	if status, msg, code, ok := officers.Refusal(err); ok {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			// HQ refused muster's officer credential, not the pod's agent token
			// (muster accepted that): passing 401/403 on would read as "your
			// token is bad". 502 names the credential instead.
			s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("HQ refused muster's credential for officer %s (%d); your agent token is fine: the operator must fix the officer credential", o.ID, status), "code": "officer_credential_rejected"})
			return
		}
		if msg == "" {
			msg = fmt.Sprintf("HQ refused the image (%d)", status)
		}
		s.writeJSON(w, status, map[string]any{"error": msg, "code": code})
		return
	}
	s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "HQ is unavailable right now; try again in a minute", "code": "hq_unavailable"})
}
