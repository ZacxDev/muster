package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/metrics"
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
	"or MUSTER_AGENT_OPENROUTER_KEY in .env, then run `docker compose up -d --wait`"

// ComposeDispatchRefusal is POST /agents' answer on the Compose chief.
const ComposeDispatchRefusal = "this install runs the chief in its own Compose container and dispatches no other agent: " +
	"agent dispatch needs muster deployed in Kubernetes (docs/features.md#dispatch)"

// ComposeChiefReplaceRefusal is POST /api/chief/replace's answer on the
// Compose chief.
const ComposeChiefReplaceRefusal = "the Compose chief cannot be replaced: this install runs one chief, on one kind " +
	"(claude-code), in its Compose container. Change its credential in .env instead (docs/chief.md)"

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
