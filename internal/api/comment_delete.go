package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/jackc/pgx/v5"
)

// Task-comment retraction (migration 0021). Lives in its own file rather than in
// notes.go so it does not collide with concurrent work in that file or in
// server.go's retention region.
//
// 🔴 WHY SOFT. A comment thread is where a task's investigation lives — the same
// argument migration 0019 made for the task row itself. A hard DELETE of a
// comment is unrecoverable inside the daily-backup window (the CronJob runs
// 04:30, so a comment posted and retracted the same day is in NO snapshot), and
// the thing being retracted is usually a mistake worth being able to look at
// again. The row stays; only its visibility goes.

// errCommentTaskNotFound distinguishes "the TASK does not exist (or is
// dismissed)" from "the COMMENT does not exist", so each caller can render the
// right 404 body. pgx.ErrNoRows out of softDeleteTaskComment always means the
// COMMENT.
var errCommentTaskNotFound = errors.New("task not found")

// errCommentNotYours is a live comment on a team's task that the caller may
// not retract (commentRetractAllowed): both callers answer 403.
var errCommentNotYours = errors.New("on a team task, only the comment's author or a team manager can retract it")

// commentRetractAllowed reports whether p may retract comment cid on n. On a
// task p owns (or one whose owner the store does not read, 0), any comment,
// as before team tasks. On a team's task, the comment's author, or a manager
// of the team; a comment with no author_id (system scope, or written before
// 0127) only a manager. A comment not in n's live thread is not decided here:
// the store answers it (404), so this is no oracle for comment ids.
func commentRetractAllowed(p authz.Principal, n notes.Note, cid int64) bool {
	if n.OwnerID == 0 || n.OwnerID == p.UserID || taskRoleAllows(p, n.OwnerID, authz.TeamManager) {
		return true
	}
	for _, c := range n.Comments {
		if c.ID == cid && !c.Retracted {
			return c.AuthorID != 0 && c.AuthorID == p.UserID
		}
	}
	return true
}

// softDeleteTaskComment is the shared core behind BOTH retraction paths — the
// machine DELETE /api/tasks/{id}/comments/{cid} and the session/UI
// DELETE /tasks/{id}/comments/{cid} — so the two can never diverge on scoping,
// side-effects, or which id a 404 refers to.
//
// The parent Get probe is load-bearing for the SAME reason it is on
// handleAPITaskDelete: it is what makes "unknown task" a 404 rather than the
// comment-shaped 404 the store's ErrNoRows would otherwise produce, and it is
// what refuses a retraction against a dismissed task.
//
// On a team's task it also refuses, before the store is asked, a comment the
// caller did not write unless they manage the team (commentRetractAllowed);
// SoftDeleteComment holds the same rule in SQL.
//
// It returns errCommentTaskNotFound (caller → 404 "task not found"),
// errCommentNotYours (caller → 403), pgx.ErrNoRows (caller → 404 "comment not
// found": unknown comment id, a comment belonging to a different task, or one
// already retracted), or any other store error (caller → 500).
func (s *Server) softDeleteTaskComment(ctx context.Context, taskID, commentID int64) error {
	n, err := s.ext.Notes.Get(ctx, taskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errCommentTaskNotFound
		}
		return err
	}
	if p, ok := authz.From(ctx); ok && !commentRetractAllowed(p, n, commentID) {
		return errCommentNotYours
	}
	if err := s.ext.Notes.SoftDeleteComment(ctx, taskID, commentID); err != nil {
		return err
	}
	// Broadcast task.changed so every OTHER open Tasks tab drops the retracted
	// comment too. The session path additionally swaps the card for the client that
	// asked; the machine path has no swap at all, so without this a retraction made
	// from a phone would stay visible on a laptop until the next refresh.
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(taskID, 10))
	return nil
}

// handleAPITaskCommentDelete handles DELETE /api/tasks/{id}/comments/{cid}
// (machine, hook-token-gated): retract one comment from a task's thread.
//
// ROUTE SHAPE. It is the DELETE twin of POST /api/tasks/{id}/comments, which is
// the only existing comment route — a comment is addressed as a child of its task
// everywhere else in this API (POST /api/tasks/{id}/comments, GET
// /tasks/{id}/attachments/{aid}), and nothing addresses a comment by a bare
// global id. A flat /api/comments/{cid} would have been a second addressing
// convention AND would drop the task scope that makes a mistyped id a 404 instead
// of a hit on someone else's thread.
//
// 🔴 AUTH. requireHookToken, NOT requireSession — because the CALLER is a
// machine. This is the route an external producer (repo-cos, the drafter, CI, a
// CC session) uses to retract what it posted, and none of them can complete a
// browser login to get a session cookie. The session-tier twin below
// (handleNoteCommentDelete) is the human spelling of the same operation.
//
// ⚠ requireHookToken is the shared machine secret: every hook script holds it,
// and it names no caller. With no MUSTER_HOOK_TOKEN it admits nobody (and the
// server does not boot).
//
// 🔴 STATUS CODES, PINNED (mirroring DELETE /api/tasks/{id}): bad task or comment
// id → 400; unknown or dismissed task → 404 "task not found"; unknown comment,
// a comment on a different task, or an ALREADY-retracted comment → 404 "comment
// not found" (idempotent-by-404, exactly as a repeat task delete answers); a
// team task's comment the caller may not retract → 403; store failure → 500;
// no/bad hook token → 401 (middleware). Always JSON — never the
// htmx card fragment the session route returns.
func (s *Server) handleAPITaskCommentDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad id"})
		return
	}
	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad comment id"})
		return
	}
	switch err := s.softDeleteTaskComment(r.Context(), id, cid); {
	case err == nil:
	case errors.Is(err, errCommentTaskNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
		return
	case errors.Is(err, errCommentNotYours):
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": errCommentNotYours.Error()})
		return
	case errors.Is(err, pgx.ErrNoRows):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "comment not found"})
		return
	default:
		s.logger.Printf("notes: api comment delete %d/%d: %v", id, cid, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not delete comment"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": cid, "taskId": id, "deleted": true})
}

// handleNoteCommentDelete handles DELETE /tasks/{id}/comments/{cid} (session):
// the UI path behind the per-comment trash control, re-rendering the single card
// so the retracted comment disappears in place.
//
// 🔴 IT IS THE SESSION-TIER TWIN OF THE HOOK-TOKEN ROUTE ABOVE, and that is a
// deliberate, flagged trade-off — the SAME pairing DELETE /tasks/{id} (session)
// and DELETE /api/tasks/{id} (hook token) have had since the machine route was
// added. The reason it exists is that the browser carries no BEARER token: a
// signed-in operator presents a session cookie, and the only way a card button
// could satisfy requireHookToken is for the server to render
// MUSTER_HOOK_TOKEN into the page — handing the token that guards /api/send
// and all of /api/tasks* to any XSS, and to a document that signing out cannot
// revoke. That is strictly worse than this route.
//
// ⚠ WHAT CHANGED, AND WHAT DID NOT. This route is no longer reachable without
// credentials: requireSession refuses a caller with no valid signed cookie, so
// the trade-off is now "a signed-in operator may retract a comment" rather than
// "anyone on the LAN may". The TIER argument above is unaffected — the browser
// still has no hook token, and rendering one is still the worse option. If the
// operator would rather have no UI control at all, delete this handler and its
// one registration in server.go; the machine route is unaffected.
//
// Bad task or comment id → 400; unknown/dismissed task or unknown/already-
// retracted comment → 404 (the session path does not distinguish the two — it
// renders no body); a team task's comment the caller did not write, unless
// they manage the team → 403; store failure → 500; otherwise the re-rendered
// card.
func (s *Server) handleNoteCommentDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil {
		http.Error(w, "bad comment id", http.StatusBadRequest)
		return
	}
	switch err := s.softDeleteTaskComment(r.Context(), id, cid); {
	case err == nil:
	case errors.Is(err, errCommentTaskNotFound), errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
		return
	case errors.Is(err, errCommentNotYours):
		http.Error(w, errCommentNotYours.Error(), http.StatusForbidden)
		return
	default:
		s.logger.Printf("notes: comment delete %d/%d: %v", id, cid, err)
		http.Error(w, "could not delete comment", http.StatusInternalServerError)
		return
	}
	note, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		s.logger.Printf("notes: get %d after comment delete: %v", id, err)
		http.Error(w, "could not load task", http.StatusInternalServerError)
		return
	}
	s.renderNoteCard(w, r, note)
}
