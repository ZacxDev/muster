package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// PLUGIN TASK REFERENCES, THE PLUGIN'S HALF (docs/plugins.md#task-references).
//
//   - POST /plugin/v1/tasks with `reference` upserts ONE reference row for the
//     key and applies the upstream's snapshot (handlePluginReference). An
//     existing copy row for the key is CONVERTED IN PLACE: same id, so routes,
//     agents and links survive.
//   - POST /plugin/v1/task-writes/claim long-polls for the writes people and
//     agents queued on this plugin's references (task_ref_queue.go) and leases
//     them; POST /plugin/v1/task-writes/{id}/result settles one, optionally with
//     a fresh snapshot, applied exactly like an upsert.
//   - GET /plugin/v1/tasks?namespace=<ns>&live=true lists the owner's live rows
//     in a namespace, so a plugin can convert copy rows and reconcile.
//
// muster never calls the plugin: the plugin's claim is the clock.
//
// 🔴 A SNAPSHOT'S STATUS IS WRITTEN AS writerPluginReference, THROUGH
// setNoteStatus. That keeps it on the logged status funnel (WithStatusLogging
// names the writer), keeps the normal pushes, and is the one place its
// `complete` is kept from stopping the chief's agent (task_status_writer.go):
// an upstream close is not the owner stopping an agent. The write is
// conditional on its own snapshot still being the stored one
// (notes.OnlyIfSnapshotAt), so two snapshots racing cannot leave the older
// one's status on the newer one's row.
// ---------------------------------------------------------------------------

const (
	pluginTaskWritesPath = pluginAPIPrefix + "/task-writes"

	// maxRefComments is how many upstream comments a snapshot keeps: the
	// newest, by created_at. More are dropped, never refused.
	maxRefComments = 50
	// maxRefLabelRunes bounds status_label (plain text, shown verbatim).
	maxRefLabelRunes = 100
	// maxRefRevRunes bounds rev; maxRefIDRunes a comment id; maxRefAuthorRunes
	// a comment author.
	maxRefRevRunes    = 100
	maxRefIDRunes     = 200
	maxRefAuthorRunes = 200
	// maxRefURLBytes bounds the upstream link; a longer one, or one that is not
	// http(s), is dropped (the snapshot is kept without it).
	maxRefURLBytes = 2048
	// maxEventIDBytes bounds event_id.
	maxEventIDBytes = 200
	// pluginRefRequestBytes is how much of a reference upsert the route reads:
	// room for maxRefComments comments of plugins.MaxBodyBytes and the body,
	// JSON-escaped at worst, with margin — a plugin sending MORE comments
	// still has them truncated rather than refused, up to this bound.
	pluginRefRequestBytes = 8 << 20

	// refWriteClaimMax bounds one claim's batch.
	refWriteClaimMax = 20
	// refListMax bounds the reconcile listing.
	refListMax = 500
)

// refWriteLease is how long a claiming plugin holds a write before it returns
// to pending: the reply drafts' lease (plugins.LeaseDuration). A var so a test
// can shorten it.
var refWriteLease = plugins.LeaseDuration

// pluginRefIn is a reference snapshot as a plugin sends it.
type pluginRefIn struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	Rev         string `json:"rev"`
	UpdatedAt   string `json:"updated_at"`
	Body        string `json:"body"`
	// Creator is OPTIONAL: the upstream's display name for whoever created the
	// record. One line of plain text, cut at maxRefAuthorRunes like a comment
	// author; the task notifications name it (taskRefFragment).
	Creator  string `json:"creator"`
	Comments []struct {
		ID        string `json:"id"`
		Author    string `json:"author"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	} `json:"comments"`
	Detach bool `json:"detach"`
}

// cleanText makes s storable text: valid UTF-8, no NUL. The reference path
// truncates and cleans, never refuses (the route's rule).
func cleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}

// plainLabel is s as one line of plain text, capped at n runes.
func plainLabel(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, cleanText(s))
	return capRunes(strings.Join(strings.Fields(s), " "), n)
}

// refURL keeps an http(s) link of bounded length, and drops anything else.
func refURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxRefURLBytes {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// validatePluginRef is validatePluginTask's half for a request with
// `reference`; the key has been checked already.
func validatePluginRef(in pluginTaskIn) string {
	ref := in.Reference
	switch {
	case in.Create != nil:
		return "reference replaces create: send one or the other"
	case in.Comment != "":
		return "comment is not allowed with reference: a reference has no relay comment (name the event with event_id)"
	case in.RouteToChief && !validNotice(in.Notice):
		return "route_to_chief needs notice: assigned, mentioned or update"
	case !in.RouteToChief && in.Notice != "":
		return "notice is only for route_to_chief"
	case in.RouteToChief && in.EventID == "":
		return "route_to_chief with reference needs event_id: it names the event"
	}
	if in.EventID != "" {
		if msg := validEventID(in.EventID); msg != "" {
			return msg
		}
	}
	if ref.Detach {
		return ""
	}
	return validateRefSnapshot(*ref)
}

// validateRefSnapshot is the part of a snapshot that is refused rather than
// truncated: a status that is not muster's, and a missing or unreadable
// updated_at (the stale rule needs it).
func validateRefSnapshot(ref pluginRefIn) string {
	if !notes.ValidStatus(ref.Status) {
		return "reference.status must be one of open, in_progress, ready_for_review, complete"
	}
	if _, err := time.Parse(time.RFC3339, ref.UpdatedAt); err != nil {
		return "reference.updated_at is required (RFC 3339)"
	}
	return ""
}

// validEventID returns a 400 message for a bad event_id, or "".
func validEventID(id string) string {
	if len(id) < 1 || len(id) > maxEventIDBytes || !utf8.ValidString(id) {
		return "event_id must be 1-200 bytes of UTF-8"
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "event_id must not contain spaces or control characters"
		}
	}
	return ""
}

// snapshot is ref, validated, as the store keeps it: capped and cleaned, the
// newest maxRefComments comments kept, oldest first.
func (ref pluginRefIn) snapshot(key string) notes.RefSnapshot {
	at, _ := time.Parse(time.RFC3339, ref.UpdatedAt)
	snap := notes.RefSnapshot{
		Title:       capRunes(strings.TrimSpace(cleanText(ref.Title)), maxTaskFieldLen),
		URL:         refURL(ref.URL),
		Status:      ref.Status,
		StatusLabel: plainLabel(ref.StatusLabel, maxRefLabelRunes),
		Rev:         plainLabel(ref.Rev, maxRefRevRunes),
		UpdatedAt:   at.UTC(),
		Body:        truncBytes(cleanText(ref.Body), plugins.MaxBodyBytes),
		Creator:     plainLabel(ref.Creator, maxRefAuthorRunes),
	}
	if snap.Title == "" {
		snap.Title = key
	}
	for _, c := range ref.Comments {
		created, _ := time.Parse(time.RFC3339, c.CreatedAt)
		snap.Comments = append(snap.Comments, notes.RefComment{
			ID:        plainLabel(c.ID, maxRefIDRunes),
			Author:    plainLabel(c.Author, maxRefAuthorRunes),
			Body:      truncBytes(cleanText(c.Body), plugins.MaxBodyBytes),
			CreatedAt: created.UTC(),
		})
	}
	sort.SliceStable(snap.Comments, func(i, j int) bool { return snap.Comments[i].CreatedAt.Before(snap.Comments[j].CreatedAt) })
	if len(snap.Comments) > maxRefComments {
		snap.Comments = snap.Comments[len(snap.Comments)-maxRefComments:]
	}
	return snap
}

// refEventDiscriminator names a reference notice for routing: its event_id.
// It starts with a NUL, which no copy-task comment can (validatePluginTask
// refuses one), so it can never name the same event as a comment.
func refEventDiscriminator(eventID string) string { return "\x00event:" + eventID }

// refNoticeChange is muster's own summary of a snapshot for a routed notice —
// a reference has no relay comment to quote. Every part of it came from the
// upstream, so the task event fences it as DATA (plugins.TaskEvent.Change).
func refNoticeChange(snap notes.RefSnapshot) string {
	label := snap.StatusLabel
	if label == "" {
		label = snap.Status
	}
	out := "upstream status: " + label
	if n := len(snap.Comments); n > 0 {
		c := snap.Comments[n-1]
		out += "\nlatest upstream comment (" + c.Author + "): " + truncBytes(c.Body, 2000)
	}
	return out
}

// handlePluginReference is POST /plugin/v1/tasks with `reference` (validated):
//
//   - the live reference for key, or the copy row it converts, or (none) a new
//     row: the snapshot is applied unless it is older than the stored one;
//     200 {id, created:false, commented:false, reference:true, applied,
//     converted} — 201 with created:true for a new row;
//   - detach: the row is dismissed (detached:true), or nothing happens when
//     there is none (id null);
//   - route_to_chief: routed as a copy task's notice is, named by event_id.
func (s *Server) handlePluginReference(w http.ResponseWriter, r *http.Request, p plugins.Plugin, in pluginTaskIn) {
	ctx := r.Context()
	var snap notes.RefSnapshot
	if !in.Reference.Detach {
		snap = in.Reference.snapshot(in.Key)
	}
	up, err := s.applyReference(ctx, p, in.Key, snap, in.Reference.Detach, true)
	switch {
	case err == nil:
	case errors.Is(err, notes.ErrRefOtherPlugin):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "another of this owner's plugins holds the reference for this key"})
		return
	default:
		s.logger.Printf("plugins: plugin %d reference %s: %v", p.ID, in.Key, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the reference could not be filed"})
		return
	}
	out := map[string]any{"id": nil, "created": up.Created, "commented": false, "reference": true,
		"applied": up.Applied, "converted": up.Converted, "detached": up.Detached}
	if !up.Found {
		s.routeAndAnswerPluginTask(w, r, p, in, taskNoticeEvent{}, http.StatusOK, nil, out)
		return
	}
	id := up.Note.ID
	out["id"] = id
	code := http.StatusOK
	if up.Created {
		code = http.StatusCreated
	}
	routeID := &id
	if up.Detached {
		routeID = nil // nothing left to route
	}
	s.routeAndAnswerPluginTask(w, r, p, in, taskNoticeEvent{
		Discriminator: refEventDiscriminator(in.EventID), Change: refNoticeChange(snap)}, code, routeID, out)
}

// applyReference upserts plugin p's reference for key with snap (or detaches
// it) and moves the row's status to the snapshot's through setNoteStatus as
// writerPluginReference. create is false for a snapshot attached to a write's
// result, which refreshes a reference but never files one.
func (s *Server) applyReference(ctx context.Context, p plugins.Plugin, key string, snap notes.RefSnapshot, detach, create bool) (notes.RefUpsert, error) {
	up, err := s.ext.Notes.UpsertReference(ctx, notes.RefUpsertIn{Key: key, PluginID: p.ID, Snapshot: snap,
		Detach: detach, Create: create, RelayAuthor: pluginCommentAuthor(p)})
	if err != nil || !up.Found {
		return up, err
	}
	id := strconv.FormatInt(up.Note.ID, 10)
	if up.Detached {
		// The owner's soft dismissal (notes.SoftDelete's effect), and only that:
		// an agent on the task is left running — an upstream unassign is not the
		// owner tearing an agent down (dismissTask's teardown is the owner's).
		s.logger.Printf("plugins: plugin %d detached reference %s (task %s)", p.ID, key, id)
		s.broadcast(ctx, EventTaskChanged, id)
		return up, nil
	}
	if up.Converted {
		s.logger.Printf("plugins: plugin %d converted task %s into the reference for %s", p.ID, id, key)
	}
	if up.Adopted {
		s.logger.Printf("plugins: plugin %d adopted the reference for %s (task %s): its holder is no longer live", p.ID, key, id)
	}
	if up.Created {
		s.taskCreatedEffects(ctx, up.Note, "api", nil)
	}
	statusMoved := false
	if up.Applied && up.Note.Status != snap.Status {
		n, err := s.setNoteStatus(notes.OnlyIfSnapshotAt(ctx, snap.UpdatedAt), writerPluginReference, up.Note.ID, snap.Status)
		switch {
		case err == nil:
			up.Note = n
			statusMoved = true // setNoteStatus sent the ready-for-review push, labelled upstream
		case errors.Is(err, notes.ErrStatusMoved):
			// A newer snapshot was applied meanwhile; it carries its own status.
		default:
			return up, err
		}
	}
	// An unchanged snapshot (a reconcile's resend) changed nothing a page shows
	// — only ext_synced_at moved — so it broadcasts nothing; an adoption moved
	// the task's writes back to pending, which the page does show.
	if !up.Created && (statusMoved || up.Adopted || (up.Applied && !up.Unchanged)) {
		s.broadcast(ctx, EventTaskChanged, id)
	}
	return up, nil
}

// handlePluginTaskWritesClaim is POST /plugin/v1/task-writes/claim {wait_s,
// limit}: 200 {"writes": [...]} — up to limit (1-20) of this plugin's pending
// writes, each now leased for refWriteLease — waiting up to wait_s (0-25)
// for one to arrive; an empty list when none did. Out-of-range values are
// clamped, not refused (the drafts claim's rule). An expired lease is back in
// the next claim.
func (s *Server) handlePluginTaskWritesClaim(w http.ResponseWriter, r *http.Request) {
	if s.ext.Notes == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no task store"})
		return
	}
	var in struct {
		WaitS int `json:"wait_s"`
		Limit int `json:"limit"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err == nil && len(bytes.TrimSpace(body)) > 0 {
		err = json.Unmarshal(body, &in)
	}
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"wait_s": 0-25, "limit": 1-20}`})
		return
	}
	wait := time.Duration(min(max(in.WaitS, 0), int(plugins.MaxClaimWait/time.Second))) * time.Second
	limit := min(max(in.Limit, 1), refWriteClaimMax)
	p := pluginFromContext(r.Context())
	ctx, cancel := s.streamContext(r.Context())
	defer cancel()
	deadline := time.Now().Add(wait)
	for {
		woken := s.refWriteWake.wait()
		got, err := s.ext.Notes.ClaimRefWrites(ctx, p.ID, limit, refWriteLease)
		if err != nil {
			if ctx.Err() != nil {
				s.writeJSON(w, http.StatusOK, map[string]any{"writes": []notes.RefWrite{}})
				return
			}
			s.logger.Printf("plugins: plugin %d task-write claim: %v", p.ID, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the claim could not be made"})
			return
		}
		if len(got) > 0 {
			for _, x := range got {
				s.broadcastTo(p.OwnerID, EventTaskChanged, strconv.FormatInt(x.NoteID, 10))
			}
			s.writeJSON(w, http.StatusOK, map[string]any{"writes": got})
			return
		}
		left := time.Until(deadline)
		if left <= 0 {
			s.writeJSON(w, http.StatusOK, map[string]any{"writes": []notes.RefWrite{}})
			return
		}
		t := time.NewTimer(min(left, claimPollInterval))
		select {
		case <-ctx.Done():
			t.Stop()
			s.writeJSON(w, http.StatusOK, map[string]any{"writes": []notes.RefWrite{}})
			return
		case <-woken:
		case <-t.C:
		}
		t.Stop()
	}
}

// handlePluginTaskWriteResult is POST /plugin/v1/task-writes/{id}/result {ok,
// error, conflict, reference}: settle a write this plugin claimed as done (ok)
// or failed (error; conflict marks the upstream having moved under it), and
// apply the snapshot when one is included, exactly as an upsert would — except
// that it never files a new row. 200 {id, state, applied}; the same write again
// is 200 with the first result; a write this plugin does not hold is 404; a bad
// snapshot is 400 and records nothing.
func (s *Server) handlePluginTaskWriteResult(w http.ResponseWriter, r *http.Request) {
	if s.ext.Notes == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no task store"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such write"})
		return
	}
	var in struct {
		OK        *bool        `json:"ok"`
		Error     string       `json:"error"`
		Conflict  bool         `json:"conflict"`
		Reference *pluginRefIn `json:"reference"`
	}
	// Unknown fields are ignored, as the drafts result's are.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, pluginRefRequestBytes)).Decode(&in); err != nil || in.OK == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"ok": true|false, "error", "conflict", "reference": {…}}`})
		return
	}
	if in.Reference != nil && !in.Reference.Detach {
		if msg := validateRefSnapshot(*in.Reference); msg != "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
			return
		}
	}
	ok := *in.OK && !in.Conflict
	why := cleanText(strings.TrimSpace(in.Error))
	if in.Conflict {
		why = "conflict: " + firstNonBlank(why, "the upstream changed under the write")
	} else if !ok && why == "" {
		why = "the plugin reported a failure"
	}
	p := pluginFromContext(r.Context())
	ctx := r.Context()
	wr, settled, err := s.ext.Notes.FinishRefWrite(ctx, p.ID, id, ok, why)
	switch {
	case err == nil:
	case errors.Is(err, notes.ErrRefWriteNotLeased):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such write claimed by this plugin"})
		return
	default:
		s.logger.Printf("plugins: plugin %d result for task write %d: %v", p.ID, id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the result could not be recorded"})
		return
	}
	if settled {
		s.logger.Printf("plugins: plugin %d task write %d (%s on task %d) %s %s", p.ID, wr.ID, wr.Kind, wr.NoteID, wr.State, wr.Error)
	}
	applied := false
	if in.Reference != nil && wr.Key != "" {
		var snap notes.RefSnapshot
		if !in.Reference.Detach {
			snap = in.Reference.snapshot(wr.Key)
		}
		up, aerr := s.applyReference(ctx, p, wr.Key, snap, in.Reference.Detach, false)
		if aerr != nil && !errors.Is(aerr, notes.ErrRefOtherPlugin) {
			s.logger.Printf("plugins: plugin %d task write %d: apply the snapshot: %v", p.ID, id, aerr)
		}
		applied = aerr == nil && up.Applied
	}
	// 🔴 THE OWNER'S `complete`, LANDED UPSTREAM, IS THE OWNER STOPPING THE
	// CHIEF'S AGENT — the same as a local complete (setNoteStatus). Only a write
	// this call settled, only `complete`, only done: a snapshot's own close
	// stops nothing, and an agent cannot queue `complete` (the agent rule runs
	// before the queue).
	if settled && wr.State == notes.RefWriteDone && wr.Kind == notes.RefWriteStatus && wr.Status == notes.StatusComplete {
		if n, gerr := s.ext.Notes.Get(ctx, wr.NoteID); gerr == nil {
			s.stopChiefAgentOnComplete(ctx, n)
		}
	}
	s.broadcastTo(p.OwnerID, EventTaskChanged, strconv.FormatInt(wr.NoteID, 10))
	s.writeJSON(w, http.StatusOK, map[string]any{"id": wr.ID, "state": wr.State, "applied": applied})
}

// pluginTaskRow is one row of the reconcile listing.
type pluginTaskRow struct {
	ID          int64              `json:"id"`
	Key         string             `json:"key"`
	Status      string             `json:"status"`
	IsReference bool               `json:"is_reference"`
	UpdatedAt   time.Time          `json:"updated_at"`
	Comments    []pluginTaskRowCmt `json:"comments"`
}

type pluginTaskRowCmt struct {
	ID        int64     `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	Retracted bool      `json:"retracted,omitempty"`
}

// handlePluginTaskList is GET /plugin/v1/tasks?namespace=<ns>&live=true
// [&key=<key>][&open=true][&after=<id>]: the live tasks (not dismissed, not
// merged away) in ns that THIS plugin may see — its own references, and copy
// rows carrying a key in ns; never another plugin's reference — with their
// comments (a retracted one as a tombstone), ascending by id, refListMax a
// page. key= is one full key (at most one row: the one an upsert for it works
// on); open=true leaves out complete tasks; after= lists ids greater than it.
// 200 {"tasks", "truncated", "next_after"}: next_after is the page's last id
// when truncated (pass it as after=), else null. live=true is the only
// listing there is, and is required so a later one can be added without
// changing what this answers.
func (s *Server) handlePluginTaskList(w http.ResponseWriter, r *http.Request) {
	if s.ext.Notes == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no task store"})
		return
	}
	q := r.URL.Query()
	ns := q.Get("namespace")
	if !notes.IsPluginTaskNamespace(ns) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace must be one of this server's plugin task namespaces (" +
			strings.Join(notes.PluginTaskNamespaces(), ", ") + ")"})
		return
	}
	if q.Get("live") != "true" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "live=true is required (only live rows are listed)"})
		return
	}
	lq := notes.RefListQuery{Namespace: ns, PluginID: pluginFromContext(r.Context()).ID, Limit: refListMax + 1}
	if q.Has("key") {
		lq.Key = q.Get("key")
		if !notes.KeyInNamespace(lq.Key, ns) {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key must be one full, normalized key in namespace " + ns})
			return
		}
	}
	switch q.Get("open") {
	case "", "false":
	case "true":
		lq.OpenOnly = true
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "open must be true or false"})
		return
	}
	if q.Has("after") {
		after, err := strconv.ParseInt(q.Get("after"), 10, 64)
		if err != nil || after < 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "after must be a task id (a non-negative integer)"})
			return
		}
		lq.After = after
	}
	list, err := s.ext.Notes.ListLiveInNamespace(r.Context(), lq)
	if err != nil {
		s.logger.Printf("plugins: list namespace %s: %v", ns, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the tasks could not be listed"})
		return
	}
	truncated := len(list) > refListMax
	var nextAfter *int64
	if truncated {
		list = list[:refListMax]
		last := list[len(list)-1].ID
		nextAfter = &last
	}
	out := make([]pluginTaskRow, 0, len(list))
	for _, n := range list {
		row := pluginTaskRow{ID: n.ID, Status: n.Status, IsReference: n.IsReference(), UpdatedAt: n.UpdatedAt,
			Comments: []pluginTaskRowCmt{}}
		if n.Ref != nil {
			row.Key = n.Ref.Key
		} else {
			for _, t := range n.Tags {
				if tns, _ := notes.ParseTag(t); tns == ns {
					row.Key = t
					break
				}
			}
		}
		for _, c := range n.Comments {
			row.Comments = append(row.Comments, pluginTaskRowCmt{ID: c.ID, Author: c.Author, Body: c.Body,
				CreatedAt: c.CreatedAt, Retracted: c.Retracted})
		}
		out = append(out, row)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"tasks": out, "truncated": truncated, "next_after": nextAfter})
}
