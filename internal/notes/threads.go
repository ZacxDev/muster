package notes

import (
	"errors"
	"time"
)

// Task-thread roles , MONOTONIC in this order: a link's role can
// only ever move UP the list, never back down.
//
//   - RoleRead    — the session fetched the task (GET /api/tasks/{id}).
//   - RoleWorked  — the session commented on it or changed its status.
//   - RoleCreated — the session filed it (POST /api/tasks, or the schema backfill
//     from notes.source_session_id). TERMINAL: nothing overwrites it.
//
// The direction matters more than the vocabulary: a session that works a task and
// later re-reads it must not be demoted to "read", because the thread is read as
// a record of who DID something, and a demotion would silently erase that.
const (
	RoleRead    = "read"
	RoleWorked  = "worked"
	RoleCreated = "created"
)

// roleRank orders the roles for the monotonic upgrade. It is the Go MIRROR of the
// `array_position(ARRAY['read','worked','created'], …)` expression in
// PGStore.LinkSession — both must stay in the same order, which
// TestRoleRankMatchesSQLOrdering pins.
//
// An unknown role ranks -1, i.e. below every real role, so it can never win an
// upgrade comparison. It cannot reach the database anyway: ValidRole rejects it at
// the store boundary and the CHECK constraint rejects it at the column.
func roleRank(role string) int {
	switch role {
	case RoleRead:
		return 0
	case RoleWorked:
		return 1
	case RoleCreated:
		return 2
	}
	return -1
}

// ValidRole reports whether role is one of the three task-thread roles.
func ValidRole(role string) bool { return roleRank(role) >= 0 }

// UpgradeRole returns the role a link should carry after a touch that claims
// `next`, given that it currently carries `cur`. It is the pure spelling of the
// monotonic rule and is what the in-memory fake uses; the Postgres path spells the
// same rule as one SQL expression rather than calling this, because the upsert has
// to be a single statement.
func UpgradeRole(cur, next string) string {
	if roleRank(next) > roleRank(cur) {
		return next
	}
	return cur
}

// MaxTaskSessions is the ADVISORY, BEST-EFFORT size of one task's thread: how many
// distinct sessions it keeps before the oldest `read` links start being evicted.
//
// 🔴 IT IS NOT A BOUND ON ATTACKER-INFLUENCED INPUT, and must not be described as
// one. It is enforced AFTER the write, by PGStore.trimThread, and the count is
// therefore racy under concurrency — 49 existing links plus 12 concurrent writers
// was measured to leave 57 rows. That is deliberate and acceptable, because the cap
// no longer decides whether a request succeeds: it only decides how much breadcrumb
// history one task keeps, and the next link on that task trims it back down.
//
// The number is a HOUSEKEEPING size, not a security limit: a task genuinely handed
// between sessions over weeks might accumulate a dozen, so fifty is generous, and
// something at 500 is looping rather than working. TestMaxTaskSessionsMagnitude
// pins the literal, because every other cap test loops on this constant and would
// pass identically at 1 or at 100000.
//
// 🔴 IT IS NOT A CEILING ON `created`/`worked` LINKS, and that asymmetry is
// deliberate. Only `read` links are evictable, so a task whose thread is genuinely
// full of work records will EXCEED this number rather than drop one of them: losing
// the record of who worked a task is a real loss, and the cap is only housekeeping.
// A new `read` breadcrumb in that situation is skipped instead (ErrTooManySessions).
// Trading the cap for a work record is the right way round; do not "fix" the
// overflow by making work evictable.
//
// 🔴 A PROVENANCE BREADCRUMB MUST NEVER BREAK THE OPERATION IT ANNOTATES. An
// earlier version enforced this as a hard bound that answered 409, which produced a
// split write on PATCH …/status (the status changed AND the caller was told it
// failed) and a permanent 409 on plain GETs once fifty sessions had read a task.
// No task-scoped route may fail because of this cap. See PGStore.LinkSession.
const MaxTaskSessions = 50

// ErrTooManySessions reports that a link was SKIPPED — the thread was already at
// MaxTaskSessions and every existing link was `created`/`worked`, so there was
// nothing evictable and the new `read` breadcrumb was dropped.
//
// 🔴 IT IS NOT A REQUEST FAILURE. Callers LOG AND COUNT it and then serve the
// request exactly as if the link had been recorded. Nothing maps it to an HTTP
// status.
var ErrTooManySessions = errors.New("task session link skipped: thread is full of created/worked links")

// SessionLink is one edge of a task's thread: a coding-agent session that touched
// the task, in what capacity, and when it first and last did so.
//
// 🔴 Project / Cwd / Host are DENORMALISED COPIES, snapshotted when the link is
// written — not a join. The session-transcript record they came from is reaped at
// 14 days, so a link that borrowed its context live would go blank exactly when the
// thread becomes the only remaining record of the work. See the 0023 migration
// header.
type SessionLink struct {
	SessionID string `json:"sessionId"`
	Role      string `json:"role"`
	Project   string `json:"project,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Host      string `json:"host,omitempty"`
	// FirstSeenAt is the moment this session first touched the task and NEVER
	// moves again; LastSeenAt moves on every touch.
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	// DetailAvailable reports whether a transcript record for this session still
	// exists RIGHT NOW — i.e. whether the other service can still render it.
	//
	// 🔴 NOTHING IN THIS PACKAGE EVER SETS IT, AND THAT IS STRUCTURAL, NOT AN
	// OVERSIGHT. The transcript records live in another service's database, so
	// reading them from here — the LEFT JOIN that used to fill this field — is
	// exactly the cross-service dependency this package was carved out to remove.
	// Every read this package performs therefore returns the ZERO VALUE here.
	//
	// 🔴 SO A CONSUMER THAT DOES NOT COMPOSE IT RENDERS A LIE, SILENTLY. false means
	// "no transcript" to the renderer, and it says so in words over a session whose
	// transcript is perfectly alive — no error, no log line, on every surface at
	// once. The composition lives at the API layer, in ONE place, wrapped around the
	// store so a handler cannot opt out: see api.withSessionLiveness. A future
	// consumer outside that process composes it from GET /api/sessions/{id}.
	//
	// 🔴 A session must still render as a row when this is false, because "this task
	// was worked by a session whose transcript is gone" and "this task was never
	// worked" are completely different facts about the task.
	DetailAvailable bool `json:"detailAvailable"`
	// DetailSeen reports whether a transcript record for this session was OBSERVED
	// at least once while this link existed . MONOTONIC: false →
	// true, never back — the sweep deleting the record does not make it untrue that
	// a transcript existed. Its observations arrive as LinkSession's detailSeen
	// argument; nothing in this package looks them up.
	//
	// 🔴 IT IS THE OTHER HALF OF THE THIRD STATE, AND DetailAvailable ALONE CANNOT
	// SUPPLY IT. One bit cannot separate "recorded, then reaped at 14 days" from
	// "never recorded at all", and the renderer used to resolve every absence to the
	// first — printing "transcript expired · kept for 14 days" over sessions minutes
	// old. That was not a cosmetic slip: the transcript record has exactly ONE
	// producer, that producer was almost entirely dead for two months, and the
	// overwhelming majority of links had no transcript record at all — so the false
	// sentence was the COMMON case, and it actively disguised the outage as ordinary
	// retention.
	//
	//	DetailAvailable=true                → link to the transcript
	//	false + DetailSeen=true             → "transcript expired" (retention sweep)
	//	false + DetailSeen=false            → "no transcript recorded" (never posted)
	//
	// 🔴 DO NOT REPLACE THIS WITH A TIMESTAMP HEURISTIC. LastSeenAt is written by the
	// API tier when a session touches a TASK; the transcript record is written by the
	// producer of transcripts at the end of a TURN. Independent producers,
	// independent clocks,
	// neither bounding the other — comparing LastSeenAt against the retention window
	// produces a confident answer and the wrong one.
	DetailSeen bool `json:"detailSeen"`
}

// TaskLink is the REVERSE view — one task a given session touched — backing
// GET /api/sessions/{id}/tasks. It carries the task's identity and lifecycle
// state so a caller (a /handoff writer, say) can render the list without a second
// round trip per task.
type TaskLink struct {
	NoteID      int64     `json:"id"`
	Title       string    `json:"title"`
	Directory   string    `json:"directory"`
	Status      string    `json:"status"`
	Role        string    `json:"role"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
}

// groupSessionLinks assigns each link to its parent note by note id, preserving
// input order (the batched query already orders by note_id, first_seen_at,
// session_id). It mirrors groupNoteChildren and is kept separate only because the
// batched sessions query returns its own (noteID, link) pairing.
type noteSessionLink struct {
	NoteID int64
	Link   SessionLink
}

func groupSessionLinks(notes []Note, links []noteSessionLink) {
	idx := make(map[int64]int, len(notes))
	for i := range notes {
		idx[notes[i].ID] = i
		// Reset so re-grouping is idempotent and nil-safe.
		notes[i].Sessions = nil
	}
	for _, l := range links {
		if i, ok := idx[l.NoteID]; ok {
			notes[i].Sessions = append(notes[i].Sessions, l.Link)
		}
	}
}
