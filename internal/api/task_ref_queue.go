package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// WRITES ON A PLUGIN TASK REFERENCE ARE QUEUED, NOT STORED
// (docs/plugins.md#task-references; internal/notes/references.go).
//
// A reference's upstream is canonical. A status a person or agent sets on it,
// or a comment one writes, does NOT change the row: it is queued as a
// task_ref_writes row for the plugin that owns the reference, which claims it
// (POST /plugin/v1/task-writes/claim), applies it upstream, and reports back
// with a fresh snapshot. Muster never calls the plugin (docs/decisions.md).
//
// 🔴 ONE RULE, ONE PLACE. "Is this a reference, so queue instead of write?" is
// queueIfReference and nothing else, and it has exactly two callers:
// setNoteStatus (every status writer in this package goes through it —
// task_status_ledger_test.go) and addTaskComment / addTaskCommentOnce (every
// comment writer does — task_ref_queue_ledger_test.go, which also pins WHICH
// origin each writer declares). A writer that called the store directly
// would write a reference's status or comment locally, silently diverging
// from the upstream — exactly the drift references exist to end.
//
// 🔴 MUSTER'S OWN BOOKKEEPING STAYS LOCAL. A comment muster itself writes (the
// reaper's `stale` note, a merge record, the chief's gate release) is not a
// person's or agent's statement about the work: it is rendered as muster-only
// history and never sent upstream (commentByMuster). The plugin's own relay
// (commentByPlugin) is the upstream talking, so it is local too.
// ---------------------------------------------------------------------------

// errQueuedUpstream: the write was queued for the reference's plugin instead
// of stored. setNoteStatus returns it with the task as it stands (unchanged);
// every caller answers it as success with `queued: true` (202 on HTTP).
var errQueuedUpstream = errors.New("queued for the upstream; the task changes when the plugin confirms")

// refQueuedText is what a tool answers for a queued write.
const refQueuedText = "sent to the upstream; it will show on the task once the plugin confirms"

// refOriginKey carries who is writing, for the queued row.
type refOriginKey struct{}

// refOrigin names who made a write, as the queued row records it: the author
// label the local path would have stamped, and the agent and session it came
// from when known.
type refOrigin struct {
	Author, Agent, Session string
}

// withRefOrigin stamps ctx with who is writing. A writer that knows its agent
// or session sets it; otherwise a status write is attributed to its writer
// label.
func withRefOrigin(ctx context.Context, o refOrigin) context.Context {
	return context.WithValue(ctx, refOriginKey{}, o)
}

// machineRefOrigin is a machine route's writer (POST /api/tasks/{id}/comments,
// PATCH /api/tasks/{id}/status): the comment author the local path stamps and
// the producer's session id header.
func (s *Server) machineRefOrigin(r *http.Request) refOrigin {
	_, session := taskSource(r, s.ext.LegacyProvenanceHeaderPrefix)
	return refOrigin{Author: taskCommentAuthor(r, s.ext.LegacyProvenanceHeaderPrefix), Session: session}
}

// queuedTask is a machine route's answer for a write queued on a reference:
// the task as it stands, plus `queued: true`.
type queuedTask struct {
	apiTask
	Queued bool `json:"queued"`
}

func refOriginFrom(ctx context.Context) refOrigin {
	o, _ := ctx.Value(refOriginKey{}).(refOrigin)
	return o
}

// queueIfReference is THE decision: when task w.NoteID is a live reference, w
// is queued for its plugin and queued is true; otherwise nothing is written
// and the caller writes locally. The store's INSERT is itself conditional on
// the row being a reference (notes.PGStore.EnqueueRefWrite), so there is no
// read-then-write gap for a task to stop or start being one in.
func (s *Server) queueIfReference(ctx context.Context, w notes.RefWrite) (notes.RefWrite, bool, error) {
	o := refOriginFrom(ctx)
	if w.Author == "" {
		w.Author = o.Author
	}
	if w.Agent == "" {
		w.Agent = o.Agent
	}
	if w.Session == "" {
		w.Session = o.Session
	}
	got, created, err := s.ext.Notes.EnqueueRefWrite(ctx, w)
	switch {
	case errors.Is(err, notes.ErrNotReference):
		return notes.RefWrite{}, false, nil
	case err != nil:
		return notes.RefWrite{}, false, err
	}
	if created {
		s.logger.Printf("tasks: task %d is a reference: %s write %d queued for plugin %d (author %q)",
			w.NoteID, w.Kind, got.ID, got.PluginID, got.Author)
		s.refWriteWake.signal()
		s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(w.NoteID, 10))
	}
	return got, true, nil
}

// commentOrigin is who a comment is from, which decides whether it can be
// queued. A named type so a writer must pick one (the ledger pins which).
type commentOrigin string

const (
	// commentByActor: a person or an agent saying something about the work.
	// On a reference it is queued for the upstream.
	commentByActor commentOrigin = "actor"
	// commentByMuster: muster's own bookkeeping. Always local; on a reference
	// it renders as muster-only history.
	commentByMuster commentOrigin = "muster"
	// commentByPlugin: a plugin relaying its upstream (POST /plugin/v1/tasks'
	// comment). The upstream talking about itself: always local.
	commentByPlugin commentOrigin = "plugin"
)

// addTaskComment is the ONE place in this package that calls
// Notes.AddComment. An actor's comment on a reference is queued instead
// (queued true, the returned comment zero, and the queued write returned);
// anything else is stored as before.
func (s *Server) addTaskComment(ctx context.Context, origin commentOrigin, c notes.Comment) (notes.Comment, *notes.RefWrite, error) {
	if origin == commentByActor {
		w, queued, err := s.queueIfReference(ctx, notes.RefWrite{
			NoteID: c.NoteID, Kind: notes.RefWriteComment, Body: c.Body, Author: c.Author})
		if err != nil {
			return notes.Comment{}, nil, err
		}
		if queued {
			return notes.Comment{}, &w, nil
		}
	}
	got, err := s.ext.Notes.AddComment(ctx, c)
	return got, nil, err
}

// addTaskCommentOnce is addTaskComment for a producer that retries (a route's
// relay): the store's AddCommentOnce locally, and on a reference a queued
// write deduplicated by dedupe, so a retried relay queues once.
func (s *Server) addTaskCommentOnce(ctx context.Context, origin commentOrigin, c notes.Comment, dedupe string) (wrote, queued bool, err error) {
	if origin == commentByActor {
		_, q, err := s.queueIfReference(ctx, notes.RefWrite{
			NoteID: c.NoteID, Kind: notes.RefWriteComment, Body: c.Body, Author: c.Author, DedupeKey: dedupe})
		if err != nil || q {
			return false, q, err
		}
	}
	wrote, err = s.ext.Notes.AddCommentOnce(ctx, c)
	return wrote, false, err
}

// queueStatusIfReference is setNoteStatus's half of the rule: a status write
// by any writer but the snapshot itself is queued on a reference. The
// conditions the store's own UPDATE would apply are honoured first, so a
// queued write is never one the local write would have refused: an agent's
// write on a complete task (notes.ForAgent), and a write conditional on the
// current status (notes.OnlyFrom), fall through to the local path — which
// answers them exactly as before. An invalid status falls through too.
func (s *Server) queueStatusIfReference(ctx context.Context, writer statusWriter, id int64, status string) (bool, error) {
	if writer == writerPluginReference || !notes.ValidStatus(status) {
		return false, nil
	}
	from, conditional := notes.OnlyFromStatus(ctx)
	if notes.IsForAgent(ctx) || conditional {
		cur, err := s.ext.Notes.Get(ctx, id)
		if err != nil || !cur.IsReference() {
			return false, nil
		}
		if (notes.IsForAgent(ctx) && cur.Status == notes.StatusComplete) || (conditional && cur.Status != from) {
			return false, nil
		}
	}
	o := refOriginFrom(ctx)
	if o.Author == "" {
		o.Author = string(writer)
	}
	_, queued, err := s.queueIfReference(withRefOrigin(ctx, o), notes.RefWrite{
		NoteID: id, Kind: notes.RefWriteStatus, Status: status, Writer: string(writer)})
	return queued, err
}
