package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// THE OWNER'S RETRY AND DISMISS OF A FAILED REFERENCE WRITE
// (docs/plugins.md#writes-on-a-reference-are-queued; migration 0074).
//
// A write queued on a plugin task reference (task_ref_queue.go) that FAILED —
// the plugin reported a failure, it was handed out RefWriteMaxAttempts times
// without a result, or its task stopped being the plugin's reference — used to
// stay on the task page for ever. Its owner can now:
//
//   - RETRY it: back to pending at its own id (attempts 0, error and lease
//     cleared), handed to the plugin that holds the reference now, in the
//     task's order (notes.PGStore.RetryRefWrite, ClaimRefWrites);
//   - DISMISS it: off the task page (dismissed_at; the row is kept).
//
// Two doors per action onto ONE body (actOnRefWrite):
//
//   - POST /tasks/{id}/ref-writes/{wid}/retry|dismiss — the task page's
//     buttons, behind requireOperatorWrite (a session and same-origin), like
//     the plugin message retry's form. htmx gets the re-rendered card; a plain
//     form post a 303 back to the task page.
//   - POST /api/tasks/{id}/ref-writes/{wid}/retry|dismiss — the machine API,
//     behind requireHookToken like the task's other /api sub-resources (a
//     personal token acts as its owner). JSON: the write as it now stands.
//
// 🔴 NOT AN AGENT'S, NOT A PLUGIN'S. Neither door admits an agent's own token,
// the chief's service identity, or a plugin token, and no agent tool calls
// actOnRefWrite (TestRefWriteRetryAndDismissAreTheOwnersOnly): retrying what the
// upstream refused is the owner's decision, not the writer's.
//
// 🔴 ONLY A FAILED WRITE: anything else is 409 and nothing changes. A missing
// task, a write on another task, and another owner's write are all 404 (RLS
// hides the row; the answer is the ghost's).
// ---------------------------------------------------------------------------

// refWriteAction is which owner action actOnRefWrite performs.
type refWriteAction string

const (
	refWriteRetry   refWriteAction = "retry"
	refWriteDismiss refWriteAction = "dismiss"
)

// actOnRefWrite is the action: the status code, the write as it now stands,
// and the refusal's sentence ("" on success).
func (s *Server) actOnRefWrite(ctx context.Context, action refWriteAction, taskID, writeID int64) (int, notes.RefWrite, string) {
	if s.ext.Notes == nil {
		return http.StatusServiceUnavailable, notes.RefWrite{}, "this server has no task store"
	}
	if _, err := s.ext.Notes.Get(ctx, taskID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return http.StatusNotFound, notes.RefWrite{}, "task not found"
		}
		s.logger.Printf("tasks: %s write %d on task %d: load the task: %v", action, writeID, taskID, err)
		return http.StatusInternalServerError, notes.RefWrite{}, "could not load task"
	}
	var (
		wr  notes.RefWrite
		err error
	)
	switch action {
	case refWriteRetry:
		wr, err = s.ext.Notes.RetryRefWrite(ctx, taskID, writeID)
	default:
		wr, err = s.ext.Notes.DismissRefWrite(ctx, taskID, writeID)
	}
	switch {
	case err == nil:
	case errors.Is(err, notes.ErrRefWriteNotFound):
		return http.StatusNotFound, notes.RefWrite{}, "no such write on this task"
	case errors.Is(err, notes.ErrRefWriteNotFailed):
		return http.StatusConflict, notes.RefWrite{}, "only a failed write can be retried or dismissed"
	case errors.Is(err, notes.ErrNotReference):
		return http.StatusConflict, notes.RefWrite{}, "the task is no longer a reference, so the write has nowhere to go; dismiss it instead"
	default:
		s.logger.Printf("tasks: %s write %d on task %d: %v", action, writeID, taskID, err)
		return http.StatusInternalServerError, notes.RefWrite{}, "the " + string(action) + " could not be recorded"
	}
	s.logger.Printf("tasks: %s write %d (%s on task %d) by its owner: now %s", action, wr.ID, wr.Kind, wr.NoteID, wr.State)
	if action == refWriteRetry {
		s.refWriteWake.signal()
	}
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(taskID, 10))
	return http.StatusOK, wr, ""
}

// refWriteIDs reads {id} and {wid}; false when either is not an id.
func refWriteIDs(r *http.Request) (taskID, writeID int64, ok bool) {
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || taskID <= 0 {
		return 0, 0, false
	}
	writeID, err = strconv.ParseInt(r.PathValue("wid"), 10, 64)
	if err != nil || writeID <= 0 {
		return 0, 0, false
	}
	return taskID, writeID, true
}

func (s *Server) handleTaskRefWriteRetry(w http.ResponseWriter, r *http.Request) {
	s.serveRefWriteForm(w, r, refWriteRetry)
}

func (s *Server) handleTaskRefWriteDismiss(w http.ResponseWriter, r *http.Request) {
	s.serveRefWriteForm(w, r, refWriteDismiss)
}

func (s *Server) handleAPITaskRefWriteRetry(w http.ResponseWriter, r *http.Request) {
	s.serveRefWriteAPI(w, r, refWriteRetry)
}

func (s *Server) handleAPITaskRefWriteDismiss(w http.ResponseWriter, r *http.Request) {
	s.serveRefWriteAPI(w, r, refWriteDismiss)
}

// serveRefWriteForm is the task page's door: the re-rendered card for htmx, a
// 303 back to the task page for a plain form post, the refusal as text.
func (s *Server) serveRefWriteForm(w http.ResponseWriter, r *http.Request, action refWriteAction) {
	taskID, writeID, ok := refWriteIDs(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	code, _, problem := s.actOnRefWrite(r.Context(), action, taskID, writeID)
	if problem != "" {
		http.Error(w, problem, code)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/tasks/"+strconv.FormatInt(taskID, 10), http.StatusSeeOther)
		return
	}
	note, err := s.ext.Notes.Get(r.Context(), taskID)
	if err != nil {
		s.logger.Printf("notes: get %d after a write %s: %v", taskID, action, err)
		http.Error(w, "could not load task", http.StatusInternalServerError)
		return
	}
	s.renderNoteCard(w, r, note)
}

// serveRefWriteAPI is the machine door: 200 with the write as it now stands,
// or {"error"} with the refusal's code.
func (s *Server) serveRefWriteAPI(w http.ResponseWriter, r *http.Request, action refWriteAction) {
	taskID, writeID, ok := refWriteIDs(r)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such write on this task"})
		return
	}
	code, wr, problem := s.actOnRefWrite(r.Context(), action, taskID, writeID)
	if problem != "" {
		s.writeJSON(w, code, map[string]any{"error": problem})
		return
	}
	s.writeJSON(w, code, wr)
}
