package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/notes"
)

// --- Projects: the `project:` tag vocabulary ----------------------------------
//
// A project is NOT a column and NOT a table — it is a `project:<slug>` tag (see
// internal/notes/tags.go). So the project vocabulary is DERIVED from the tag
// vocabulary rather than stored separately, which is what keeps the two from ever
// disagreeing about what "in use" means. No migration, no new store method.

// projectsResponse is the wire shape of GET /api/projects. It is an OBJECT with a
// `projects` key rather than a bare array (unlike /api/tags) so the payload has
// room to grow without breaking the capture extension's parser, and the field is
// a non-nil slice so it always encodes as `[]` — a `null` there would break the
// extension's list handling.
type projectsResponse struct {
	Projects []notes.ProjectCount `json:"projects"`
}

// handleAPIProjects handles GET /api/projects (machine, hook-token-gated): the
// distinct project slugs currently in use with a task count each, ordered by
// count DESC then slug ASC. It backs the capture extension's project combobox.
//
// 🔑 COUNTING SEMANTICS — deliberate, and pinned by TestProjectsCountCompleteTasks
// / TestProjectsDropDismissedTasks:
//
//   - A task counts toward its project REGARDLESS OF STATUS, including
//     `complete`. This endpoint feeds a PICKER; a project whose tasks are all
//     finished is still a project you want to capture into, and dropping it would
//     make the project vanish from the picker exactly when you finish a batch of
//     work — forcing a re-type and re-fragmenting the vocabulary that slug
//     normalization exists to keep converged. It also matches `/api/tags`, the
//     sibling vocabulary endpoint, which has no status predicate either; two
//     vocabulary endpoints that disagreed would be a trap.
//   - There is no "dismissed" status in this codebase: dismissing a task DELETES
//     the row, so dismissed tasks stop counting structurally rather than via a
//     predicate.
//
// A store failure is a LOUD 500, never a silent empty list — a picker that shows
// "no projects" while the database is down teaches the user to re-type a project
// that already exists, which is precisely the fragmentation this feature avoids.
func (s *Server) handleAPIProjects(w http.ResponseWriter, r *http.Request) {
	vocab, err := s.ext.Notes.TagVocabulary(r.Context())
	if err != nil {
		s.logger.Printf("notes: api projects: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load projects"})
		return
	}
	s.writeJSON(w, http.StatusOK, projectsResponse{Projects: notes.ProjectsFromVocabulary(vocab)})
}

// projectMergeGuard closes the MERGE hole in the single-project rule.
//
// notes.ValidateTags enforces "at most one project:" over the tag set it is
// handed. That is the whole rule for REPLACE paths (POST /api/tasks, PATCH with
// `tags`), which submit the complete set. But the ADD paths merge: they validate
// only the INCOMING tags and then set-union them with what the task already
// carries — so a task holding `project:a` could accept `project:b` and end up
// with both, which is exactly the ambiguity the rule exists to prevent.
//
// This runs the SAME rule against the MERGED result. It only reads the task when
// the incoming set actually names a project, so ordinary tag adds keep their
// current single-statement cost.
//
// Two things it deliberately does NOT reject, so the guard cannot over-correct
// into "a project tag can never be touched again":
//   - re-adding the SAME project is idempotent;
//   - a REASSIGNMENT in one request — `removeTags:["project:a"]` together with
//     `addTags:["project:b"]` — is legal, so `removing` is subtracted from the
//     current state before the comparison. (The machine PATCH applies RemoveTags
//     BEFORE AddTags, so the row never transiently carries two project tags and
//     the FINAL state is single-project; rejecting it would be a wrong 400.)
func (s *Server) projectMergeGuard(ctx context.Context, id int64, incoming, removing []string) (int, string, bool) {
	next, ok := notes.ProjectName(incoming)
	if !ok {
		return 0, "", true
	}
	cur, err := s.ext.Notes.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return http.StatusNotFound, "task not found", false
		}
		s.logger.Printf("notes: project merge guard get %d: %v", id, err)
		return http.StatusInternalServerError, "could not load task", false
	}
	prev, has := notes.ProjectName(notes.SubtractTags(cur.Tags, removing))
	if !has || prev == next {
		return 0, "", true
	}
	return http.StatusBadRequest, fmt.Sprintf(
		"task already has %s:%s; remove it before adding %s:%s (a task may carry at most one project)",
		notes.NSProject, prev, notes.NSProject, next), false
}
