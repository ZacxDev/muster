package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/runbooks"
	"github.com/ZacxDev/muster/internal/ui"
)

// registerRunbookRoutes wires the runbooks registry (define/list/delete) and,
// when a provisioner is available, the dispatch action. Runbooks are a
// session/human surface mounted in the Agents tab.
// 🔴 NO EARLY RETURN ON A nil DEPENDENCY — see the package doc in server.go.
// Every handler below answers its own "nothing to serve from" case, so the
// recorded route set is a function of the CODE and never of the fixture.
func (s *Server) registerRunbookRoutes(mux Mux) {
	mux.HandleFunc("GET /ui/runbooks", s.requireSession(s.handleRunbooksContent))
	mux.HandleFunc("POST /runbooks", s.requireSession(s.handleRunbookCreate))
	mux.HandleFunc("DELETE /runbooks/{id}", s.requireSession(s.handleRunbookDelete))
	mux.HandleFunc("POST /runbooks/{id}/dispatch", s.requireSession(s.handleRunbookDispatch))
}

func (s *Server) handleRunbooksContent(w http.ResponseWriter, r *http.Request) {
	list, err := s.ext.Runbooks.ListRunbooks(r.Context())
	if err != nil {
		s.logger.Printf("runbooks: list: %v", err)
		http.Error(w, "could not load runbooks", http.StatusInternalServerError)
		return
	}
	s.renderRunbooks(w, r, list)
}

func (s *Server) handleRunbookCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "runbook name required", http.StatusBadRequest)
		return
	}
	spec, err := runbookSpecFromForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(spec.BodyTemplate) == "" {
		http.Error(w, "body template is required", http.StatusBadRequest)
		return
	}
	_, err = s.ext.Runbooks.CreateRunbook(r.Context(), runbooks.Runbook{
		Name:        name,
		DisplayName: strings.TrimSpace(r.FormValue("display_name")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Spec:        spec,
	})
	if err != nil {
		s.logger.Printf("runbooks: create %q: %v", name, err)
		http.Error(w, "could not create runbook (name may already exist)", http.StatusBadRequest)
		return
	}
	s.handleRunbooksContent(w, r)
}

// runbookSpecFromForm builds a runbook Spec from the create form. A non-empty
// raw `spec` field (advanced / programmatic) wins and is parsed whole;
// otherwise the spec is assembled from the discrete fields, with the structured
// arrays (params, steps) supplied as their own small JSON blobs and the profile
// list as a comma/newline-separated string.
func runbookSpecFromForm(r *http.Request) (runbooks.Spec, error) {
	if raw := strings.TrimSpace(r.FormValue("spec")); raw != "" {
		var spec runbooks.Spec
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			return runbooks.Spec{}, fmt.Errorf("spec is not valid JSON: %v", err)
		}
		return spec, nil
	}
	spec := runbooks.Spec{
		DefaultRepo:     strings.TrimSpace(r.FormValue("default_repo")),
		DefaultBranch:   strings.TrimSpace(r.FormValue("default_branch")),
		Model:           strings.TrimSpace(r.FormValue("model")),
		BodyTemplate:    r.FormValue("body_template"),
		GrantProfiles:   splitList(r.FormValue("grant_profiles")),
		SuccessCriteria: strings.TrimSpace(r.FormValue("success_criteria")),
	}
	if raw := strings.TrimSpace(r.FormValue("params")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &spec.Params); err != nil {
			return runbooks.Spec{}, fmt.Errorf("params is not a valid JSON array: %v", err)
		}
	}
	if raw := strings.TrimSpace(r.FormValue("steps")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &spec.Steps); err != nil {
			return runbooks.Spec{}, fmt.Errorf("steps is not a valid JSON array: %v", err)
		}
	}
	return spec, nil
}

// splitList splits a comma- or newline-separated string into trimmed,
// non-empty entries.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if t := strings.TrimSpace(f); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) handleRunbookDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.ext.Runbooks.DeleteRunbook(r.Context(), id); err != nil {
		s.logger.Printf("runbooks: delete %d: %v", id, err)
		http.Error(w, "could not delete runbook", http.StatusInternalServerError)
		return
	}
	s.handleRunbooksContent(w, r)
}

// handleRunbookDispatch renders the runbook's body with the submitted params,
// creates a task note for it, dispatches an agent bound to that note, and grants
// the runbook's privilege profiles to the agent. The agent then appears on the
// Agents tab (the response triggers its refresh).
func (s *Server) handleRunbookDispatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	rb, err := s.ext.Runbooks.GetRunbook(ctx, id)
	if err != nil {
		http.Error(w, "could not load runbook", http.StatusBadRequest)
		return
	}

	// Param values arrive as param_<name>; snapshot the declared ones.
	values := make(map[string]string, len(rb.Spec.Params))
	for _, p := range rb.Spec.Params {
		values[p.Name] = strings.TrimSpace(r.FormValue("param_" + p.Name))
	}

	if _, err := s.dispatchRunbook(ctx, rb, values, r.FormValue("repo")); err != nil {
		// A render/validation error is the user's to fix; report it verbatim.
		if errors.Is(err, errRunbookRender) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Printf("runbooks: dispatch %q: %v", rb.Name, err)
		http.Error(w, "could not dispatch agent", http.StatusInternalServerError)
		return
	}

	// Refresh the Agents list (and Tasks badge) so the new agent shows up.
	w.Header().Set("HX-Trigger", "agents:changed")
	s.handleRunbooksContent(w, r)
}

// errRunbookRender wraps a runbook render/validation failure (a bad-request the
// caller should surface to the user) so callers can distinguish it from an
// internal dispatch error.
var errRunbookRender = errors.New("runbook render")

// runbookDispatchResult is the outcome of a successful runbook dispatch.
type runbookDispatchResult struct {
	Agent  agents.Agent
	NoteID *int64
	Body   string
}

// dispatchRunbook renders rb with the given param values, binds the result to a
// task note, dispatches an agent on it, grants the runbook's privilege profiles
// to that agent, and records the run. repoOverride wins over the runbook's
// default repo when non-empty. Shared by the human dispatch form and the
// operator_run_runbook tool. A render/validation failure is returned wrapped in
// errRunbookRender.
func (s *Server) dispatchRunbook(ctx context.Context, rb runbooks.Runbook, values map[string]string, repoOverride string) (runbookDispatchResult, error) {
	body, err := rb.Render(values)
	if err != nil {
		return runbookDispatchResult{}, fmt.Errorf("%w: %v", errRunbookRender, err)
	}

	repo := strings.TrimSpace(repoOverride)
	if repo == "" {
		repo = rb.Spec.DefaultRepo
	}

	// Bind the rendered body to a task note so the run is tracked on the Tasks tab.
	var noteID *int64
	if s.ext.Notes != nil {
		if n, err := s.ext.Notes.Create(ctx, notes.Note{Body: body, Directory: rb.Name}); err == nil {
			noteID = &n.ID
		} else {
			s.logger.Printf("runbooks: create note for %q: %v", rb.Name, err)
		}
	}

	grantIDs := s.resolveProfileIDs(ctx, rb.Spec.GrantProfiles)

	agent, err := s.createAndDispatchAgent(ctx, dispatchParams{
		Repo:            repo,
		RepoBranch:      rb.Spec.DefaultBranch,
		Model:           rb.Spec.Model,
		NoteID:          noteID,
		NoteText:        body,
		Kickoff:         true,
		GrantProfileIDs: grantIDs,
	})
	if err != nil {
		return runbookDispatchResult{}, err
	}

	rbID := rb.ID
	if _, err := s.ext.Runbooks.CreateRun(ctx, runbooks.Run{
		RunbookID:    &rbID,
		RunbookName:  rb.Name,
		AgentID:      agent.ID,
		NoteID:       noteID,
		Params:       values,
		RenderedBody: body,
	}); err != nil {
		s.logger.Printf("runbooks: record run for %q: %v", rb.Name, err)
	}

	// A runbook run successfully dispatched an agent: count it (usage of the
	// parameterized-dispatch loop).
	metrics.RunbookRuns.Inc()

	return runbookDispatchResult{Agent: agent, NoteID: noteID, Body: body}, nil
}

// resolveProfileIDs maps privilege profile names to their IDs, skipping unknown
// names (logged). Returns nil when the privilege store is unset.
func (s *Server) resolveProfileIDs(ctx context.Context, names []string) []int64 {
	if len(names) == 0 || s.ext.Privilege == nil {
		return nil
	}
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		prof, err := s.ext.Privilege.GetProfileByName(ctx, name)
		if err != nil {
			s.logger.Printf("runbooks: unknown privilege profile %q referenced; skipping", name)
			continue
		}
		ids = append(ids, prof.ID)
	}
	return ids
}

func (s *Server) renderRunbooks(w http.ResponseWriter, r *http.Request, list []runbooks.Runbook) {
	// Best-effort run stats (count + most-recent) for the cards, batched into a
	// single query for all runbooks. On error we degrade to no badges (like the
	// per-runbook lookup did when it failed).
	ids := make([]int64, len(list))
	for i, rb := range list {
		ids[i] = rb.ID
	}
	stats, err := s.ext.Runbooks.RunStatsForRunbooks(r.Context(), ids)
	if err != nil {
		s.logger.Printf("runbooks: run stats: %v", err)
		stats = nil
	}
	views := runbookViews(list, stats)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderRunbooks(w, views); err != nil {
		s.logger.Printf("runbooks: render: %v", err)
	}
}

// runbookViews projects runbooks into their card views, folding the batched run
// stats into each (count 0 / zero time when a runbook has no recorded runs). It
// is pure (no I/O) so the stats→view mapping is unit-testable.
func runbookViews(list []runbooks.Runbook, stats map[int64]runbooks.RunStats) []ui.RunbookView {
	views := make([]ui.RunbookView, 0, len(list))
	for _, rb := range list {
		v := runbookView(rb)
		if st, ok := stats[rb.ID]; ok {
			v.Runs = st.Count
			v.LastRunAt = st.LastRunAt
		}
		views = append(views, v)
	}
	return views
}

// runbookView projects a runbook into its render-side view, including the param
// form fields and a short summary of what it dispatches.
func runbookView(rb runbooks.Runbook) ui.RunbookView {
	params := make([]ui.RunbookParam, 0, len(rb.Spec.Params))
	for _, p := range rb.Spec.Params {
		label := p.Label
		if label == "" {
			label = p.Name
		}
		params = append(params, ui.RunbookParam{
			Name: p.Name, Label: label, Description: p.Description,
			Required: p.Required, Default: p.Default, Enum: p.Enum,
		})
	}
	return ui.RunbookView{
		ID: rb.ID, Name: rb.Name, DisplayName: rb.DisplayName,
		Description: rb.Description, Summary: runbookSummary(rb),
		Repo: rb.Spec.DefaultRepo, Params: params,
	}
}

// runbookSummary is a short human description of what a runbook dispatches.
func runbookSummary(rb runbooks.Runbook) string {
	var parts []string
	if rb.Spec.DefaultRepo != "" {
		parts = append(parts, rb.Spec.DefaultRepo)
	}
	if n := len(rb.Spec.Params); n > 0 {
		parts = append(parts, strconv.Itoa(n)+" param"+plural(n))
	}
	if n := len(rb.Spec.Steps); n > 0 {
		parts = append(parts, strconv.Itoa(n)+" step"+plural(n))
	}
	if n := len(rb.Spec.GrantProfiles); n > 0 {
		parts = append(parts, strconv.Itoa(n)+" grant"+plural(n))
	}
	if len(parts) == 0 {
		return "no params"
	}
	return strings.Join(parts, " · ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
