package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/privilege"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/runbooks"
)

// Extensions are the server's optional dependencies. A Server built without
// them still boots, serves the shell, answers /health and /readyz, and refuses
// every data route with a reason — which is the behaviour a first run against
// no database should have.
//
// 🔴 THIS STRUCT IS THE muster HALF OF A STRUCT THAT USED TO HOLD EIGHTEEN
// FIELDS, AND THE NINE THAT ARE MISSING ARE MISSING ON PURPOSE. Upstream it
// also carried Suggest, Attention, Tmux, Transcripts, TranscriptArchive,
// TermWrite, Layout, AutoApprove and GenSuggestions — every one of which backs
// a surface on the permission router's side of the route partition. Carrying
// them across would give this service nine dependencies it has no route for,
// and would make the extraction's central claim — that the two halves are
// separable — false in the type system while looking true in the diff.
//
// 🔴 `Suggest` IS THE ONE WHOSE ABSENCE IS LOAD-BEARING RATHER THAN TIDY, AND
// IT MUST NOT COME BACK. It is the store that owns the session table, and the
// whole of the upstream work that preceded this carve existed to remove that
// table from the domain layer so the domain layer could move without it. The
// bit it used to supply — SessionLink.DetailAvailable — is now composed from a
// SEAM instead; see UseExtensions and session_liveness.go. A reviewer who sees
// a `Suggest` field reappear here should read that as the extraction being
// undone, not as a convenience.
type Extensions struct {
	Notes     notes.Store
	Agents    agents.Store
	GitHub    github.Store
	Privilege privilege.Store
	Runbooks  runbooks.Store

	// SessionLiveness answers "does a transcript record for this session still
	// exist" — the one bit SessionLink.DetailAvailable is made of.
	//
	// 🔴 IT IS A PORT, NOT A STORE, BECAUSE THE DATA IS NOT THIS SERVICE'S. The
	// session table stays with the permission router. Nil is TOLERATED and is not
	// a supported production state: the wrapper still applies, but every link
	// resolves to false and the three-state transcript row collapses to two. See
	// withSessionLiveness for why that is tolerated rather than fatal, and
	// TestExtensionsWiringPairsNotesWithALivenessProbe for what stops it shipping.
	SessionLiveness SessionLivenessProbe

	// PrivilegeApply applies/removes a granted profile's Kubernetes RBAC live.
	// Nil (no in-cluster client) means grants are recorded but not applied — the
	// handlers log and carry on, mirroring a nil Provisioner.
	PrivilegeApply PrivilegeApplier

	// TagAutoDispatch arms the `auto:dispatch` routing tag
	// (MUSTER_TAG_AUTODISPATCH). It ships FALSE and implements PLUMBING ONLY
	// (recognition + eligibility + a log line + the metric) — no hands-off
	// dispatch behaviour. Rationale: the MANUAL dispatch loop has never closed
	// end-to-end, and automating a path that has never once worked by hand
	// converts an unproven feature into an unattended one. While false,
	// `auto:dispatch` behaves as a descriptive tag.
	TagAutoDispatch bool

	// Provisioner drives agent pod lifecycle (provision/start/stop/delete, logs,
	// chat). Nil disables the agent-control routes even if Agents is set.
	Provisioner Provisioner

	// GitHubOAuth configures the OAuth web flow for connecting an account.
	GitHubOAuth GitHubOAuthConfig
}

// GitHubOAuthConfig holds the OAuth App credentials + the public base URL used
// to build the redirect URI.
type GitHubOAuthConfig struct {
	ClientID     string
	ClientSecret string
	// BaseURL is the externally reachable origin used to construct the callback
	// URL. If empty the callback is derived from the inbound request host.
	BaseURL string
}

// Configured reports whether the OAuth App credentials are present.
func (c GitHubOAuthConfig) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// UseExtensions attaches the dependencies. Call before Handler.
//
// 🔴 IT WRAPS Notes, AND THAT WRAP IS THE WHOLE REASON THIS FUNCTION EXISTS
// RATHER THAN A PLAIN FIELD ASSIGNMENT. internal/notes does not join the
// session table — it cannot, the table is another service's — so every
// SessionLink it returns has DetailAvailable=false until something composes it.
// This is the ONE place that happens. Doing it in handlers instead would be
// twenty-odd places to forget, and forgetting is SILENT: false renders the
// words "no transcript recorded" over a live transcript, with nothing logged
// and nothing 404ing. Upstream that exact conflation disguised a months-long
// outage as ordinary retention.
//
// ⚠ It is applied here rather than in the binary's wiring on purpose: this is
// the only door into s.ext, so a fixture and the binary get the same store.
func (s *Server) UseExtensions(ext Extensions) {
	s.extDefects = ext.defects()
	ext.Notes = withSessionLiveness(ext.Notes, ext.SessionLiveness)
	s.ext = ext
}

// defects lists wiring mistakes that must not be SERVED with, as opposed to
// dependencies that are merely absent.
//
// 🔴 THE DISTINCTION IS "DEGRADES VISIBLY" VERSUS "LIES SILENTLY", AND IT IS
// THE ONLY test applied here. A nil Runbooks store is not a defect: the
// Runbooks tab renders an empty list, which is TRUE — this server has no
// runbooks. A notes store with no liveness probe IS a defect: every transcript
// row renders "no transcript recorded" over transcripts that may be perfectly
// alive, and there is no request that fails, nothing logged, and nothing for an
// operator to notice. The first is a configuration; the second is a page that
// states a falsehood.
//
// 🔴 THE CONSEQUENCE IS /readyz, NOT A PANIC, AND THAT IS DELIBERATE. A panic
// in a wiring call is a crash loop that takes the whole service down over a
// feature; a readiness failure keeps the process up, keeps /health green for
// liveness, and keeps the pod OUT OF SERVICE until someone reads the reason —
// which handleReady prints. It is the loudest signal that is not also an
// outage.
//
// ⚠ IT IS A METHOD ON Extensions, NOT ON Server, SO IT CAN BE TESTED WITHOUT
// ONE — and so a future second caller (a config validator, a boot log) reaches
// the same list rather than writing a second one.
func (e Extensions) defects() []string {
	var out []string
	if e.Notes != nil && e.SessionLiveness == nil {
		out = append(out, "Notes is wired but SessionLiveness is not: every task's transcript "+
			"links would render \"no transcript recorded\" whether or not a transcript exists, "+
			"silently and on every surface. Wire a liveness probe (internal/router satisfies it) "+
			"or leave Notes unset.")
	}
	return out
}

// PrivilegeApplier applies a granted privilege profile's Kubernetes RBAC to an
// agent's ServiceAccount, live (no pod restart). Implemented by
// internal/provision (which holds the in-cluster client + knows the
// SA/namespace naming); defined here consumer-side like Provisioner.
type PrivilegeApplier interface {
	// ApplyGrant ensures the RBAC objects for profile (its ClusterRules and/or
	// NamespaceRules) exist, bound to the agent's ServiceAccount. Idempotent.
	ApplyGrant(ctx context.Context, agentName, namespace string, profile privilege.Profile) error
	// RemoveGrant deletes the RBAC objects this service created for
	// (agent, profile).
	RemoveGrant(ctx context.Context, agentName, namespace, profileName string) error
}

// ProfileReapplier re-applies an agent's granted-profile env/kubeconfig to its
// running pod (a helm upgrade; the pod rolls). Optional. The grant path
// type-asserts for it, so a nil or fake provisioner (and the RBAC-only path) is
// unaffected.
type ProfileReapplier interface {
	ReapplyProfiles(ctx context.Context, agentID int64) error
}

// Provisioner is the agent-pod lifecycle driver the agent handlers depend on.
// It is defined here (consumer side) so this package does not import the heavy
// helm/client-go dependencies unless the binary wires a real provisioner in.
type Provisioner interface {
	// Dispatch provisions a pod for the agent and, when kickoff is true, sends
	// the note as the first message once the gateway is reachable.
	Dispatch(agentID int64, kickoff bool) error
	// Start scales a stopped/provisioned agent up (kicking off if pending).
	Start(agentID int64) error
	// Stop scales a running agent down to zero replicas.
	Stop(agentID int64) error
	// Destroy uninstalls the release and deletes the namespace.
	Destroy(agentID int64) error
	// Instances returns the live state of every agent workload, for status
	// reconciliation.
	//
	// 🔴 IT IS provision.Instance, NOT A POD, AND THE RENAME IS THE CARVE. The
	// method this replaces was `Pods` returning a Kubernetes-shaped struct, which
	// made every status consumer in this package a Kubernetes consumer by
	// transitivity. The provisioner contract's own vocabulary is what lets
	// agents.ComputeStatus answer for an agent running under any driver.
	Instances(ctx context.Context) ([]provision.Instance, error)
	// TailLogs returns the last N log lines of the agent's running pod.
	TailLogs(ctx context.Context, a agents.Agent, lines int64) (string, error)
	// StreamLogs follows the agent's pod logs, invoking emit per line.
	StreamLogs(ctx context.Context, a agents.Agent, emit func(string)) error
	// Chat sends a user message to the agent gateway under the given chat-session
	// key (each session = an independent gateway context), streaming assistant
	// deltas to emit, and returns the full assistant reply.
	Chat(ctx context.Context, a agents.Agent, sessionKey, message string, emit func(string)) (string, error)
	// ChatWithTools runs a tool-enabled turn via the gateway's responses API
	// under the given chat-session key: the model gets the native function tools
	// and dispatch executes each call in-process. emit (nil-safe) streams live
	// text/thinking deltas plus tool_call/tool_result events as the loop runs.
	// Returns agents.ErrResponsesUnsupported when the agent's gateway lacks the
	// responses API, so the caller can fall back to Chat.
	ChatWithTools(ctx context.Context, a agents.Agent, sessionKey, instructions, message string, tools []agents.ToolDef, dispatch agents.ToolDispatch, emit agents.StreamEmit) (string, error)
}
