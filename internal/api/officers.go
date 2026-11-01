package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/transcript"
	"github.com/ZacxDev/muster/internal/officership"
	"github.com/ZacxDev/muster/internal/provision"
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
// officer is declared.
type officerRuntime struct {
	reg *officers.Registry
	hq  *officers.ClientHQ
	rec *officers.Reconciler
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
}

// OfficersConfig is EnableOfficers' input.
type OfficersConfig struct {
	Officers []officers.Officer
	// HTTPClient is the HQ client's transport; nil uses officers.NewClient's.
	HTTPClient *http.Client
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
	ShipHosts  [2]string
	ShipHTTP   *http.Client
	// ShipCI replaces a ci_logs officer's cluster reader (tests); nil is the
	// in-cluster API with a token minted per call for its service_account.
	ShipCI func(officers.Officer) officership.CIReader
	// NoShipResume skips resuming rollout watches at boot (tests).
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
func (s *Server) newTranscriptForwarder(cfg OfficersConfig, hq *officers.ClientHQ) (*transcript.Forwarder, error) {
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
	f, err := transcript.New(transcript.Config{Source: src, Poster: transcript.NewClientPoster(hq), Logger: s.logger, Now: cfg.Now, Interval: cfg.TranscriptInterval})
	if err != nil {
		return nil, err
	}
	s.logger.Printf("officers: forwarding instance transcripts to HQ for %s while an instance holds a claimed item", strings.Join(on, ", "))
	return f, nil
}

// EnableOfficers turns the officer runtime on and returns its wake loop for the
// caller to Run. It needs the agents store and the provisioner (an officer is
// nothing without instances), and the claude-code kind.
func (s *Server) EnableOfficers(cfg OfficersConfig) (*officers.Reconciler, error) {
	if len(cfg.Officers) == 0 {
		return nil, errors.New("officers: none declared")
	}
	if s.ext.Agents == nil || s.ext.Provisioner == nil {
		return nil, errors.New("officers: need the agents store and an agent provisioner")
	}
	if cfg.InstallSession == nil {
		return nil, errors.New("officers: no way to install the instances' session settings on the provisioner (every tool allowed, officer tools, model)")
	}
	if !s.ext.kindEnabled(agents.KindClaudeCode) {
		return nil, fmt.Errorf("officers: instances run on the %s kind, which MUSTER_AGENT_KINDS does not enable", agents.KindClaudeCode)
	}
	reg := officers.NewRegistry(cfg.Officers)
	hc := cfg.HTTPClient
	hq := officers.NewClientHQ(func(o officers.Officer) *officers.Client { return officers.NewClient(o, hc) })
	fwd, err := s.newTranscriptForwarder(cfg, hq)
	if err != nil {
		return nil, err
	}
	rcfg := officers.Config{Registry: reg, HQ: hq, Fleet: officerFleet{s: s}, Logger: s.logger, Now: cfg.Now}
	if fwd != nil {
		rcfg.Observed = fwd.Sync
	}
	rec, err := officers.NewReconciler(rcfg)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	s.officerRT = &officerRuntime{reg: reg, hq: hq, rec: rec, fwd: fwd, claimOwner: fmt.Sprintf("officer-reprompt/%s/%d", host, os.Getpid())}
	if err := s.enableShips(cfg); err != nil {
		s.officerRT = nil
		return nil, err
	}
	cfg.InstallSession(s.OfficerClaudeCodeSession)
	if fwd != nil && !cfg.NoTranscriptRun {
		// Ends with the server's streams (EndStreams, at shutdown).
		ctx := s.streams.context()
		safeGo(s.logger, "officer transcript forwarder", func() { fwd.Run(ctx) })
	}
	for _, o := range reg.All() {
		repo := "none"
		if o.Repo != "" {
			repo = o.Repo + " (github connector, read through the proxy; no checkout)"
		}
		s.logger.Printf("officers: %s on: instances %s..%s, idle scale-to-zero after %s, poll %s, repo %s",
			o.ID, officers.InstanceName(o.ID, 1), officers.InstanceName(o.ID, o.MaxInstances), o.IdleAfter, o.PollInterval, repo)
	}
	return rec, nil
}

// officerOf is THE binding rule: the declared officer agent a is an instance
// of, and which instance. Not an officer instance → ok false.
func (s *Server) officerOf(a agents.Agent) (officers.Officer, int, bool) {
	if s.officerRT == nil {
		return officers.Officer{}, 0, false
	}
	return s.officerRT.reg.ForAgentName(a.Name)
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
	return errOfficerConnector{agent: a.Name, officer: o.ID, connector: name, declared: o.Connectors}
}

// errOfficerConnector is officerConnectorRefusal's refusal. It is
// connectors.ErrInvalid (errors.Is), so the agent's request route answers 400
// and its connector_request tool shows it verbatim, as any invalid request.
type errOfficerConnector struct {
	agent, officer, connector string
	declared                  []string
}

func (e errOfficerConnector) Error() string {
	if len(e.declared) == 0 {
		return fmt.Sprintf("%s is an instance of officer %s, and an officer may use only the %q connector (to read its "+
			"repository); %q was refused", e.agent, e.officer, officers.GitHubConnector, e.connector)
	}
	return fmt.Sprintf("%s is an instance of officer %s, which may use only the %q connector and the connectors it "+
		"declares (%s); %q was refused", e.agent, e.officer, officers.GitHubConnector, strings.Join(e.declared, ", "), e.connector)
}

func (e errOfficerConnector) Is(target error) bool { return target == connectors.ErrInvalid }

// officerPrincipal is ctx acting as owner, through authz.ViaOfficer: how the
// wake loop reads and moves one officer's instance agents, so row-level
// security confines it to that owner's rows.
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
//     until an operator looks; any claim it held lapses with its lease.
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
		if bo, _, ok := f.s.officerRT.reg.ForAgentName(a.Name); ok && bo.ID == o.ID {
			out[a.Name] = a
		}
	}
	return out, nil
}

// Instances implements officers.Fleet. It FAILS BLIND: an unreadable agent
// table or provisioner is an error, never "nothing is up".
func (f officerFleet) Instances(ctx context.Context, o officers.Officer) ([]officers.InstanceState, error) {
	octx := f.s.officerPrincipal(ctx, o.Owner)
	rows, err := f.byName(octx, o)
	if err != nil {
		return nil, fmt.Errorf("officers: read agents: %w", err)
	}
	idx := f.s.instanceIndexWithin(octx, officerInstancesWithin)
	if idx == nil {
		return nil, errors.New("officers: the provisioner's instances could not be read")
	}
	out := make([]officers.InstanceState, 0, o.MaxInstances)
	for n := 1; n <= o.MaxInstances; n++ {
		name := officers.InstanceName(o.ID, n)
		a, ok := rows[name]
		out = append(out, officers.InstanceState{N: n, Name: name, Exists: ok, Up: ok && instanceUp(a, idx),
			Error:         ok && liveStatusIndexed(a, idx) == agents.StatusError,
			PromptPending: ok && promptPending(a)})
	}
	return out, nil
}

// Start implements officers.Fleet: create instance n with kickoff as its first
// turn, or — when its agent exists — re-arm its first turn and start it.
func (f officerFleet) Start(ctx context.Context, o officers.Officer, n int, kickoff string) error {
	octx := f.s.officerPrincipal(ctx, o.Owner)
	name := officers.InstanceName(o.ID, n)
	a, err := f.s.ext.Agents.GetByName(octx, name)
	if err != nil {
		_, err := f.s.createAndDispatchAgent(octx, dispatchParams{
			Name:        name,
			DisplayName: fmt.Sprintf("officer %s #%d", o.ID, n),
			Kind:        agents.KindClaudeCode,
			CCAccount:   o.CCAccount,
			CCFailover:  o.CCFailover,
			NoteText:    kickoff,
			Kickoff:     true,
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
// never landed and marked it `error` — and an instance in error holds its slot
// for ever (TestARepromptDuringARunningFirstTurnNeverStrandsTheInstance).
// Now the row is never written: no stuck verdict can be derived from it.
//
//   - The session claim (agents.Store.ClaimKickoff) is the one a first turn, a
//     memo and this hold for a whole turn. Lost = a turn is running:
//     officers.ErrInstanceBusy, nothing sent, and the loop tries next pass.
//   - A kickoff still owed (promptPending) is busy too: the first turn goes first.
//   - Won: the send runs in the background (a turn can outlast many passes),
//     holding the claim until it returns, then releases it; the turn is
//     recorded on the agent's session like any other.
func (f officerFleet) Reprompt(ctx context.Context, o officers.Officer, n int, text string) error {
	s := f.s
	if s.ext.Gateway == nil {
		return errors.New("officers: no agent gateway to send a re-prompt through")
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	name := officers.InstanceName(o.ID, n)
	a, err := s.ext.Agents.GetByName(octx, name)
	if err != nil {
		return err
	}
	if promptPending(a) {
		return officers.ErrInstanceBusy
	}
	turn := agents.KindTurnTimeout(a.Kind) + repromptSlack
	owner := s.officerRT.claimOwner
	won, err := s.ext.Agents.ClaimKickoff(octx, a.ID, owner, turn+repromptClaimMargin)
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
		tctx, cancel := context.WithTimeout(sendCtx, turn)
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
	if errors.Is(err, context.DeadlineExceeded) {
		return " (the turn outlasted its deadline)"
	}
	return " (failed; see the agent's transcript)"
}

// Stop implements officers.Fleet: scale instance n to zero (agent and volume
// kept). A claim it still holds is released by the loop's next pass, which
// sees its holder down.
func (f officerFleet) Stop(ctx context.Context, o officers.Officer, n int) error {
	octx := f.s.officerPrincipal(ctx, o.Owner)
	a, err := f.s.ext.Agents.GetByName(octx, officers.InstanceName(o.ID, n))
	if err != nil {
		return err
	}
	f.s.clearAgentWork(a.ID)
	return f.s.ext.Provisioner.Stop(octx, a.ID)
}

// WaitForOfficerReprompts blocks until every in-flight re-prompt send has
// ended and released its session claim. Called by the shutdown drain after
// EndStreams (which cancels them), and by tests.
func (s *Server) WaitForOfficerReprompts() {
	if s.officerRT != nil {
		s.officerRT.wg.Wait()
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
//     postgres_query when the officer declares `connectors`. Redundant
//     under bypass, and kept: it names exactly the muster tools the instance
//     is for, and holds if bypass is ever lifted.
//   - Model: the officer's configured model.
func (s *Server) OfficerClaudeCodeSession(a agents.Agent) (agentspec.ClaudeCodeSession, bool) {
	o, _, ok := s.officerOf(a)
	if !ok {
		return agentspec.ClaudeCodeSession{}, false
	}
	var allow []string
	for _, d := range officerToolDefs() {
		if officers.ToolOffered(o, d.Name) {
			allow = append(allow, d.Name)
		}
	}
	if s.ext.Connectors != nil {
		for _, d := range connectorToolDefs() {
			// postgres_query only for an officer that declares a connector it
			// could serve; every officer still reaches github.
			if d.Name == postgresQueryToolName && len(o.Connectors) == 0 {
				continue
			}
			allow = append(allow, d.Name)
		}
	}
	return agentspec.ClaudeCodeSession{AllowMCPTools: allow, Model: o.Model, BypassPermissions: true}, true
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
		_, _, officer := s.officerOf(a)
		return officer
	}
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
	return append(queueToolDefs(), shipToolDefs()...)
}

// queueToolDefs are every officer's queue tools (and the release officer's
// captain tool).
func queueToolDefs() []agents.ToolDef {
	item := strProp("The item's id, as officer_items or officer_brief returned it.")
	lease := intProp(fmt.Sprintf("How long the lease runs, in seconds (default %d, %d to %d).", officers.DefaultLeaseSeconds, officers.MinLeaseSeconds, officers.MaxLeaseSeconds),
		officers.MinLeaseSeconds, officers.MaxLeaseSeconds)
	return []agents.ToolDef{
		{Type: "function", Name: officers.ToolBrief,
			Description: "Load your officer's brief from HQ: your charter (your standing instructions, versioned), your open items, recent outcomes and your memory scopes. Call it first when you wake, instead of looking for an earlier conversation.",
			Parameters:  objSchema(map[string]any{})},
		{Type: "function", Name: officers.ToolItems,
			Description: "List your officer's items in HQ. status: open (default: what you can claim), claimed, done or escalated.",
			Parameters: objSchema(map[string]any{
				"status": map[string]any{"type": "string", "enum": []string{officers.StatusOpen, officers.StatusClaimed, officers.StatusDone, officers.StatusEscalated}},
				"limit":  intProp("At most this many (default HQ's).", 1, 100),
				"cursor": strProp("The `next` value of the previous page (HQ returns it with each page; null on the last)."),
			})},
		{Type: "function", Name: officers.ToolClaim,
			Description: "Claim one item so no other instance works it. If YOU already hold it (e.g. you were restarted mid-item), this succeeds, renews your lease and says \"already yours\" — carry on with it. A conflict answer (refused, status 409) means ANOTHER instance holds it: pick another item. Heartbeat before the lease runs out.",
			Parameters:  objSchema(map[string]any{"item_id": item, "lease_seconds": lease}, "item_id")},
		{Type: "function", Name: officers.ToolHeartbeat,
			Description: "Extend your lease on an item you hold. A conflict answer means you lost the lease: stop working the item.",
			Parameters:  objSchema(map[string]any{"item_id": item, "lease_seconds": lease}, "item_id")},
		{Type: "function", Name: officers.ToolComplete,
			Description: "Finish an item you hold: outcome done, escalated (a human must act; say who and why in the summary) or no_action, with a summary (at most 24,576 characters — " +
				"in shadow mode your full evaluation or report goes here) and optional links.",
			Parameters: objSchema(map[string]any{
				"item_id": item,
				"outcome": map[string]any{"type": "string", "enum": officers.Outcomes},
				"summary": strProp("What you found and did, at most 24,576 characters."),
				"links":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": officers.MaxLinks, "description": "Optional http(s) URLs (a PR, a dashboard, a report), at most 10."},
			}, "item_id", "outcome", "summary")},
		{Type: "function", Name: officers.ToolRelease,
			Description: "Give an item back before its lease ends, so another instance can take it.",
			Parameters:  objSchema(map[string]any{"item_id": item}, "item_id")},
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
	}
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
	o, _, ok := s.officerOf(a)
	if !ok {
		return toolErr("%s is available to officer instances only", name), true
	}
	if !officers.ToolOffered(o, name) {
		return toolErr("%s is not one of officer %s's tools", name, o.ID), true
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &raw); err != nil {
		return toolErr("arguments must be a JSON object"), true
	}
	// 🔴 NAMING ANOTHER OFFICER IS REFUSED, NOT IGNORED. No schema offers an
	// officer argument, but a runtime can send any keys; silently acting for
	// the caller's own officer would hide an attempt to act for another.
	for _, k := range []string{"officer", "officer_id"} {
		if v, ok := raw[k]; ok {
			var named string
			_ = json.Unmarshal(v, &named)
			if named != o.ID {
				return toolErr("you act for officer %s only; %s cannot name another officer", o.ID, name), true
			}
		}
	}
	if isShipTool(name) {
		return s.dispatchShipTool(ctx, a, o, name, arguments), true
	}
	c := s.officerRT.hq.Client(o)
	instance := a.Name
	var in struct {
		ItemID       officers.ItemID `json:"item_id"`
		LeaseSeconds int             `json:"lease_seconds"`
		Status       string          `json:"status"`
		Limit        int             `json:"limit"`
		Cursor       string          `json:"cursor"`
		Outcome      string          `json:"outcome"`
		Summary      string          `json:"summary"`
		Links        []string        `json:"links"`
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
		body, err = c.Brief(ctx)
	case officers.ToolItems:
		status := in.Status
		if status == "" {
			status = officers.StatusOpen
		}
		var page officers.ItemsPage
		page, err = c.Items(ctx, officers.ItemsQuery{Status: status, Limit: in.Limit, Cursor: in.Cursor})
		body = page.Raw
	case officers.ToolClaim:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		body, err = s.officerClaim(ctx, c, in.ItemID, instance, in.LeaseSeconds)
		if err == nil && s.officerRT.fwd != nil {
			s.officerRT.fwd.Claimed(o, instance, in.ItemID)
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
		ok := false
		for _, v := range officers.Outcomes {
			ok = ok || v == in.Outcome
		}
		if !ok {
			return toolErr("outcome must be one of %s", strings.Join(officers.Outcomes, ", ")), true
		}
		if strings.TrimSpace(in.Summary) == "" || utf8.RuneCountInString(in.Summary) > officers.MaxSummaryChars {
			return toolErr("summary is required and at most %d characters", officers.MaxSummaryChars), true
		}
		if len(in.Links) > officers.MaxLinks {
			return toolErr("at most %d links", officers.MaxLinks), true
		}
		body, err = c.Complete(ctx, in.ItemID, instance, officers.Completion{Outcome: in.Outcome, Summary: in.Summary, Links: in.Links})
		if err == nil && s.officerRT.fwd != nil {
			s.officerRT.fwd.Ended(o, instance, in.ItemID)
		}
	case officers.ToolRelease:
		if bad := needItem(); bad != "" {
			return bad, true
		}
		body, err = c.Release(ctx, in.ItemID, instance)
		if err == nil && s.officerRT.fwd != nil {
			s.officerRT.fwd.Ended(o, instance, in.ItemID)
		}
	case officers.ToolReleaseEvaluation:
		post, perr := officers.ParseCaptainPost([]byte(orEmptyJSON(arguments)))
		if perr != nil {
			return toolErr("%v", perr), true
		}
		body, err = c.ReleaseCaptain(ctx, instance, post)
	}
	return officerToolResult(name, body, err), true
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
func (s *Server) officerClaim(ctx context.Context, c *officers.Client, item officers.ItemID, instance string, lease int) ([]byte, error) {
	body, err := c.Claim(ctx, item, instance, lease)
	var hqErr *officers.HQError
	if err == nil || !errors.As(err, &hqErr) || hqErr.Status != http.StatusConflict {
		return body, err
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
		return c.Claim(ctx, item, instance, lease)
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

// officerToolResult is HQ's answer as a tool result: HQ's JSON on success; a
// structured refusal carrying HQ's status and body on a 4xx (a 409 on a claim
// names the holder, which is what the instance needs); a plain retry-later on
// an outage.
func officerToolResult(name string, body []byte, err error) string {
	var hqErr *officers.HQError
	switch {
	case err == nil:
		if !json.Valid(body) {
			return toolErr("HQ answered %s with something that is not JSON", name)
		}
		return string(body)
	case errors.As(err, &hqErr):
		refused := map[string]any{"status": hqErr.Status}
		if json.Valid(hqErr.Body) {
			refused["hq"] = json.RawMessage(hqErr.Body)
		}
		return toolJSON(map[string]any{"error": fmt.Sprintf("HQ refused %s (%d)", name, hqErr.Status), "refused": refused})
	case errors.Is(err, officers.ErrHQUnavailable):
		return toolErr("HQ is unavailable right now; try %s again in a minute", name)
	default:
		return toolErr("%s failed", name)
	}
}
