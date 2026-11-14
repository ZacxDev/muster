package notes

import (
	"errors"
	"strings"
)

// --- Moving a task into a team (team sharing phase 4, slice 4.3) -----------
//
// A task moves from its owner (a person) into a team they operate in: ONE
// transaction under the system scope, because owner_id never changes on a
// share-class table outside it (migration 0127's owner_fixed). The handler
// (internal/api handleTaskMove) decides who may — the caller owns the task
// and is at least an operator in the team — and hands the store the SYSTEM
// scope to move with (this package imports no authz:
// TestNotesImportsOnlyItsTwoDependencies). PGStore.MoveToTeam, MoveChildren
// and the move's SQL live in pgstore.go, the one file that may name
// note_comments (TestNoteCommentsQueriedOnlyByPGStore).
//
// 🔴 EVERY FOREIGN KEY ONTO notes IS CLASSIFIED, in MoveChildren (pgstore.go)
// or here, as moved or as a named refusal. notes.TestEveryForeignKeyOntoNotesIsClassified reads the
// database's own catalog and fails when a key appears that is in neither
// ledger, or when a ledger names a key that no longer exists.

// MoveRefusals are the foreign keys onto notes whose rows REFUSE the move
// when a LIVE one points at the task, with the reason the refusal names and
// what "live" means for that table. Each row belongs to its owner's personal
// state (an agent, a host dispatch, a plugin's queue or message) or carries
// its own same-owner rule that a move would silently break, so the task stays
// where it is rather than leave a live link pointing across owners.
//
// 🔴 A FINISHED ROW DOES NOT REFUSE (team sharing phase 4, slice 4.7, closing
// slice 4.3's limitation (a): a task any agent had ever worked on could never
// move). A row whose Live predicate is false stays where it is — its owner's,
// pointing at a task that is now the team's — because nothing acts through it
// any more: an ask that is answered (its delivery settled), cancelled or
// expired; a session route that is answered back, relayed, timed out, fell
// back or failed; a host dispatch that ended. Each of those four keys is a
// SIMPLE key onto notes(id), so the row's owner is not tied to the task's and
// the move cannot break it; the task's new owner (the team) does not see the
// row (it is its old owner's, under row-level security), and its old owner
// still does. Live "" is "every row refuses":
//   - agents.note_id is a COMPOSITE key onto notes(id, owner_id) — an agent's
//     row, stopped or errored included, would fail the move's own statement
//     (23503) and cannot be moved with the task (its credentials, namespace
//     and account are its owner's); a destroyed agent has no row at all;
//   - plugin_messages and task_ref_writes are a plugin's queue: still refused.
var MoveRefusals = map[string]MoveRefusal{
	"agents.note_id": {Reason: "an agent works on it (agents are not shared yet)"},
	"attention_asks.task_id": {Reason: "an ask is linked to it",
		Live: "((status = 'open' AND expires_at > now()) OR delivery IN ('queued', 'delivering'))"},
	"host_dispatches.task_id": {Reason: "a host dispatch is linked to it", Live: "status <> 'ended'"},
	"plugin_messages.task_id": {Reason: "a plugin message is linked to it"},
	"session_routes.task_id":  {Reason: "a session route is linked to it", Live: "state IN ('routed', 'answered')"},
	"task_ref_writes.note_id": {Reason: "writes are queued for its plugin"},
}

// MoveRefusal is one MoveRefusals entry: the sentence the refusal names, and
// the SQL predicate over that table's row that makes it LIVE — refusing —
// ("" for every row). A constant of this file, never input.
type MoveRefusal struct {
	Reason string
	Live   string
}

// ErrMoveNotYours is a move of a task the caller does not own (or that does
// not exist, or is dismissed): nothing moved.
var ErrMoveNotYours = errors.New("notes: that task is not yours to move")

// MoveBlockedError is a move refused because rows of another owner would
// point at the task afterwards. Links names each, as MoveRefusals says it,
// with how many rows.
type MoveBlockedError struct{ Links []string }

func (e *MoveBlockedError) Error() string {
	return "notes: the task cannot move: " + strings.Join(e.Links, "; ")
}
