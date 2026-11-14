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
// when one points at the task, with the reason the refusal names. Each row
// belongs to its owner's personal state (an agent, a host dispatch, a
// plugin's queue or message) or carries its own same-owner rule that a move
// would silently break, so the task stays where it is rather than leave a
// link pointing across owners.
var MoveRefusals = map[string]string{
	"agents.note_id":          "an agent works on it (agents are not shared yet)",
	"attention_asks.task_id":  "an ask is linked to it",
	"host_dispatches.task_id": "a host dispatch is linked to it",
	"plugin_messages.task_id": "a plugin message is linked to it",
	"session_routes.task_id":  "a session route is linked to it",
	"task_ref_writes.note_id": "writes are queued for its plugin",
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
