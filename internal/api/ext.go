package api

import (
	"context"
	"net/http"

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

// ProvisionerUnwiredField is the JSON key requireProvisioner sets to `true` on
// its 503 body.
//
// 🔴 IT IS THE FIELD, NOT THE SENTENCE, THAT A CLIENT BRANCHES ON — the same
// shape HookUnarmedField gives the machine tier, and for a sharper reason. Every
// OTHER 503 a caller meets on these routes is a transient outage worth retrying;
// this one is a permanent property of the BUILD, and a client that cannot tell
// them apart retries for ever against a server whose answer can never change.
// A discriminator made of prose is one a reword silently breaks.
const ProvisionerUnwiredField = "provisionerUnwired"

// requireProvisioner refuses a route that cannot do its work without
// Extensions.Provisioner, rather than letting the handler reach a nil interface.
//
// 🔴 THIS EXISTS BECAUSE "REGISTERED AND REFUSES" WAS NOT TRUE, AND THE BOOT
// BANNER SAID IT ON EVERY START. Measured against the image this tree builds,
// with Provisioner nil (the only state cmd/muster-server can be in today):
// POST /agents answered 200 and rendered a card while the row sat
// `provisioning` for ever; POST /agents/{id}/start answered 200 with 3,590
// bytes of card HTML; POST /agents/{id}/stop answered 200; DELETE /agents/{id}
// answered 200 and htmx removed the card while the row was still there on the
// next read; and GET /agents/{name}/logs/stream answered 200 with `: connected`
// and then EOF, which a reader sees as "this agent has no logs".
// muster_panics_total{source="goroutine"} stood at 4. Every one of those is a
// surface stating a falsehood with nothing logged where a user is looking.
//
// 🔴 IT IS THE SAME OBSERVABLE doc_seams.go ENTRY 1 ARGUES AGAINST. That entry
// refuses to wire a FAKE provisioner because a fake "would make the Agents tab
// LOOK operational: dispatch would report success, no pod would exist, and the
// agent would sit in `provisioning` for ever." Wiring nil produced exactly that,
// byte for byte. The nil was never the honest option on its own — this wrapper
// is what makes it one.
//
// 🔴 WHY A WRAPPER AND NOT A defects() ENTRY, STATED SO THE NEXT READER DOES NOT
// RE-OPEN IT. Adding `Provisioner == nil && Agents != nil` to defects() would
// fail /readyz, which is fail-closed and simpler — and would make this module
// UNDEPLOYABLE until the provisioner adapter lands, taking the task board, notes,
// repos, projects and runbooks down with a seam that is open on purpose. The
// refusal belongs at the routes that cannot work, not at the whole process. With
// this wrapper a nil Provisioner moves from the "lies silently" side of
// defects()'s own line to the "degrades visibly" side, which is precisely why it
// is NOT listed there.
//
// ⚠ IT IS THE INNER WRAPPER, WITH AUTH OUTSIDE IT, AND THE ORDER IS LOAD-BEARING.
// Registered as requireSession(requireProvisioner(h)) the session check runs
// first, so an anonymous caller is refused before this reply can tell them which
// dependencies this deployment has wired. The reverse order turns every one of
// these routes into an unauthenticated probe of the server's build.
//
// ⚠ IT DOES NOT MOVE THE ROUTE GOLDEN, WHICH IS WHY THE ROUTES STAY REGISTERED.
// The golden records patterns only (see routes_golden_test.go's KNOWN LIMIT
// banner), and a wrapper changes the handler, not the pattern. doc_seams.go
// entry 1 says the golden moving for this nil would itself be a defect; it does
// not move.
func (s *Server) requireProvisioner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Provisioner == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "this build has no agent provisioner wired, so it cannot " +
					"provision, start, stop, destroy, stream logs from or chat with an " +
					"agent. This is a declared seam, not an outage and not your " +
					"credential: see cmd/muster-server/doc_seams.go entry 1.",
				ProvisionerUnwiredField: true,
			})
			return
		}
		next(w, r)
	}
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
