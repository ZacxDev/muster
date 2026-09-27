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

	// Provisioner drives agent instance LIFECYCLE (provision/start/stop/delete,
	// state, logs). Nil disables the seven lifecycle routes even if Agents is set,
	// through requireLifecycleProvisioner.
	Provisioner Provisioner

	// Gateway drives agent CHAT. Nil disables the two chat routes even if
	// Provisioner is set, through requireGatewayProvisioner.
	//
	// 🔴 IT IS A SEPARATE FIELD FROM Provisioner ON PURPOSE — see [Gateway]. The
	// two nils are independent, so a deployment that can provision but not chat
	// renders truthfully instead of having to choose which lie to tell.
	Gateway Gateway

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
	// 🔴 THIS ENTRY IS cmd/muster-server/doc_seams.go ENTRY 2 FALLING DUE, AND
	// ENTRY 2 NAMED THE EXACT MOMENT. A privilege store that RECORDS grants
	// nobody applies was defensible only while no agent pod could exist to hold
	// one: there was no surface on which a user was told a privilege was live when
	// it was not, because there was no live agent. A wired Provisioner creates
	// real instances, so the grant chip becomes a page stating a falsehood — the
	// chip says granted over a ServiceAccount with none of the permissions — and
	// that is the "lies silently" side of this function's own line.
	//
	// ⚠ IT IS FAIL-CLOSED AND IT WILL REFUSE A DEPLOYMENT THAT USED TO COME UP.
	// That is the point and it is the choice entry 2 offered: wire a
	// PrivilegeApplier in the same change, or make the combination unready. It is
	// listed here rather than wrapped at a route — unlike the nil Provisioner,
	// which IS wrapped — because there is no single route to refuse: the falsehood
	// is rendered by every surface that shows a grant, and a grant recorded
	// through one route is read back through several.
	//
	// ⚠ IT OVER-TRIGGERS FOR A PROVISIONER THAT CREATES NOTHING, and that is
	// stated rather than fixed. The noop driver records instead of provisioning,
	// so a grant over one of its instances lies about a pod that was never real
	// either — harmless. Distinguishing the two would mean this function reading
	// the driver's capabilities, which makes a readiness check depend on a
	// backend's self-report; the fail-closed direction is cheaper and wrong only
	// in the direction of refusing to serve.
	if e.Provisioner != nil && e.Privilege != nil && e.PrivilegeApply == nil {
		out = append(out, "Provisioner is wired but PrivilegeApply is not, while a privilege "+
			"store IS wired: grants would be RECORDED and never applied, over agent instances "+
			"that now really exist. Every surface showing a grant would claim a permission the "+
			"instance's ServiceAccount does not have. Wire a PrivilegeApplier, or leave the "+
			"privilege store unset, or leave the provisioner unwired. See "+
			"cmd/muster-server/doc_seams.go entry 2, which named this exact combination as the "+
			"moment its own argument dies.")
	}
	return out
}

// ProvisionerUnwiredField is the JSON key both provisioner wrappers set to `true` on
// its 503 body.
//
// 🔴 IT IS THE FIELD, NOT THE SENTENCE, THAT A CLIENT BRANCHES ON — the same
// shape HookUnarmedField gives the machine tier, and for a sharper reason. Every
// OTHER 503 a caller meets on these routes is a transient outage worth retrying;
// this one is a permanent property of the BUILD, and a client that cannot tell
// them apart retries for ever against a server whose answer can never change.
// A discriminator made of prose is one a reword silently breaks.
const ProvisionerUnwiredField = "provisionerUnwired"

// The provisioner wrappers refuse a route that cannot do its work without
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
// Registered as requireSession(requireLifecycleProvisioner(h)) the session check runs
// first, so an anonymous caller is refused before this reply can tell them which
// dependencies this deployment has wired. The reverse order turns every one of
// these routes into an unauthenticated probe of the server's build.
//
// ⚠ IT DOES NOT MOVE THE ROUTE GOLDEN, WHICH IS WHY THE ROUTES STAY REGISTERED.
// The golden records patterns only (see routes_golden_test.go's KNOWN LIMIT
// banner), and a wrapper changes the handler, not the pattern. doc_seams.go
// entry 1 says the golden moving for this nil would itself be a defect; it does
// not move.
// 🔴 THERE ARE TWO WRAPPERS NOW, NOT ONE, AND EVERY WORD ABOVE STILL APPLIES TO
// BOTH. The split is described on [Gateway]: one nil used to gate nine routes, so
// wiring the seven lifecycle ones meant also claiming the two chat ones. Each
// wrapper refuses at the same door, in the same vocabulary, naming the dependency
// the route actually needs — which is the reason the split was preferred over an
// adapter whose chat methods returned a rendered refusal from inside a handler.
// One condition, one refusal shape.
func (s *Server) requireLifecycleProvisioner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Provisioner == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "this build has no agent provisioner wired, so it cannot " +
					"provision, start, stop, destroy or stream logs from an agent. " +
					"This is a declared seam, not an outage and not your credential: " +
					"see cmd/muster-server/doc_seams.go entry 1.",
				ProvisionerUnwiredField: true,
			})
			return
		}
		next(w, r)
	}
}

// requireGatewayProvisioner is the chat half. See requireLifecycleProvisioner.
func (s *Server) requireGatewayProvisioner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Gateway == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "this build has no agent gateway wired, so it cannot chat " +
					"with an agent. Provisioning may still work — the two are separate " +
					"dependencies. This is a declared seam, not an outage and not your " +
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

// Provisioner is the agent-instance LIFECYCLE driver the agent handlers depend
// on: bring one into existence, scale it, destroy it, read its state and its
// logs. It is defined here (consumer side) so this package does not import the
// heavy cluster dependencies unless the binary wires a real provisioner in.
//
// ⚠ CHAT IS NOT HERE ANY MORE — see [Gateway] for why the two are separate.
type Provisioner interface {
	// Dispatch provisions an instance for the agent when kickoff is true, and
	// creates NOTHING when it is false — see the note below on both halves.
	//
	// 🔴 THIS DOC USED TO DESCRIBE BEHAVIOUR NO IMPLEMENTATION HAS. It read
	// "provisions a pod for the agent and, when kickoff is true, sends the note as
	// the first message once the gateway is reachable", which is wrong twice over
	// for internal/agentprovision — the only implementation in this module:
	//
	//   - it does NOT send the note. Delivery needs an api.Gateway and there is no
	//     implementation of one here, so the non-delivery is recorded on the row
	//     instead (agentprovision.UndeliveredKickoffReason). An interface doc
	//     promising a delivery is how a future implementer comes to call
	//     SetKickedOff, which is the one thing that package must never do.
	//   - kickoff=false creates NOTHING rather than provisioning-then-storing-
	//     stopped. The caller's OTHER action is the UI's "Save for later", whose
	//     gate check (agents.go, the `gate:<reason>` refusal) allows a save on the
	//     stated grounds that it "provisions nothing".
	//
	// An implementer that provisions on kickoff=false, or that reports a delivery
	// it did not make, is wrong against this interface — not merely different.
	Dispatch(agentID int64, kickoff bool) error
	// Start brings a stopped or never-provisioned agent up, creating its instance
	// when the driver reports the backend does not have one.
	//
	// ⚠ IT DOES NOT KICK OFF, AND THIS DOC SAID IT DID. It read "(kicking off if
	// pending)" — the same retracted claim Dispatch's doc above corrects, left
	// behind in the sibling method for one round. The only implementation records
	// the non-delivery instead; see agentprovision.UndeliveredKickoffReason.
	Start(agentID int64) error
	// Stop scales a running agent down to zero replicas, keeping its declaration.
	// An instance the backend does not have is already stopped, not an error.
	Stop(agentID int64) error
	// Destroy removes the instance and everything the driver created for it, then
	// deletes the stored row.
	//
	// ⚠ IT DELETES THE ROW ONLY WHEN THE BACKEND HOLDS NOTHING, and this doc did
	// not say so — which is the contract the lifecycle adapter added. A terminal
	// ownership refusal or an unreachable backend KEEPS the row, because the row is
	// then the only record of a live instance.
	//
	// ⚠ ITS PREVIOUS WORDING WAS "uninstalls the release and deletes the
	// namespace" — helm and Kubernetes vocabulary in an interface whose Instances
	// doc makes a 🔴 point of having removed exactly that, and false for any driver
	// without namespaces.
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
	// TailLogs returns the last N log lines of the agent's running instance.
	TailLogs(ctx context.Context, a agents.Agent, lines int64) (string, error)
	// StreamLogs follows the agent's instance output, invoking emit per line.
	StreamLogs(ctx context.Context, a agents.Agent, emit func(string)) error
}

// Gateway is the agent CHAT surface: talking to a running instance's model
// gateway.
//
// 🔴 IT IS A SEPARATE INTERFACE FROM Provisioner, AND THE SPLIT IS THE WHOLE
// POINT. Both used to be one, behind one nil and one wrapper, so the seven
// LIFECYCLE methods could not be wired without also claiming the two CHAT ones.
// That is not a packaging preference: provision.Provisioner — the driver contract
// a lifecycle adapter is built on — has no notion of a chat turn at all, so an
// adapter over it can implement the seven honestly and the two not at all. Behind
// one interface the only ways to express that were to wire a stub (which
// doc_seams.go entry 1 rejects, because dispatch reporting success over a pod that
// does not exist is the defect the wrapper was built to remove) or to leave
// lifecycle unwired for as long as chat is. Two interfaces make "lifecycle works,
// chat honestly refuses" sayable.
//
// ⚠ A DEPLOYMENT MAY WIRE EITHER, BOTH, OR NEITHER, and each combination has a
// truthful rendering. Chat without lifecycle is unusual but not incoherent — an
// installation whose instances are provisioned by something else entirely.
type Gateway interface {
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
