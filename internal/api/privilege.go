package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/privilege"
	"github.com/ZacxDev/muster/internal/ui"
)

// registerPrivilegeRoutes wires the privilege profiles registry, per-agent grants,
// and the approve/deny of agent privilege requests (session/human surface).
// 🔴 NO EARLY RETURN ON A nil DEPENDENCY — see the package doc in server.go.
// Every handler below answers its own "nothing to serve from" case, so the
// recorded route set is a function of the CODE and never of the fixture.
func (s *Server) registerPrivilegeRoutes(mux Mux) {
	mux.HandleFunc("GET /ui/privilege-requests", s.requireSession(s.handlePrivilegeRequests))
	mux.HandleFunc("GET /ui/privileges", s.requireSession(s.handleProfilesContent))
	mux.HandleFunc("POST /privileges", s.requireSession(s.handleProfileCreate))
	mux.HandleFunc("DELETE /privileges/{id}", s.requireSession(s.handleProfileDelete))
	mux.HandleFunc("POST /ui/privilege-requests/{id}/approve", s.requireSession(s.handleRequestApprove))
	mux.HandleFunc("POST /ui/privilege-requests/{id}/deny", s.requireSession(s.handleRequestDeny))
	mux.HandleFunc("GET /ui/agents/{id}/grants", s.requireSession(s.handleAgentGrants))
	mux.HandleFunc("POST /agents/{id}/grants", s.requireSession(s.handleAgentGrant))
	mux.HandleFunc("DELETE /agents/{id}/grants/{profileId}", s.requireSession(s.handleAgentRevoke))
}

// --- profiles ---

func (s *Server) handleProfilesContent(w http.ResponseWriter, r *http.Request) {
	profs, err := s.ext.Privilege.ListProfiles(r.Context())
	if err != nil {
		s.logger.Printf("privilege: list profiles: %v", err)
		http.Error(w, "could not load profiles", http.StatusInternalServerError)
		return
	}
	s.renderProfiles(w, profs)
}

func (s *Server) handleProfileCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	// 🔴 THE NAME BECOMES PART OF A CLUSTER OBJECT'S IDENTITY, SO IT IS REFUSED
	// HERE RATHER THAN AT THE FIRST GRANT. This used to be an empty-string check
	// only, so `Cluster Triage` was accepted and then failed three packages away as
	// `500 could not apply grant: …` — at a moment when the operator is looking at
	// an agent rather than at the form they typed it into. privilege.ValidateName
	// owns the rule and states which artefact imposes it; the message is shown
	// verbatim because it is the only thing that tells them what to type instead.
	if err := privilege.ValidateName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var spec privilege.Spec
	if raw := strings.TrimSpace(r.FormValue("spec")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			http.Error(w, "spec is not valid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	_, err := s.ext.Privilege.CreateProfile(r.Context(), privilege.Profile{
		Name:        name,
		DisplayName: strings.TrimSpace(r.FormValue("display_name")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Spec:        spec,
	})
	if err != nil {
		s.logger.Printf("privilege: create profile %q: %v", name, err)
		http.Error(w, "could not create profile (name may already exist)", http.StatusBadRequest)
		return
	}
	s.handleProfilesContent(w, r)
}

// profileLock is one profile's write lock plus the count of goroutines that want
// it. The count is what lets the map be pruned: an entry lives exactly as long as
// someone holds or awaits it, so the map is bounded by CONCURRENT REQUESTS rather
// than by the number of profiles that have ever been written.
type profileLock struct {
	mu      sync.Mutex
	waiters int
}

// lockProfile serialises the privilege write paths that touch one profile, and
// returns the unlock. It is the fix for the TOCTOU window described in the
// invariant block below: a grant recorded BETWEEN handleProfileDelete's holder
// list and its DeleteProfile is not in the list, so its RBAC is never removed,
// and the delete's cascade then takes the row that was the only thing able to
// name it. Live escalated RBAC that no record names — the one direction the
// invariant forbids and the one that is not recoverable.
//
// 🔴 A LOCK, NOT A TRANSACTION, AND THE CHOICE IS DELIBERATE. The other candidate
// was to capture and remove the holders in one statement inside a transaction
// (`DELETE FROM agent_privileges WHERE profile_id = $1 RETURNING agent_id`,
// revoking the returned set, committing last). Three reasons it is not what this
// does:
//
//  1. IT DOES NOT ACTUALLY CLOSE THE WINDOW AT THE ISOLATION LEVEL WE RUN AT.
//     `DELETE … RETURNING` takes row locks on the rows that ALREADY exist. The
//     interleaving grant INSERTs a row that does not exist yet, and READ
//     COMMITTED has no predicate lock, so nothing blocks it. Making that shape
//     airtight needs SERIALIZABLE plus a retry loop, or a `SELECT … FOR UPDATE`
//     on the parent profile row — and the latter IS this function, implemented in
//     the database instead of in the process.
//  2. IT HOLDS A POSTGRES TRANSACTION OPEN ACROSS N APISERVER ROUND-TRIPS. The
//     revokes are network calls to the kube apiserver with client-go's retries
//     behind them, so the idle-in-transaction time — and the pinned pool
//     connection, and the held row locks — scale with the holder count.
//  3. A COMMIT THAT FAILS AFTER THE CLUSTER HAS ALREADY CHANGED lands in exactly
//     the state issue #14 is about, and makes it harder to describe rather than
//     easier: the rows come back on the rollback while the RBAC stays gone.
//
// 🟡 THE COST ACCEPTED IN EXCHANGE: this lock is IN-PROCESS, so it serialises one
// muster. That is the right scope for this deployment rather than a shortcut —
// `replicas: 1` is a documented code fact for muster, not an accident of
// capacity: the SSE broadcaster is an in-process map of Go channels with no Redis
// fan-out and no LISTEN/NOTIFY anywhere in the tree, and the background loops
// have no cross-process lease in this module. The deployment's own comment says
// raising the count means adding a lease first.
// 🔴 SO: IF muster EVER RUNS MORE THAN ONE REPLICA, THIS MUST BECOME A POSTGRES
// ADVISORY LOCK. internal/db's MigrationLockKey is the pattern, and LeaderGate
// (server.go) is the precedent for reaching one from this package without a pool.
// A second replica would reopen the window silently — no error, just two
// processes each serialising only themselves.
func (s *Server) lockProfile(profileID int64) func() {
	s.profileLocksMu.Lock()
	if s.profileLocks == nil {
		// Lazily, because Server is constructed several ways (New, and fixtures
		// that build it field-by-field) and a nil map here would panic on write.
		s.profileLocks = make(map[int64]*profileLock)
	}
	entry, ok := s.profileLocks[profileID]
	if !ok {
		entry = &profileLock{}
		s.profileLocks[profileID] = entry
	}
	entry.waiters++
	s.profileLocksMu.Unlock()

	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()
		s.profileLocksMu.Lock()
		entry.waiters--
		if entry.waiters == 0 {
			delete(s.profileLocks, profileID)
		}
		s.profileLocksMu.Unlock()
	}
}

// handleProfileDelete revokes the profile's live RBAC from every agent holding it,
// drops the profile, and then recomputes each holder's env/kubeconfig.
//
// 🔴 IT USED TO CALL DeleteProfile AND NOTHING ELSE, WHICH MADE A DELETE
// UNRECOVERABLE. The store's delete cascades the agent_privileges rows away, so
// afterwards nothing recorded that any agent ever held the profile — while the
// ClusterRole and ClusterRoleBinding the grant created were still bound to that
// agent's ServiceAccount. RemoveGrant resolves those objects from the
// (agent, profile) NAME pair, and the name was gone, so the revoke path could
// never be reached again: only an instance Destroy — which enumerates by LABEL —
// would ever have cleaned them up. With the `escalate` verb granted, the orphan
// can hold arbitrary rules, up to cluster-admin. It is the mirror image of the
// falsehood api.Extensions.defects' privilege entry exists to refuse: there the
// record claims access the cluster does not give, here the cluster gives access no
// record claims.
//
// 🔴 IT FAILS CLOSED: any revoke that does not succeed ABORTS the delete, leaving
// the profile and its grants in place and the operation retryable. The alternative
// — delete anyway and log — is what the revoke path used to do, and that is
// precisely the direction this round was told to make coherent (see the
// RECORD ⊇ LIVE invariant above grantProfile).
//
// 🔴 IT HOLDS THE PROFILE'S WRITE LOCK FOR THE WHOLE SEQUENCE, which is what makes
// the holder list it revokes from complete. Without it a grant recorded between
// the list and the delete is never revoked and its row is then cascaded away. See
// lockProfile.
//
// 🔴 IT REAPPLIES ENV/KUBECONFIG PER HOLDER AFTER THE DELETE, and the ORDER is
// load-bearing: ReapplyProfiles recomputes from the grants that exist WHEN IT
// RUNS, so a reapply before the delete would recompute with this profile's grant
// still in place and change nothing. RBAC removal is not enough on its own — a
// profile can contribute env vars and a mounted kubeconfig SECRET, and removing a
// role does not unwrite a credential already in a pod's filesystem.
func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	// Serialise against the grant paths for the whole read-revoke-delete sequence.
	unlock := s.lockProfile(id)
	defer unlock()
	// The NAME is what RemoveGrant needs, and this is the last moment it exists.
	prof, err := s.ext.Privilege.GetProfile(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("privilege: delete profile %d: read it first: %v", id, err)
		http.Error(w, "could not delete profile", http.StatusInternalServerError)
		return
	}
	holders, err := s.revokeLiveRBACForProfile(ctx, prof)
	if err != nil {
		s.logger.Printf("privilege: delete profile %d (%q): revoking live RBAC first: %v",
			id, prof.Name, err)
		http.Error(w, "could not delete profile: its cluster RBAC is still bound to at least one "+
			"agent and removing it failed, so the profile was KEPT — deleting it now would drop "+
			"the only record of that access and leave it unrevocable. NOTHING HAS CHANGED: every "+
			"grant record survives and any RBAC already removed before the failure is named by a "+
			"record that can revoke it again. Retry, or revoke the grants individually first. "+
			"Cause: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 🔴 THIS FAILURE IS NOT INERT AND THE MESSAGE MUST SAY SO — the two branches of
	// this function used to be calibrated in OPPOSITE directions, the safer one
	// explaining itself and the riskier one not. Above, nothing has changed. Here,
	// every holder's RBAC is already gone from the cluster while every grant row
	// survives, so each grant chip in the UI now claims access the ServiceAccount no
	// longer has. That is the TOLERABLE direction (the row still names the pair and
	// the revoke path treats an absent object as success, so a retry converges) but
	// it is NOT the same state as the branch above, and the operator's next move
	// differs: there they may leave it alone, here the access is already withdrawn
	// whatever the UI shows.
	if err := s.ext.Privilege.DeleteProfile(ctx, id); err != nil {
		s.logger.Printf("privilege: delete profile %d (%q): the RBAC was already removed from %d "+
			"holder(s) and the records were kept: %v", id, prof.Name, len(holders), err)
		http.Error(w, "could not delete profile, and THE CLUSTER HAS ALREADY CHANGED: this "+
			"profile's RBAC was removed from every agent holding it BEFORE the delete failed, so "+
			"the access is already withdrawn while the profile and its grant records were KEPT. "+
			"The grant chips still shown for it now claim access the ServiceAccount no longer "+
			"has. Nothing is unrevocable — every record survives and removal is idempotent — so "+
			"retrying this delete is safe and is the way to converge. Cause: "+err.Error(),
			http.StatusInternalServerError)
		return
	}
	// Recompute env/kubeconfig for each holder now that the grant rows are gone, the
	// way handleAgentRevoke does for its one agent. Best-effort and per-holder: the
	// RBAC and the records are already consistent, so a failure here cannot reopen
	// the invariant — it leaves a pod carrying env for a profile that no longer
	// exists until its next provision, which is the pre-existing behaviour this
	// closes rather than a new one it opens.
	for _, agent := range holders {
		s.reapplyEnvAsync(agent, prof)
	}
	s.handleProfilesContent(w, r)
}

// revokeLiveRBACForProfile removes the profile's RBAC from the agents recorded as
// holding it, and RETURNS THOSE AGENTS so the caller can finish the job. It is the
// pre-condition of a safe profile delete.
//
// ⚠ IT STOPS AT THE FIRST HOLDER WHOSE REMOVAL FAILS, so on error the agents after
// that one keep their RBAC and the returned slice is not the full holder set. That
// is safe rather than a bug — the caller refuses the delete, so every row survives
// and every remaining object is still named by one — but it is stated because the
// line here USED TO SAY "every agent", and a docstring claiming a width the body
// does not have is the shape that gets a guard written against the sentence
// instead of against the code. On success the slice IS every holder.
//
// 🔴 IT IS RBAC-ONLY, AND THE PROFILE CARRIES MORE THAN RBAC — WHICH IS WHY IT
// RETURNS THE AGENTS RATHER THAN JUST AN ERROR. A profile's env vars and its
// KubeconfigSecret have to be RECOMPUTED once a grant is gone, and removing a role
// does not unwrite a kubeconfig CREDENTIAL already mounted in a pod's filesystem.
// handleAgentRevoke has always followed its removal with reapplyEnvAsync for that
// reason; handleProfileDelete called nothing of the sort, for any holder, so a
// deleted profile left every running holder carrying its env and its credential
// until something else reprovisioned it — while the operator had been told the
// profile was gone. The agents come back from here because THIS is the only place
// that resolves them, and re-resolving them after the delete is impossible: the
// cascade has taken the rows by then. The caller does the reapply, after the
// delete, and the doc on handleProfileDelete says why that order is required.
//
// ⚠ A nil APPLIER IS A no-op, AND THAT IS THE ONE HOLE IN THE INVARIANT — STATED
// RATHER THAN PAPERED OVER. With no applier this server has applied nothing, so
// there is nothing of its making to remove; what it CANNOT see is RBAC left behind
// by an EARLIER run of the same deployment that had the tier armed. That
// transition is not silent, though: a deployment with a provisioner and a privilege
// store and no applier is a readiness defect (api.Extensions.defects), so the pod
// is out of service rather than quietly deleting profiles. Refusing the delete
// instead would break every development deployment that never applied anything.
//
// ⚠ IT DOES NOT SKIP A PROFILE WHOSE SPEC CARRIES NO RBAC, which is the one place
// this path is deliberately WIDER than grantProfile. Removal is idempotent and an
// absent object is success, so attempting it costs an API call; skipping it trusts
// that the spec has not changed since the grant was applied, which is a claim about
// history that a spec read today cannot make.
func (s *Server) revokeLiveRBACForProfile(ctx context.Context, prof privilege.Profile) ([]agents.Agent, error) {
	if s.ext.PrivilegeApply == nil {
		// 🔴 THE HOLDERS ARE STILL RESOLVED, BECAUSE THE CALLER NEEDS THEM FOR THE
		// ENV/KUBECONFIG HALF AND THAT HALF DOES NOT DEPEND ON AN APPLIER. A nil
		// applier means no RBAC of this server's making exists to remove; it does not
		// mean the profile contributed no env and no mounted kubeconfig. Returning
		// early with nil here — which is what an RBAC-shaped reading of this function
		// would do — would silently reinstate exactly the leak above.
		return s.holdersOf(ctx, prof)
	}
	if s.ext.Agents == nil {
		// Reachable only from a fixture: an applier needs an agent's name and
		// namespace, and those live on the row. Refusing is fail-closed.
		return nil, errors.New("no agents store, so the agents holding this profile cannot be " +
			"resolved and their cluster RBAC cannot be removed")
	}
	holders, err := s.holdersOf(ctx, prof)
	if err != nil {
		return nil, err
	}
	for i, agent := range holders {
		if err := s.ext.PrivilegeApply.RemoveGrant(ctx, agent.Name, agent.Namespace, prof.Name); err != nil {
			// Return the prefix whose RBAC IS gone, so a caller that decides to carry
			// on is not told the set was empty. Today's caller refuses the delete.
			return holders[:i], fmt.Errorf("removing profile %q's RBAC from agent %q: %w",
				prof.Name, agent.Name, err)
		}
	}
	return holders, nil
}

// holdersOf resolves the agents recorded as holding a profile. Separated so the
// nil-applier path above resolves them the same way rather than open-coding a
// second walk of the same two calls.
func (s *Server) holdersOf(ctx context.Context, prof privilege.Profile) ([]agents.Agent, error) {
	if s.ext.Agents == nil {
		return nil, nil
	}
	grants, err := s.ext.Privilege.ListGrantsForProfile(ctx, prof.ID)
	if err != nil {
		return nil, fmt.Errorf("listing the agents holding profile %q: %w", prof.Name, err)
	}
	out := make([]agents.Agent, 0, len(grants))
	for _, g := range grants {
		agent, err := s.ext.Agents.Get(ctx, g.AgentID)
		if err != nil {
			return nil, fmt.Errorf("resolving agent %d, which holds profile %q: %w",
				g.AgentID, prof.Name, err)
		}
		out = append(out, agent)
	}
	return out, nil
}

func (s *Server) renderProfiles(w http.ResponseWriter, profs []privilege.Profile) {
	views := make([]ui.ProfileView, 0, len(profs))
	for _, p := range profs {
		views = append(views, ui.ProfileView{
			ID: p.ID, Name: p.Name, DisplayName: p.DisplayName,
			Description: p.Description, Summary: profileSummary(p.Spec),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderProfiles(w, views); err != nil {
		s.logger.Printf("privilege: render profiles: %v", err)
	}
}

// profileSummary is a short human description of what a profile grants.
func profileSummary(spec privilege.Spec) string {
	var parts []string
	if len(spec.ClusterRules) > 0 {
		parts = append(parts, "cluster RBAC")
	}
	if len(spec.NamespaceRules) > 0 {
		parts = append(parts, "namespace RBAC")
	}
	if len(spec.Env) > 0 {
		parts = append(parts, "env (on redispatch)")
	}
	if spec.KubeconfigSecret != "" {
		parts = append(parts, "kubeconfig (on redispatch)")
	}
	if len(parts) == 0 {
		return "empty"
	}
	return strings.Join(parts, " · ")
}

// --- requests: approve / deny ---

func (s *Server) handleRequestApprove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	req, err := s.ext.Privilege.GetRequest(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("privilege: get request %d: %v", id, err)
		http.Error(w, "could not load request", http.StatusInternalServerError)
		return
	}
	prof, err := s.ext.Privilege.GetProfileByName(ctx, req.Profile)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "no profile named "+req.Profile+"; define it first, then approve", http.StatusBadRequest)
			return
		}
		s.logger.Printf("privilege: get profile %q: %v", req.Profile, err)
		http.Error(w, "could not load profile", http.StatusInternalServerError)
		return
	}
	agent, err := s.ext.Agents.Get(ctx, req.AgentID)
	if err != nil {
		s.logger.Printf("privilege: get agent %d: %v", req.AgentID, err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	if err := s.grantProfile(ctx, agent, prof, "user"); err != nil {
		s.logger.Printf("privilege: grant on approve (req %d): %v", id, err)
		http.Error(w, "could not apply grant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.ext.Privilege.DecideRequest(ctx, id, privilege.StatusApproved, "user"); err != nil {
		s.logger.Printf("privilege: decide request %d: %v", id, err)
	}
	s.broadcast(EventPrivilegeCreated, strconv.FormatInt(id, 10)) // refresh the requests block
	s.renderPendingRequests(w, r)
}

func (s *Server) handleRequestDeny(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.ext.Privilege.DecideRequest(r.Context(), id, privilege.StatusDenied, "user"); err != nil {
		s.logger.Printf("privilege: deny request %d: %v", id, err)
		http.Error(w, "could not deny request", http.StatusInternalServerError)
		return
	}
	s.renderPendingRequests(w, r)
}

// renderPendingRequests re-renders the #privilege-requests block.
func (s *Server) renderPendingRequests(w http.ResponseWriter, r *http.Request) {
	reqs, err := s.ext.Privilege.ListPendingRequests(r.Context())
	if err != nil {
		s.logger.Printf("privilege: list pending: %v", err)
		http.Error(w, "could not load privilege requests", http.StatusInternalServerError)
		return
	}
	views := make([]ui.PrivilegeRequestView, 0, len(reqs))
	for _, pr := range reqs {
		views = append(views, ui.PrivilegeRequestView{
			ID: pr.ID, AgentName: pr.AgentName, Profile: pr.Profile, Reason: pr.Reason, CreatedAt: pr.CreatedAt,
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderPrivilegeRequests(w, views); err != nil {
		s.logger.Printf("privilege: render requests: %v", err)
	}
}

// --- grants (per agent) ---

func (s *Server) handleAgentGrants(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	s.renderAgentGrants(w, r, id)
}

func (s *Server) handleAgentGrant(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	profileID, err := strconv.ParseInt(r.FormValue("profile_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad profile id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	agent, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	prof, err := s.ext.Privilege.GetProfile(ctx, profileID)
	if err != nil {
		http.Error(w, "could not load profile", http.StatusBadRequest)
		return
	}
	if err := s.grantProfile(ctx, agent, prof, "user"); err != nil {
		s.logger.Printf("privilege: grant profile %d to agent %d: %v", profileID, id, err)
		http.Error(w, "could not apply grant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderAgentGrants(w, r, id)
}

func (s *Server) handleAgentRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	profileID, err := strconv.ParseInt(r.PathValue("profileId"), 10, 64)
	if err != nil {
		http.Error(w, "bad profile id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	// Same lock as the grant and delete paths: a per-agent revoke racing a profile
	// delete is benign today (removal is idempotent, an absent object is success),
	// but serialising all three is what lets the invariant be stated without a
	// carve-out a later reader has to re-derive.
	unlock := s.lockProfile(profileID)
	defer unlock()
	agent, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	prof, err := s.ext.Privilege.GetProfile(ctx, profileID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The profile is gone, and its grants went with it (the store's delete
			// cascades) — so there is no record to drop and no name to resolve RBAC
			// from. Rendering current state is the honest answer to a stale button.
			s.renderAgentGrants(w, r, id)
			return
		}
		s.logger.Printf("privilege: revoke profile %d from agent %d: read the profile: %v",
			profileID, id, err)
		http.Error(w, "could not revoke", http.StatusInternalServerError)
		return
	}
	// 🔴 A FAILED RBAC REMOVAL ABORTS THE REVOKE, AND IT USED TO BE LOGGED AND
	// IGNORED. The old order was: RemoveGrant best-effort, then drop the row, then
	// render "revoked" — so a transient apiserver error, or a ServiceAccount missing
	// the `delete` verb, left ESCALATED RBAC live with nothing recording it, and the
	// UI said the privilege was gone. That is the same direction as the deleted
	// profile above and the OPPOSITE direction from grantProfile, which applies
	// before recording and fails closed. Both now fail closed: see the invariant on
	// grantProfile.
	if s.ext.PrivilegeApply != nil {
		if err := s.ext.PrivilegeApply.RemoveGrant(ctx, agent.Name, agent.Namespace, prof.Name); err != nil {
			s.logger.Printf("privilege: remove RBAC for agent %d profile %d: %v", id, profileID, err)
			http.Error(w, "could not revoke: removing the cluster RBAC failed, so the grant was "+
				"KEPT rather than recorded as revoked — the access is still live and the record "+
				"is still the only thing that can find it. Retry. Cause: "+err.Error(),
				http.StatusInternalServerError)
			return
		}
	}
	if err := s.ext.Privilege.Revoke(ctx, id, profileID); err != nil {
		s.logger.Printf("privilege: revoke profile %d from agent %d: %v", profileID, id, err)
		http.Error(w, "could not revoke", http.StatusInternalServerError)
		return
	}
	// Reflect the removed env/kubeconfig in the running pod (grant already dropped,
	// so reapply recomputes without it). Best-effort; RBAC was removed above.
	s.reapplyEnvAsync(agent, prof)
	s.renderAgentGrants(w, r, id)
}

// ---------------------------------------------------------------------------
// THE ONE INVARIANT THE THREE WRITE PATHS SHARE, STATED HERE BECAUSE IT WAS
// STATED NOWHERE AND EACH PATH HAD PICKED ITS OWN DIRECTION.
//
// 🔴 THE RECORD IS NEVER NARROWER THAN THE CLUSTER — AS THE DIRECTION EACH PATH
// FAILS IN, AND, SINCE THE THREE PATHS TOOK A SHARED PER-PROFILE LOCK, ALSO ACROSS
// THE CONCURRENT WINDOW THAT USED TO BE THE STANDING EXCEPTION HERE: every RBAC
// object this server has applied is named by a grant row that still exists. The
// qualifier that used to sit in this sentence — "EXCEPT across the concurrent
// window … which no ordering of these three calls closes" — is retired, and the
// wording is deliberate about WHY: no ORDERING closes it, and none was what closed
// it; serialising the paths did. See lockProfile. The record MAY be wider —
// and that direction is not harmless either: a row whose objects are gone renders a
// grant chip claiming access the ServiceAccount does not have, which is precisely
// the falsehood api.Extensions.defects' privilege entry refuses to serve. What makes
// it the TOLERABLE direction is RECOVERABILITY: the row still names the pair, so a
// retry can re-apply or revoke, and the revoke path treats an absent object as
// success. The other direction is not recoverable at all. The object name is
// DERIVED from the (agent, profile) pair the row carries, so a row lost while its
// objects live is escalated access nothing can name — and with the `escalate` verb
// granted, those objects can hold arbitrary rules.
//
// 🔴 WHAT THAT DICTATES, PER PATH — AND THE THREE USED TO DISAGREE:
//
//   - grantProfile: apply, THEN record. A failed apply records nothing (fails
//     closed, and always did). A failed RECORD after a successful apply is the
//     window the invariant forbids, so the apply is rolled BACK; if the rollback
//     also fails the error names the orphan explicitly, because at that point a
//     human has to remove it.
//   - handleAgentRevoke: remove, THEN drop the row. A failed removal KEEPS the row
//     (it used to log and drop it — fail-open, the exact inversion of the grant
//     path).
//   - handleProfileDelete: remove for every holder, THEN delete. A failed removal
//     aborts the delete (it used to not remove at all, and the delete cascades the
//     rows, so the objects outlived every record of themselves).
//
// ⚠ NONE OF THIS IS ATOMIC, AND CALLING IT TRANSACTIONAL WOULD BE A LIE. There is
// no transaction spanning Postgres and an apiserver; what these paths guarantee is
// an ORDER and a direction of failure, and that a SEQUENTIAL operation is retryable
// from wherever it stopped.
//
// 🔴 CONCURRENCY IS WHY A LOCK IS PART OF THE INVARIANT AND NOT AN OPTIMISATION.
// Ordering alone was not enough, and this block used to say so: handleProfileDelete
// lists the holders (ListGrantsForProfile), removes each one's RBAC, and only then
// calls DeleteProfile, which cascades the grant rows. A grant recorded BETWEEN the
// list and the delete — through handleAgentGrant, or through handleRequestApprove,
// both reachable from a second tab — was not in the list, so its RBAC was never
// removed, and the cascade then took its row: live escalated RBAC that no record
// names, the forbidden direction. All three paths now take the same per-profile
// lock (lockProfile), so no grant can be recorded inside a delete's window. The
// lock is IN-PROCESS and that bound is stated on lockProfile — a second replica
// would reopen this silently.
//
// 🔴 AND THE INVARIANT IS NO LONGER RBAC-ONLY. A profile's env and its
// KubeconfigSecret are recomputed by reapplyEnvAsync, which handleAgentRevoke has
// always called and which handleProfileDelete now calls for every holder, after the
// delete. That matters most for the credential: RBAC removal is visible in the
// cluster, but a kubeconfig already written into a pod's filesystem is not undone by
// removing a role. See revokeLiveRBACForProfile, which returns the holders for
// exactly this, and handleProfileDelete for why the reapply must follow the delete.
// ---------------------------------------------------------------------------

// grantProfile applies the profile's RBAC live (if an applier is wired) then
// records the grant. Applying first means a failed apply doesn't leave a phantom
// grant. A nil applier (no in-cluster client) records the grant only. If the
// profile also carries env/kubeconfig and the agent is running, it re-applies
// those to the pod — RBAC needs no restart, env does. A stopped agent picks up the
// env at its next dispatch.
//
// 🔴 IT TAKES THE PROFILE'S WRITE LOCK, AND THAT IS WHERE THE TOCTOU FIX IS PAID
// ON THIS SIDE. This is the one place both grant entry points funnel through —
// handleAgentGrant and handleRequestApprove — so locking here rather than in each
// of them is what makes "a grant cannot interleave with a delete" true of every
// caller instead of true of the callers someone remembered. Note the profile is
// read by the CALLER, before this lock: a grant that loses the race therefore
// applies against a profile that has since been deleted, its record then fails the
// foreign key, and the rollback below removes the RBAC it had just applied. That is
// the correct outcome, and it is the tolerable direction.
func (s *Server) grantProfile(ctx context.Context, agent agents.Agent, prof privilege.Profile, by string) error {
	unlock := s.lockProfile(prof.ID)
	defer unlock()
	applied := false
	if s.ext.PrivilegeApply != nil && prof.Spec.HasRBAC() {
		if err := s.ext.PrivilegeApply.ApplyGrant(ctx, agent.Name, agent.Namespace, prof); err != nil {
			return err
		}
		applied = true
	}
	if _, err := s.ext.Privilege.Grant(ctx, agent.ID, prof.ID, by); err != nil {
		if !applied {
			return err
		}
		// 🔴 LIVE RBAC WITH NO RECORD OF IT — the state the invariant above forbids,
		// and the one this branch did not exist to handle. Roll the apply back so the
		// cluster and the record agree again.
		if rmErr := s.ext.PrivilegeApply.RemoveGrant(ctx, agent.Name, agent.Namespace, prof.Name); rmErr != nil {
			return fmt.Errorf("recording the grant of %q to %q failed (%w) AND rolling back the "+
				"RBAC that was already applied failed (%v): the cluster now holds RBAC for that "+
				"pair which NO record names, so nothing in muster can revoke it — remove the "+
				"clusterrole/clusterrolebinding (and role/rolebinding) labelled "+
				"muster.dev/policy=%s by hand",
				prof.Name, agent.Name, err, rmErr, prof.Name)
		}
		return fmt.Errorf("recording the grant of %q to %q failed, so the RBAC that had already "+
			"been applied was rolled back and nothing was granted: %w", prof.Name, agent.Name, err)
	}
	s.reapplyEnvAsync(agent, prof)
	return nil
}

// reapplyEnvAsync re-applies an agent's granted-profile env/kubeconfig to its
// running pod when the just-granted-or-revoked profile carried env/kubeconfig, so
// the pod's env reflects current grants (a helm upgrade; the pod rolls — RBAC is
// applied/removed live separately, no restart). No-op for a stopped agent (env
// applies on its next dispatch) or when no reapplier/provisioner is wired.
func (s *Server) reapplyEnvAsync(agent agents.Agent, prof privilege.Profile) {
	if len(prof.Spec.Env) == 0 && prof.Spec.KubeconfigSecret == "" {
		return
	}
	if agent.Status != agents.StatusRunning && agent.Status != agents.StatusProvisioning {
		return
	}
	r, ok := s.ext.Provisioner.(ProfileReapplier)
	if !ok {
		return
	}
	safeGo(s.logger, "reapply profiles", func() {
		if err := r.ReapplyProfiles(context.Background(), agent.ID); err != nil {
			s.logger.Printf("privilege: reapply env/kubeconfig for agent %d: %v", agent.ID, err)
		}
	})
}

// AgentProfileEnv resolves the merged env vars + kubeconfig secret across all
// privilege profiles granted to an agent. Wired into the provisioner (via
// SetProfileProvider) so granted env/kubeconfig lands in the agent's pod at
// (re)provision. Returns empty when the privilege store is unset.
func (s *Server) AgentProfileEnv(ctx context.Context, agentID int64) ([]privilege.EnvVar, string) {
	if s.ext.Privilege == nil {
		return nil, ""
	}
	grants, err := s.ext.Privilege.ListGrantsForAgent(ctx, agentID)
	if err != nil {
		s.logger.Printf("privilege: profile env lookup for agent %d: %v", agentID, err)
		return nil, ""
	}
	// Grants carry the joined profile spec (see ListGrantsForAgent), so read the
	// env/kubeconfig off each grant directly — no per-grant GetProfile (avoids an
	// N+1). Order and first-non-empty-kubeconfig semantics are unchanged.
	var env []privilege.EnvVar
	var kubeconfigSecret string
	for _, g := range grants {
		env = append(env, g.Spec.Env...)
		if kubeconfigSecret == "" && g.Spec.KubeconfigSecret != "" {
			kubeconfigSecret = g.Spec.KubeconfigSecret
		}
	}
	return env, kubeconfigSecret
}

// AgentGrantedProfiles returns the names of the privilege profiles an agent
// currently holds. Wired into the provisioner (via SetRBACTeardown) so Destroy
// knows which cluster-scoped ClusterRole/ClusterRoleBinding to delete before the
// agent row — and with it the cascading agent_privileges rows — disappears.
//
// Unlike AgentProfileEnv this SURFACES the error rather than degrading to empty:
// an unreadable grant list means "unknown, possibly non-empty", and silently
// treating that as "no grants" is precisely how the RBAC leaked. The caller logs
// it and counts it.
func (s *Server) AgentGrantedProfiles(ctx context.Context, agentID int64) ([]string, error) {
	if s.ext.Privilege == nil {
		return nil, nil
	}
	grants, err := s.ext.Privilege.ListGrantsForAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(grants))
	for _, g := range grants {
		if g.ProfileName == "" {
			continue // no name ⇒ no deterministic RBAC object name to delete
		}
		names = append(names, g.ProfileName)
	}
	return names, nil
}

func (s *Server) renderAgentGrants(w http.ResponseWriter, r *http.Request, agentID int64) {
	ctx := r.Context()
	grants, err := s.ext.Privilege.ListGrantsForAgent(ctx, agentID)
	if err != nil {
		s.logger.Printf("privilege: list grants for agent %d: %v", agentID, err)
		http.Error(w, "could not load grants", http.StatusInternalServerError)
		return
	}
	profs, err := s.ext.Privilege.ListProfiles(ctx)
	if err != nil {
		s.logger.Printf("privilege: list profiles: %v", err)
		http.Error(w, "could not load profiles", http.StatusInternalServerError)
		return
	}
	gv := make([]ui.GrantView, 0, len(grants))
	granted := make(map[int64]bool, len(grants))
	for _, g := range grants {
		gv = append(gv, ui.GrantView{ProfileID: g.ProfileID, ProfileName: g.ProfileName})
		granted[g.ProfileID] = true
	}
	// Offer only not-yet-granted profiles in the grant selector.
	opts := make([]ui.ProfileOption, 0, len(profs))
	for _, p := range profs {
		if !granted[p.ID] {
			opts = append(opts, ui.ProfileOption{ID: p.ID, Name: p.Name})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderAgentGrants(w, agentID, gv, opts); err != nil {
		s.logger.Printf("privilege: render grants: %v", err)
	}
}
