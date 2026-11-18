package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE COMPOSE CHIEF (MUSTER_CHIEF_RUNTIME=compose)
//
// On the docker compose install the chief runs in the Compose file's `chief`
// container, which muster configures through a shared volume (the compose
// provisioner driver; cmd/muster-server composechief.go). That provisioner
// runs ONE instance, the admin's chief, so:
//
//   - lifecycle is wired for the chief (the Account switch, Start/Stop, the
//     chief's restart) — lifecycleServed;
//   - nothing DISPATCHES: POST /agents, the sidebar's "+", a task card's
//     Dispatch button and agent_dispatch's targets read dispatchServed, which
//     is false here (requireDispatchServed answers POST /agents);
//   - only the admin (user 1) has a chief; anyone else's Account page says why
//     (chiefToggleAvailable);
//   - `chief replace` is refused: there is one kind here and nothing to
//     replace the chief with (handleChiefReplace);
//   - the chief's kind is claude-code and its Claude account is
//     ComposeChiefAccount, whatever credential it runs on (the database
//     requires a claude-code row to name one, migration 0003). Which credential
//     — a Claude subscription token or the owner's OpenRouter key through the
//     LLM proxy — is decided by [ComposeChief.Credential], one rule handed to
//     this package and to the spec builder alike.
// ---------------------------------------------------------------------------

// OpenRouterKeySettingsURL is where an OpenRouter key's credit limit is set.
// The Compose chief on OpenRouter has no muster budget: the limit on the key is
// its spending cap.
const OpenRouterKeySettingsURL = "https://openrouter.ai/settings/keys"

// ComposeChiefAccount is the Claude account the Compose chief's row names. It
// holds MUSTER_CHIEF_CLAUDE_TOKEN when that is set; otherwise the account has
// no token and the chief runs on the LLM route.
const ComposeChiefAccount = "chief"

// ComposeChiefCred is which credential the Compose chief runs on.
type ComposeChiefCred string

const (
	// ComposeChiefClaude: the Claude subscription token, MUSTER_CHIEF_CLAUDE_TOKEN.
	ComposeChiefClaude ComposeChiefCred = "claude"
	// ComposeChiefOpenRouter: the owner's OpenRouter key, through muster's LLM
	// proxy (the claude-code CLI on CCD_LLM_ROUTE=muster).
	ComposeChiefOpenRouter ComposeChiefCred = "openrouter"
)

// ErrComposeChiefNoCredential is [ComposeChief.Credential]'s answer when the
// chief has neither credential.
var ErrComposeChiefNoCredential = errors.New("the chief has no credential")

// ComposeChiefNeedsCredential is what the operator is told to add.
const ComposeChiefNeedsCredential = "set MUSTER_CHIEF_CLAUDE_TOKEN (a Claude subscription token from `claude setup-token`) " +
	"or MUSTER_AGENT_OPENROUTER_KEY in .env, then run `docker compose up -d --wait`, which restarts muster"

// ComposeDispatchRefusal is POST /agents' answer on the Compose chief.
const ComposeDispatchRefusal = "this install runs the chief in its own Compose container and dispatches no other agent: " +
	"agent dispatch needs muster deployed in Kubernetes (docs/features.md#dispatch)"

// ComposeChiefReplaceRefusal is POST /api/chief/replace's answer on the
// Compose chief.
const ComposeChiefReplaceRefusal = "the Compose chief cannot be replaced: this install runs one chief, on one kind " +
	"(claude-code), in its Compose container. Change its credential in .env instead, then run " +
	"`docker compose up -d --wait`: that restarts muster, and muster then re-applies a running chief on the new " +
	"credential (docs/chief.md)"

// ComposeChiefNotYours is the Account page's text for a user who is not the
// admin on a Compose install.
const ComposeChiefNotYours = "Not available to you on this install. It runs one chief, in its own Compose " +
	"container, and that chief is the admin's. Everything else works without it."

// ComposeChiefNoTerminal is the Terminal view's text on the Compose chief:
// muster reaches its container over HTTP only (there is no exec without the
// Docker socket, and the compose driver declares none).
const ComposeChiefNoTerminal = "the Compose chief's terminal is not reachable through muster: there is no " +
	"exec into its container without the Docker socket. Its output is in `docker compose logs chief` on the server"

// ComposeChief is Extensions.ComposeChief: the Compose chief's settings.
type ComposeChief struct {
	// Credential is which credential owner's chief runs on, or
	// ErrComposeChiefNoCredential naming what to add. Required.
	Credential func(ctx context.Context, owner int64) (ComposeChiefCred, error)
	// OpenRouterKey is owner's OpenRouter key from the credential plane
	// (credentials.Plane.OpenRouterKey, the plane the Account page and
	// Credential read). The LLM proxy serves the chief on it. Required.
	OpenRouterKey func(ctx context.Context, owner int64) (string, error)
	// Spec reads the chief's spec on the volume: its generation (whether the
	// boot re-apply restarted the chief) and its replicas (whether it is live:
	// the re-apply never starts a stopped spec). Nil: nothing is re-applied.
	Spec func() (generation int64, replicas int, err error)
	// StartIfLive is the provisioner's Start, made only when live says so, the
	// check and the start under the agent's lifecycle lock (agentprovision
	// Adapter.StartIfLive): a Stop from the agent page during boot is never
	// undone by the boot re-apply. Nil: nothing is re-applied.
	StartIfLive func(ctx context.Context, agentID int64, live func() (bool, error)) (bool, error)
}

// composeReapplyTimeout bounds the boot re-apply as a whole (its Start has
// the adapter's own operation budget inside it).
const composeReapplyTimeout = 2 * time.Minute

// ReapplyComposeChiefAtBoot re-applies the admin's RUNNING Compose chief
// through the provisioner's Start, in the background, and logs the outcome on
// one "chief runtime:" line. The returned channel is closed when it is done.
//
// 🔴 WHY: the credential rule ([ComposeChief.Credential]) reads the current
// config on every call, but the chief's spec (the CLI's environment: its
// CLAUDE_CODE_OAUTH_TOKEN or its LLM route) is built only when the chief
// starts. An admin who changes the credential in .env and runs `docker
// compose up -d --wait` restarts muster, not the chief container (its own
// environment does not change). Without this the running chief kept its old
// spec: Account named one credential while the CLI used the other, and on
// OpenRouter -> token the LLM proxy refused every turn (audit #469 F3).
//
// Start rebuilds the spec from the current config, and the compose driver
// moves the generation (the supervisor's restart signal) only when the spec's
// fingerprint changed: an unchanged config is a no-op, a changed one restarts
// the chief on the new credential.
//
// 🔴 IT NEVER STARTS A CHIEF THAT IS OFF, on either axis:
//   - the admin's chief switch must be on (chiefOn: a stored Off wins);
//   - its spec must be live (spec.json's replicas are 1; never the instance's
//     phase, which reads pending or failed for a spec that is stopping). A
//     chief stopped from its agent page, or never applied, stays as it is.
//
// It runs under the admin's switch lock, so a click on Account → Chief during
// boot is ordered with it, and the spec check and the Start run under the
// agent's own lifecycle lock (StartIfLive), so a Stop from the agent page
// during boot is never undone.
//
// 🔴 A RUNNING CHIEF WITH NO CREDENTIAL LEFT IS STOPPED (Provisioner.Stop): the
// admin withdrew it, and agentprovision.Start refuses to revive any agent on a
// withdrawn credential. Keeping the spec would leave the CLI on the deleted
// token while Account says "No credential yet".
func (s *Server) ReapplyComposeChiefAtBoot(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), composeReapplyTimeout)
	safeGo(s.logger, "compose chief re-apply", func() {
		defer close(done)
		defer cancel()
		line, err := s.reapplyComposeChief(ctx)
		if err != nil {
			s.logger.Printf("chief runtime: could not re-apply the running chief's spec at boot: %v (it keeps the spec it had)", err)
			return
		}
		s.logger.Printf("chief runtime: %s", line)
	})
	return done
}

// reapplyComposeChief is ReapplyComposeChiefAtBoot's work: the boot line's
// text, or why it could not decide.
func (s *Server) reapplyComposeChief(ctx context.Context) (string, error) {
	if s.ext.ComposeChief == nil || !s.lifecycleServed() || s.ext.Agents == nil {
		return "no Compose chief to re-apply at boot", nil
	}
	owner := authz.OperatorUserID
	ctx = authz.Operator(ctx)
	mu := s.chiefSwitchLock(owner)
	mu.Lock()
	defer mu.Unlock()
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return "", fmt.Errorf("list the agents: %w", err)
	}
	// The reserved slug: the one name the compose driver admits.
	name := agents.ChiefNameFor(owner)
	var chief agents.Agent
	var has bool
	for _, a := range ownedBy(list, owner) {
		if a.Name == name {
			chief, has = a, true
		}
	}
	if !has {
		return "the admin has no chief agent: nothing to re-apply at boot", nil
	}
	on, err := s.chiefOn(ctx, owner, list)
	if err != nil {
		return "", fmt.Errorf("read the chief switch: %w", err)
	}
	if !on {
		return "the chief is turned off: its spec was not re-applied at boot", nil
	}
	// 🔴 LIVE IS THE SPEC'S REPLICAS, NOT THE INSTANCE'S PHASE. A stopped spec
	// whose container is still stopping, or whose stop left a process running,
	// reads pending or failed (the driver asks the supervisor at replicas 0),
	// and a Start here would turn it back on.
	if s.ext.ComposeChief.Spec == nil || s.ext.ComposeChief.StartIfLive == nil {
		return "the chief's spec cannot be read: it was not re-applied at boot", nil
	}
	live := func() (bool, error) {
		_, replicas, err := s.ext.ComposeChief.Spec()
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read the chief's spec: %w", err)
		}
		return replicas > 0, nil
	}
	if ok, err := live(); err != nil {
		return "", err
	} else if !ok {
		return "the chief's spec is stopped: it was not re-applied at boot", nil
	}
	cred, err := s.ext.ComposeChief.Credential(ctx, owner)
	// 🔴 NO CREDENTIAL LEFT STOPS IT. The admin deleted the chief's credential
	// from .env: re-applying is refused, and keeping the spec would leave the
	// CLI running on the credential they withdrew while Account says "No
	// credential yet". agentprovision.Start refuses to revive any agent on a
	// withdrawn credential; a running one is stopped here, the same rule.
	if errors.Is(err, ErrComposeChiefNoCredential) {
		if err := s.ext.Provisioner.Stop(ctx, chief.ID); err != nil {
			return "", fmt.Errorf("stop the chief, whose credential was removed: %w", err)
		}
		return "stopped the running chief at boot: no credential is left in .env (" + ComposeChiefNeedsCredential + ")", nil
	}
	if err != nil {
		return "", fmt.Errorf("the chief's credential: %w", err)
	}
	gen := func() (int64, bool) {
		g, _, err := s.ext.ComposeChief.Spec()
		return g, err == nil
	}
	before, okBefore := gen()
	started, err := s.ext.ComposeChief.StartIfLive(ctx, chief.ID, live)
	if err != nil {
		return "", err
	}
	if !started {
		return "the chief was stopped while muster booted: it was not re-applied", nil
	}
	after, okAfter := gen()
	label := composeChiefCredLabel(cred)
	if okBefore && okAfter && before == after {
		return "the running chief's spec is unchanged at boot, nothing to re-apply (credential: " + label + ")", nil
	}
	return "re-applied the running chief's spec at boot (credential: " + label + ")", nil
}

// composeChiefCredLabel is cred as the boot line names it.
func composeChiefCredLabel(cred ComposeChiefCred) string {
	switch cred {
	case ComposeChiefClaude:
		return "the Claude subscription, MUSTER_CHIEF_CLAUDE_TOKEN"
	case ComposeChiefOpenRouter:
		return "the OpenRouter key, through the LLM proxy"
	}
	return string(cred)
}

// ComposeChiefOnSubscription is the LLM proxy's refusal to a Compose chief
// that the credential rule puts on its Claude subscription.
const ComposeChiefOnSubscription = "this chief runs on its Claude subscription (MUSTER_CHIEF_CLAUDE_TOKEN), not on the " +
	"server's OpenRouter key: the LLM proxy does not serve it, and nothing was sent"

// composeChiefLLMKey is the key serveChiefLLM serves the Compose chief a on,
// asked of the same rule the spec builder and the Account page ask
// ([ComposeChief.Credential]), so the proxy cannot serve a chief that rule put
// on its subscription:
//
//   - the rule says OpenRouter: the owner's key, read from the credential plane;
//   - the rule says the subscription: refused (403), nothing sent;
//   - the rule finds no credential: "" (serveChiefLLM's no-key refusal);
//   - any read error: refused (503), nothing sent.
//
// ok is false when the refusal was written to w.
func (s *Server) composeChiefLLMKey(w http.ResponseWriter, ctx context.Context, a agents.Agent) (key string, ok bool) {
	owner := agentOwnerID(a)
	cred, err := s.ext.ComposeChief.Credential(ctx, owner)
	switch {
	case errors.Is(err, ErrComposeChiefNoCredential):
		return "", true
	case err != nil:
		metrics.OfficerLLMRefused.WithLabelValues(chiefLLMLabel, "credential_error").Inc()
		s.logger.Printf("llm proxy: refused the chief agent %d (%q): reading its credential: %v (nothing sent)", a.ID, a.Name, err)
		llmproxy.Refuse(w, http.StatusServiceUnavailable, "api_error", "the chief's credential could not be read; nothing was sent")
		return "", false
	case cred != ComposeChiefOpenRouter:
		metrics.OfficerLLMRefused.WithLabelValues(chiefLLMLabel, "subscription").Inc()
		s.logger.Printf("llm proxy: refused the chief agent %d (%q): it runs on its Claude subscription (nothing sent)", a.ID, a.Name)
		llmproxy.Refuse(w, http.StatusForbidden, "permission_error", ComposeChiefOnSubscription)
		return "", false
	}
	key, err = s.ext.ComposeChief.OpenRouterKey(ctx, owner)
	if err != nil {
		metrics.OfficerLLMRefused.WithLabelValues(chiefLLMLabel, "credential_error").Inc()
		s.logger.Printf("llm proxy: refused the chief agent %d (%q): reading its OpenRouter key: %v (nothing sent)", a.ID, a.Name, err)
		llmproxy.Refuse(w, http.StatusServiceUnavailable, "api_error", "the chief's OpenRouter key could not be read; nothing was sent")
		return "", false
	}
	return strings.TrimSpace(key), true
}

// composeChiefOwner reports whether owner is the one user a Compose install
// runs a chief for: the operator, whose chief is agents.ChiefName — the one
// name the compose driver admits.
func composeChiefOwner(owner int64) bool { return owner == authz.OperatorUserID }

// composeChiefKindAccount is resolveKindAccount on the Compose chief: the one
// agent it creates is the admin's chief, on claude-code and
// ComposeChiefAccount, and only when Credential has something to run it on.
func (s *Server) composeChiefKindAccount(ctx context.Context, p dispatchParams) (string, string, error) {
	owner := callerID(ctx)
	if !composeChiefOwner(owner) || p.Name != agents.ChiefName {
		return "", "", fmt.Errorf("%w: %s", ErrDispatchRefused, ComposeDispatchRefusal)
	}
	if k := agents.ResolveKind(p.Kind); p.Kind != "" && k != agents.KindClaudeCode {
		return "", "", fmt.Errorf("%w: the Compose chief runs on %s, not %s", ErrDispatchRefused, agents.KindClaudeCode, k)
	}
	if p.CCAccount != "" || p.Model != "" || p.WorkerPool != "" || p.Repo != "" {
		return "", "", fmt.Errorf("%w: the Compose chief takes no account, model, worker pool or repository", ErrDispatchRefused)
	}
	if _, err := s.ext.ComposeChief.Credential(ctx, owner); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrDispatchRefused, err)
	}
	return agents.KindClaudeCode, ComposeChiefAccount, nil
}

// ChiefNotSetUp is the Account page's text where the chief cannot run: no
// Compose chief runtime and no agent provisioner. It is the enable step.
const ChiefNotSetUp = "Not available on this install yet. To turn it on, add COMPOSE_PROFILES=chief, " +
	"MUSTER_CHIEF_RUNTIME=compose and MUSTER_CONNECTOR_ENCRYPTION_KEY to .env, run docker compose up -d --wait, " +
	"and come back here (docs/features.md#chief). Everything else works without it."

// The Account page's lines about the Compose chief, below its switch.
const (
	ComposeChiefRunsOnClaude     = "Runs on your Claude subscription (MUSTER_CHIEF_CLAUDE_TOKEN)."
	ComposeChiefNetworkNote      = "Its network is not confined: the chief's container reaches the internet, and not muster's database (docs/chief.md)."
	ComposeChiefRoutingOn        = "Routing work to your sessions: on."
	ComposeChiefRoutingOff       = "Routing work to your sessions: off. Add MUSTER_SESSION_ROUTING=1 to .env and run docker compose up -d --wait."
	composeChiefInstanceDeadline = 3 * time.Second
)

// ComposeChiefStopFailed is the container line of a chief turned off whose
// container says the stop left something running.
func ComposeChiefStopFailed(reason string) string {
	return "Off, but its container did not stop: " + reason + ". It keeps retrying; " +
		"docker compose restart chief ends everything in it."
}

// ComposeChiefRunsOnOpenRouter is the credential line for the OpenRouter key.
func ComposeChiefRunsOnOpenRouter(model string) string {
	return "Runs on your OpenRouter key, on " + model + " (MUSTER_CHIEF_LLM_MODEL). muster adds no budget of its own: " +
		"set a credit limit on the key itself, at " + OpenRouterKeySettingsURL + "."
}

// composeChiefDetails is what the admin's Account page says about the Compose
// chief beside its switch: which credential it runs on (and, on OpenRouter,
// that the key's credit limit is the only spending cap), whether its container
// is answering when it is on, that its network is not confined, and whether
// routing to sessions is on.
func (s *Server) composeChiefDetails(ctx context.Context, owner int64, chief agents.Agent, has, on bool) string {
	p := func(id, text string) string {
		return `<p data-compose-chief="` + id + `" class="text-sm text-fg2">` + html.EscapeString(text) + `</p>
`
	}
	var b string
	switch cred, err := s.ext.ComposeChief.Credential(ctx, owner); {
	case err != nil:
		b += p("credential", "No credential yet: "+ComposeChiefNeedsCredential+".")
	case cred == ComposeChiefOpenRouter:
		model := "(the LLM proxy is not enabled)"
		if s.chiefLLM != nil {
			model = s.chiefLLM.model
		}
		b += p("credential", ComposeChiefRunsOnOpenRouter(model))
	default:
		b += p("credential", ComposeChiefRunsOnClaude)
	}
	if has {
		if inst, ok := s.instanceIndexWithin(ctx, composeChiefInstanceDeadline)[chief.Name]; ok && inst != nil {
			switch {
			case on && inst.Phase != provision.PhaseRunning && inst.Reason != "":
				b += p("container", "Its container: "+inst.Reason+".")
			// 🔴 OFF, BUT THE STOP LEFT SOMETHING RUNNING. The switch reads off and
			// the chief gets no work, but the supervisor says a process outlived
			// the stop (internal/provision/compose: replicas 0 reads its status;
			// ccd compose reports error, never off, while one lives).
			case !on && inst.Phase == provision.PhaseFailed && inst.Reason != "":
				b += p("container", ComposeChiefStopFailed(inst.Reason))
			}
		}
	}
	b += p("network", ComposeChiefNetworkNote)
	// route_to_session's own gate (agent.go): session routing, which needs no
	// plugins since an owner's request and a task are routed without one.
	if routesOn(s.ext) {
		b += p("routing", ComposeChiefRoutingOn)
	} else {
		b += p("routing", ComposeChiefRoutingOff)
	}
	return b
}
