package agents

import (
	"errors"
	"fmt"
	"strings"
)

// ErrModelNotCredentialed is what every ValidateModelCredential refusal wraps, so
// an HTTP handler can answer 400 (the operator typed a bad slug) instead of 500
// (the server broke). Without it a refusal is indistinguishable from a database
// failure at the call site, and the operator gets "could not create agent" with
// the corrected slug hidden in a server log.
var ErrModelNotCredentialed = errors.New("model provider has no credential in the agent")

// credentialedAuthProvider is THE provider this service bootstraps an auth
// profile for — the single source of truth for BOTH halves of a seam that used
// to be two independent decisions.
//
// 🔴 THE SEAM THIS CLOSES. An agent's PRIMARY MODEL and its CREDENTIAL SET were
// chosen about two lines apart in the deployment renderer and never compared:
//
//	auth:  {provider: "openrouter"}   // ← which credential the agent is given
//	model: {primary: <chosen slug>}   // ← which model it is told to run
//
// The renderer turns the FIRST into exactly one auth profile, keyed from that
// provider's API-key environment variable. The runtime resolves the provider
// for the SECOND from the model slug's PREFIX, not from that value — it splits
// on the first "/" and takes the left half. So any primary slug not routed
// through the credentialed provider asks the runtime for a provider whose key
// the agent does not have.
//
// 🔴 IT FAILS SILENTLY, WHICH IS WHY A GUARD AND NOT A DOC. Measured on a live
// agent provisioned with `z-ai/glm-5.3-flash` (the routing provider's own model
// id, copied without the routing prefix): 493 attempts on `zai/glm-5.3-flash`,
// 493 `FailoverError: No API key found for provider "zai"`, 493 silent
// falls-back to a different model, about 11 s of failover burned on EVERY turn.
// The agent answered — on the wrong model — so nothing at the operator's end
// said anything was wrong.
//
// ⚠ WIDENING THIS WIDENS THE GUARD AUTOMATICALLY. If this service ever ships a
// second provider credential, the fix is to make this a SET and have the
// deployment renderer and ValidateModelCredential read that same set — never to
// add a second, independent list. The whole defect was two independent choices.
const credentialedAuthProvider = "openrouter"

// ModelProvider reports the provider the agent runtime will resolve a model slug
// to, and whether that provider is determinable FROM THE SLUG ALONE.
//
// It mirrors the runtime's own model-reference parser, read out of the live
// agent image rather than out of its documentation: trim, split on the FIRST
// "/", provider = the left half, lowercased.
//
// ⚠ ok=false for a slug with NO "/", and that is a deliberate limit, not an
// oversight. That parser falls back to a default-provider argument in that case
// — a value resolved inside the agent process from its own config, which this
// service cannot see at provision time. Judging such a slug would be a guess,
// and a guessed refusal is worse than no refusal. Slugs WITH a "/" — which is
// every slug this service's own model picker and ModelOptions produce, and the
// shape the live defect had — are decided exactly.
//
// ⚠ NO ALIAS TABLE, ON PURPOSE. The runtime also normalises provider aliases
// (z-ai and z.ai both become zai, and there are a dozen more). Reproducing that
// table here would be a second copy of someone else's data, free to drift — and
// it would change NOTHING: not one alias in that table maps to the credentialed
// provider, so normalisation cannot flip this function's verdict. The cost is
// cosmetic: the message below names the prefix as the operator typed it
// ("z-ai") while the agent's log names the normalised id ("zai"). That is a
// fair trade for a table that cannot go stale.
func ModelProvider(slug string) (string, bool) {
	s := strings.TrimSpace(slug)
	i := strings.Index(s, "/")
	if i <= 0 {
		return "", false
	}
	p := strings.TrimSpace(s[:i])
	if p == "" || strings.TrimSpace(s[i+1:]) == "" {
		return "", false
	}
	return strings.ToLower(p), true
}

// ValidateModelCredential refuses a primary model slug whose provider this
// service will not have credentials for inside the agent.
//
// Empty is always fine: it means "use the cluster default", which the
// provisioner supplies and which this same guard covers wherever an operator can
// set it.
//
// 🔴 IT REFUSES RATHER THAN WARNS. A warning reproduces the bug it is warning
// about — the whole failure mode is that nobody reads the pod log. Refusing at
// the moment the operator types the slug turns a fleet agent that has "never run
// on its configured model" into a 400 with the corrected slug in it.
//
// ⚠ WHAT IT CANNOT SEE. (a) A slug with no "/" — see ModelProvider. (b) The
// runtime can also resolve a provider key from the ENVIRONMENT, so an
// environment variable could in principle credential a second provider.
// Measured on a live agent, the only credential in its environment was the one
// this service puts there, and no privilege profile in this tree ships a
// provider key — so the agent's credential set is exactly the one profile the
// renderer writes. If that changes, widen credentialedAuthProvider (see its
// comment), not this function. (c) Opaque runtime configuration could define a
// custom provider id; this service sets none today.
func ValidateModelCredential(slug string) error {
	provider, ok := ModelProvider(slug)
	if !ok || provider == credentialedAuthProvider {
		return nil
	}
	return fmt.Errorf(
		"%w: model %q routes to provider %q, but an agent is only credentialed for %q: "+
			"it is given exactly one auth profile (%s:default), so every turn would fail "+
			"auth and silently fall back to the fallback model. Did you mean %q?",
		ErrModelNotCredentialed,
		strings.TrimSpace(slug), provider, credentialedAuthProvider,
		credentialedAuthProvider, credentialedAuthProvider+"/"+strings.TrimSpace(slug))
}

// ---------------------------------------------------------------------------
// 🔴 THE OTHER SIDE OF THIS SEAM WAS NOT CARVED, AND THIS IS THE RECORD OF IT.
//
// credentialedAuthProvider exists to be read by TWO callers: this guard, which
// refuses a slug the agent cannot authenticate, and the deployment renderer,
// which writes the auth profile the agent actually gets. Upstream a test pinned
// that RELATIONSHIP — it rendered the deployment's values and asserted the
// provider written into them equalled this constant, so pointing one at a
// different provider failed a test rather than shipping agents that fail auth
// on every turn.
//
// That renderer did not come across. It built a values map for a vendored chart,
// and this repository provisions through internal/provision instead — which
// renders its own objects and carries an opaque Spec.Config for whatever runtime
// is attached. So the constant currently has ONE reader.
//
// ⚠ OWED, AND NAMED SO IT IS NOT AN OBJECT NOBODY CAN CLOSE: while there is one
// reader, this guard is a claim about a credential set NOTHING in this
// repository establishes. modelauth_test.go says so at the test that used to be
// the seam guard, so a reader cannot mistake the surviving half for the whole.
// CLOSING CONDITION: the pull request that teaches muster to build an agent's
// provision.Spec must read credentialedAuthProvider when it sets the runtime's
// auth profile, and must restore the equality assertion between the two sides.
// WHO CHECKS IT: the reviewer of that pull request, against this comment.
// ---------------------------------------------------------------------------
