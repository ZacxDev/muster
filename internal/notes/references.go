package notes

import (
	"context"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// PLUGIN TASK REFERENCES (migration 0072; docs/plugins.md#task-references).
//
// A task an upstream system owns is ONE reference row here, not a copy. The
// upstream is canonical: this package stores the key, the last snapshot the
// plugin pushed (rendered read-only) and when it was applied. Every status or
// comment a person or agent writes on a reference is QUEUED as a RefWrite for
// the plugin that owns the reference to apply upstream — muster never calls a
// plugin — and the row's own title/body/status change only when a snapshot is
// applied.
//
// 🔴 WHAT MAKES A ROW A REFERENCE IS notes.ext_key, AND NOTHING ELSE. Note.Ref is
// non-nil exactly when that column is; the api tier's one predicate
// (api.queueIfReference) asks the store through EnqueueRefWrite, whose INSERT
// is conditional on that same column, so "is this a reference" has one
// spelling in SQL and one caller in Go.
//
// 🔴 THE SNAPSHOT'S STATUS IS ONE OF MUSTER'S FOUR, AND ITS LABEL IS OPAQUE.
// Mapping an upstream vocabulary onto open|in_progress|ready_for_review|complete
// is the plugin's job; muster never learns the upstream's words, only a display
// label it shows verbatim.
// ---------------------------------------------------------------------------

// Reference is a reference row's upstream half.
type Reference struct {
	// Key is the plugin task key (`<namespace>:<id>`), notes.ext_key.
	Key string `json:"key"`
	// PluginID is the plugin that holds the reference (it created, converted or
	// adopted it): the only plugin its queued writes are handed to.
	PluginID int64 `json:"pluginId,omitempty"`
	// Snapshot is the last APPLIED snapshot.
	Snapshot RefSnapshot `json:"snapshot"`
	// SyncedAt is when that snapshot was applied (not when the upstream changed:
	// that is Snapshot.UpdatedAt).
	SyncedAt time.Time `json:"syncedAt"`
}

// RefSnapshot is the upstream's view of one task, as the plugin pushed it,
// after the route's caps (truncate, never reject).
type RefSnapshot struct {
	Title       string       `json:"title"`
	URL         string       `json:"url,omitempty"`
	Status      string       `json:"status"`
	StatusLabel string       `json:"status_label,omitempty"`
	Rev         string       `json:"rev,omitempty"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Body        string       `json:"body,omitempty"`
	Comments    []RefComment `json:"comments,omitempty"`
}

// RefComment is one upstream comment in a snapshot. Every field came from
// outside: DATA, rendered escaped.
type RefComment struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// RefWrite kinds and states (task_ref_writes.kind / .state).
const (
	RefWriteStatus  = "status"
	RefWriteComment = "comment"

	RefWritePending = "pending"
	RefWriteClaimed = "claimed"
	RefWriteDone    = "done"
	RefWriteFailed  = "failed"

	// RefWriteMaxAttempts is how many times a write is handed out (a claim
	// whose lease lapsed without a result) before it is failed with
	// refWriteGaveUp.
	RefWriteMaxAttempts = 10
)

// RefWrite is one write a person or agent made on a reference, queued for the
// plugin (task_ref_writes).
type RefWrite struct {
	ID     int64  `json:"id"`
	NoteID int64  `json:"task_id"`
	Key    string `json:"key"`
	// PluginID is the plugin it is handed to: the reference's, copied at enqueue.
	PluginID int64  `json:"-"`
	Kind     string `json:"kind"`
	// Status is the muster status a status write asks for; "" otherwise.
	Status string `json:"status,omitempty"`
	// Body is a comment write's text; "" otherwise.
	Body string `json:"body,omitempty"`
	// Author is the label the LOCAL path would have stamped (an agent's name,
	// "user", a route's session…); Agent and Session name the agent and the
	// session it came from, when known.
	Author  string `json:"author,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	// Writer is a status write's api writer label (api.statusWriter): which
	// path asked. Not part of the plugin's contract.
	Writer string `json:"-"`
	// DedupeKey, when set, makes a retried producer's write one row per task.
	DedupeKey  string     `json:"-"`
	State      string     `json:"state"`
	Error      string     `json:"error,omitempty"`
	Attempts   int        `json:"attempts"`
	LeaseUntil *time.Time `json:"lease_until,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// DismissedAt is when the owner dismissed this failed write (0074): it is
	// then off the task page, and can be neither retried nor dismissed again.
	DismissedAt *time.Time `json:"dismissed_at,omitempty"`
	// Superseded: a failed STATUS write with a later status write on the same
	// task already done (refWriteSuperseded). It cannot be retried (it would
	// undo the newer status); it can be dismissed. Not part of the plugin's
	// contract.
	Superseded bool `json:"-"`
}

// RefUpsert is what UpsertReference did.
type RefUpsert struct {
	// Note is the reference row afterwards (its status is still the one it had
	// before the snapshot: the status follows through Store.SetStatus, so it is
	// logged with its writer — see api.applyReference).
	Note Note
	// Found is false only for a detach that found nothing to detach.
	Found bool
	// Created: a new row was filed. Converted: an existing COPY row (the key's
	// tag, no ext_key) became the reference in place.
	Created, Converted bool
	// Applied is false for a snapshot OLDER than the stored one (accepted, not
	// applied). Equal is applied again (idempotent).
	Applied bool
	// Unchanged: an applied snapshot whose content (title, body, status, label,
	// url, comments, rev, upstream updated_at) is exactly the stored one. The row
	// was not rewritten and updated_at did not move: only ext_synced_at did (it
	// feeds the stale badge), so there is nothing for a page to re-render.
	Unchanged bool
	// Adopted: the reference was held by another of the owner's plugins that is
	// no longer live (revoked or deleted), and this plugin took it over — with
	// its unfinished writes (see UpsertReference).
	Adopted bool
	// Detached: the row was dismissed (the same soft delete an owner's dismiss is).
	Detached bool
}

// RefUpsertIn is one upsert.
type RefUpsertIn struct {
	Key      string
	PluginID int64
	Snapshot RefSnapshot
	// Detach dismisses the live reference (or the copy row it would convert).
	Detach bool
	// Create files a new row when none exists. False for a snapshot attached to
	// a write's result: that may refresh a reference, never resurrect one.
	Create bool
	// RelayAuthor is the converting plugin's own comment author
	// (`plugin:<name>`): its relay comments on a copy row are retracted when the
	// row is converted — the reference renders the upstream's thread instead.
	RelayAuthor string
}

// RefListQuery is one page of a plugin's reconcile listing
// (Store.ListLiveInNamespace; GET /plugin/v1/tasks).
type RefListQuery struct {
	// Namespace is required: the plugin task namespace listed.
	Namespace string
	// PluginID is the listing plugin: the only plugin whose references are seen.
	PluginID int64
	// Key, when set, is one full key in Namespace: at most the one row an
	// upsert for it would work on.
	Key string
	// OpenOnly leaves out complete tasks.
	OpenOnly bool
	// After lists only ids greater than it (the paging cursor).
	After int64
	// Limit bounds the page.
	Limit int
}

var (
	// ErrNotReference: the task is not a live reference (EnqueueRefWrite's
	// "write locally instead").
	ErrNotReference = errors.New("notes: the task is not a reference")
	// ErrRefOtherPlugin: the live reference for the key belongs to another of
	// the owner's plugins, and that plugin is still live (not revoked).
	ErrRefOtherPlugin = errors.New("notes: the reference belongs to another plugin")
	// ErrRefWriteNotLeased: a result for a write this plugin does not hold.
	ErrRefWriteNotLeased = errors.New("notes: no such write leased to this plugin")
	// ErrRefWriteNotFound: no such write on that task (as the caller sees it:
	// another owner's write is not found either).
	ErrRefWriteNotFound = errors.New("notes: no such write on this task")
	// ErrRefWriteNotFailed: an owner's retry or dismiss of a write that is not
	// failed (pending, claimed, done) or was already dismissed.
	ErrRefWriteNotFailed = errors.New("notes: only a failed write can be retried or dismissed")
	// ErrRefWriteSuperseded: a retry of a status write a later, done status
	// write on the same task supersedes.
	ErrRefWriteSuperseded = errors.New("notes: a later status write on this task is already applied")
)

// refScan holds the reference columns as scanned; adoptRef turns them into
// Note.Ref. Unexported, so the JSON and every caller see only Ref.
type refScan struct {
	key      *string
	pluginID *int64
	snapshot *RefSnapshot
	syncedAt *time.Time
}

// adoptRef sets n.Ref from the scanned reference columns (nil for an ordinary
// task) and clears the scratch. Every scan of noteCols calls it (scanNote).
func (n *Note) adoptRef() {
	r := n.refRaw
	n.refRaw = refScan{}
	if r.key == nil {
		n.Ref = nil
		return
	}
	ref := &Reference{Key: *r.key}
	if r.pluginID != nil {
		ref.PluginID = *r.pluginID
	}
	if r.snapshot != nil {
		ref.Snapshot = *r.snapshot
	}
	if r.syncedAt != nil {
		ref.SyncedAt = *r.syncedAt
	}
	n.Ref = ref
}

// KeyInNamespace reports whether key is one full, normalized plugin task key
// in namespace ns (the listing's key= rule).
func KeyInNamespace(key, ns string) bool {
	kns, _ := ParseTag(key)
	return kns == ns && NormalizeTag(key) == key
}

// IsReference reports whether n is a reference row.
func (n Note) IsReference() bool { return n.Ref != nil }

// refOnlyIfKey carries OnlyIfSnapshotAt's required snapshot time.
type refOnlyIfKey struct{}

// OnlyIfSnapshotAt makes ctx's status writes CONDITIONAL on the reference's
// stored snapshot being the one stamped at: PGStore.SetStatus moves the row
// only while ext_snapshot's updated_at is still that instant — in the UPDATE
// itself — and answers ErrStatusMoved otherwise. It is how a snapshot's status
// follows its snapshot without a newer snapshot, applied in between, being
// overwritten by an older one's status.
func OnlyIfSnapshotAt(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, refOnlyIfKey{}, snapshotStamp(at))
}

func onlyIfSnapshot(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(refOnlyIfKey{}).(string)
	return v, ok
}

// snapshotStamp is how a snapshot's updated_at is compared in SQL: the text
// encoding/json gives a time.Time (RFC 3339 with nanoseconds, UTC), which is
// exactly what the stored jsonb holds because the store marshals the same
// value.
func snapshotStamp(at time.Time) string {
	b, _ := at.UTC().MarshalJSON()
	return string(b[1 : len(b)-1])
}
