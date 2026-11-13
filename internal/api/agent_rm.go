package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// handleAPIAgentRemove is DELETE /api/agents/{name} (`muster agent rm`): the
// agent's teardown through destroyAgent — the same protections (the chief, a
// declared officer's instance) and the same background teardown as the
// agent page's Delete. A host officer instance's row (left from a moved or
// undeclared officer) is removed with no provisioner (destroyAgent's
// host-row branch); any other agent needs one, and is refused 503 without.
func (s *Server) handleAPIAgentRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.ext.Agents == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no agents store"})
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	a, err := s.ext.Agents.GetByName(ctx, name)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such agent: " + name})
		return
	}
	if a.Provisioned() && !s.lifecycleServed() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no agent provisioner, so it cannot tear down " + name +
			" (only a host officer instance's row is removed without one)", ProvisionerUnwiredField: true})
		return
	}
	destroy := func(ctx context.Context, id int64) error { return s.ext.Provisioner.Destroy(ctx, id) }
	var chief errDestroyChief
	_, err = s.destroyAgent(ctx, a.ID, destroy, destroyHooks{})
	switch {
	case err == nil:
	case errors.As(err, &chief):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": chief.Error()})
		return
	default:
		s.logger.Printf("agents: api remove %s: %v", name, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not check whether the agent may be removed; nothing was removed"})
		return
	}
	teardown := "provisioner"
	if !a.Provisioned() {
		teardown = "row"
	}
	s.writeJSON(w, http.StatusAccepted, map[string]any{"removing": a.Name, "id": a.ID, "teardown": teardown})
}
