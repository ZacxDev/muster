package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

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
	if name == "" {
		http.Error(w, "profile name required", http.StatusBadRequest)
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

func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.ext.Privilege.DeleteProfile(r.Context(), id); err != nil {
		s.logger.Printf("privilege: delete profile %d: %v", id, err)
		http.Error(w, "could not delete profile", http.StatusInternalServerError)
		return
	}
	s.handleProfilesContent(w, r)
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
	agent, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return
	}
	prof, perr := s.ext.Privilege.GetProfile(ctx, profileID)
	// Remove the live RBAC first (best-effort), then drop the record.
	if perr == nil && s.ext.PrivilegeApply != nil {
		if err := s.ext.PrivilegeApply.RemoveGrant(ctx, agent.Name, agent.Namespace, prof.Name); err != nil {
			s.logger.Printf("privilege: remove RBAC for agent %d profile %d: %v", id, profileID, err)
		}
	}
	if err := s.ext.Privilege.Revoke(ctx, id, profileID); err != nil {
		s.logger.Printf("privilege: revoke profile %d from agent %d: %v", profileID, id, err)
		http.Error(w, "could not revoke", http.StatusInternalServerError)
		return
	}
	// Reflect the removed env/kubeconfig in the running pod (grant already dropped,
	// so reapply recomputes without it). Best-effort; RBAC was removed above.
	if perr == nil {
		s.reapplyEnvAsync(agent, prof)
	}
	s.renderAgentGrants(w, r, id)
}

// grantProfile applies the profile's RBAC live (if an applier is wired) then
// records the grant. Applying first means a failed apply doesn't leave a phantom
// grant. A nil applier (no in-cluster client) records the grant only. If the
// profile also carries env/kubeconfig and the agent is running, it re-applies
// those to the pod (a helm upgrade; the pod rolls) — RBAC needs no restart, env
// does. A stopped agent picks up the env at its next dispatch.
func (s *Server) grantProfile(ctx context.Context, agent agents.Agent, prof privilege.Profile, by string) error {
	if s.ext.PrivilegeApply != nil && prof.Spec.HasRBAC() {
		if err := s.ext.PrivilegeApply.ApplyGrant(ctx, agent.Name, agent.Namespace, prof); err != nil {
			return err
		}
	}
	if _, err := s.ext.Privilege.Grant(ctx, agent.ID, prof.ID, by); err != nil {
		return err
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
