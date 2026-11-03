package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
)

// PLUGIN TASK REFERENCES end to end (plugin_task_refs.go, task_ref_queue.go):
// the real routes, the real Postgres stores, a plugin token minted on the
// owner's account.
//
// ⚠ THIS FILE USES ONLY SYMBOLS THAT EXIST WITHOUT THE FEATURE (the routes,
// the handlers, the fixtures, and SQL for what it reads back), so it compiles
// on the pre-change tree and its tests go red there for a BEHAVIOURAL reason —
// `reference` is an unknown field (400), there is no task_ref_writes table, a
// status write lands locally — rather than for a build error.

// refFixture is the complete-stop fixture (owner, chief, recording stop
// provisioner, task routing) plus a plugin of the owner's and its token.
type refFixture struct {
	*completeStopFixture
	tok, tok2 string
	plugin    int64
	plugin2   int64
}

func newRefFixture(t *testing.T) *refFixture {
	t.Helper()
	f := &refFixture{completeStopFixture: newCompleteStopFixture(t)}
	tok, p, err := f.plugins.Create(f.asOwner, "example-tracker", "Example tracker", "example-tracker")
	if err != nil {
		t.Fatal(err)
	}
	tok2, p2, err := f.plugins.Create(f.asOwner, "other-tracker", "Other tracker", "other-tracker")
	if err != nil {
		t.Fatal(err)
	}
	f.tok, f.plugin, f.tok2, f.plugin2 = tok, p.ID, tok2, p2.ID
	return f
}

// key is a fresh plugin task key in the fixture's namespace.
func (f *refFixture) key(t *testing.T) string {
	t.Helper()
	return "tracker:" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// post sends body to a plugin route with tok and decodes the JSON answer.
func (f *refFixture) post(t *testing.T, tok, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := f.connFixture.call(t, http.MethodPost, path, strings.NewReader(string(raw)),
		map[string]string{"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out == nil {
		out = map[string]any{}
	}
	out["_raw"] = rec.Body.String()
	return rec.Code, out
}

// refSnap is a reference snapshot.
func refSnap(status, label string, at time.Time, extra map[string]any) map[string]any {
	r := map[string]any{"title": "Upstream task", "url": "https://tracker.example.invalid/t/1", "status": status,
		"status_label": label, "rev": "1", "updated_at": at.UTC().Format(time.RFC3339), "body": "The upstream's description."}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

// upsert posts a reference for key and returns the code, answer and task id.
func (f *refFixture) upsert(t *testing.T, tok, key string, r map[string]any, extra map[string]any) (int, map[string]any, int64) {
	t.Helper()
	body := map[string]any{"key": key, "reference": r}
	for k, v := range extra {
		body[k] = v
	}
	code, out := f.post(t, tok, "/plugin/v1/tasks", body)
	var id int64
	if v, ok := out["id"].(float64); ok {
		id = int64(v)
	}
	return code, out, id
}

// mustUpsert is upsert that must answer 200 or 201 and name a task.
func (f *refFixture) mustUpsert(t *testing.T, key string, r map[string]any) int64 {
	t.Helper()
	code, out, id := f.upsert(t, f.tok, key, r, nil)
	if (code != http.StatusOK && code != http.StatusCreated) || id == 0 {
		t.Fatalf("reference upsert for %s: HTTP %d %v", key, code, out["_raw"])
	}
	return id
}

// row reads task id's own columns back with SQL.
func (f *refFixture) row(t *testing.T, id int64) (status, title, extKey string, comments int) {
	t.Helper()
	var k *string
	if err := f.pool.QueryRow(f.asOwner, `SELECT status, title, ext_key,
		(SELECT count(*) FROM note_comments c WHERE c.note_id = n.id AND c.deleted_at IS NULL)
		FROM notes n WHERE id = $1`, id).Scan(&status, &title, &k, &comments); err != nil {
		t.Fatal(err)
	}
	if k != nil {
		extKey = *k
	}
	return
}

// queued is task id's queued writes, as "kind:status|body:author", oldest first.
func (f *refFixture) queued(t *testing.T, id int64) []string {
	t.Helper()
	rows, err := f.pool.Query(f.asOwner, `SELECT kind, status, body, author FROM task_ref_writes WHERE note_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, status, body, author string
		if err := rows.Scan(&kind, &status, &body, &author); err != nil {
			t.Fatal(err)
		}
		out = append(out, kind+":"+status+body+":"+author)
	}
	return out
}

func pv(id int64) map[string]string { return map[string]string{"id": strconv.FormatInt(id, 10)} }

// TestAReferenceIsFiledAndAStaleSnapshotNeverOverwritesIt: an upsert files ONE
// reference row (201) whose title, body and status follow the snapshot; an
// OLDER snapshot is accepted (200) and applies nothing; the same snapshot again
// applies again (idempotent, the same row); a newer one moves it.
func TestAReferenceIsFiledAndAStaleSnapshotNeverOverwritesIt(t *testing.T) {
	f := newRefFixture(t)
	key := f.key(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	code, out, id := f.upsert(t, f.tok, key, refSnap("in_progress", "in progress", t0, nil), nil)
	if code != http.StatusCreated || id == 0 || out["reference"] != true || out["created"] != true || out["applied"] != true {
		t.Fatalf("first upsert: HTTP %d %v; want 201 created, reference and applied", code, out["_raw"])
	}
	if st, title, k, _ := f.row(t, id); st != notes.StatusInProgress || title != "Upstream task" || k != key {
		t.Fatalf("filed reference: status %q title %q ext_key %q; want in_progress, the snapshot's title, %q", st, title, k, key)
	}

	// Older: accepted, applies nothing.
	code, out, again := f.upsert(t, f.tok, key, refSnap("complete", "done", t0.Add(-time.Minute), map[string]any{"title": "STALE"}), nil)
	if code != http.StatusOK || again != id {
		t.Fatalf("stale upsert: HTTP %d %v; want 200 and the same id", code, out["_raw"])
	}
	// Not fatal: a stale snapshot that applies should ALSO be seen overwriting.
	if out["applied"] != false {
		t.Errorf("stale upsert answered applied:%v, want false", out["applied"])
	}
	if st, title, _, _ := f.row(t, id); st != notes.StatusInProgress || title != "Upstream task" {
		t.Fatalf("a STALE snapshot overwrote the reference: status %q title %q", st, title)
	}

	// Equal: applied again, still one row.
	code, out, again = f.upsert(t, f.tok, key, refSnap("in_progress", "in progress", t0, nil), nil)
	if code != http.StatusOK || again != id || out["applied"] != true {
		t.Fatalf("equal upsert: HTTP %d %v; want 200, the same id, applied:true", code, out["_raw"])
	}
	// Newer: moves the status.
	if _, _, again = f.upsert(t, f.tok, key, refSnap("ready_for_review", "awaiting review", t0.Add(time.Minute), nil), nil); again != id {
		t.Fatalf("newer upsert named task %d, want %d", again, id)
	}
	if st, _, _, _ := f.row(t, id); st != notes.StatusReadyForReview {
		t.Fatalf("after a newer snapshot the status is %q, want ready_for_review", st)
	}
	var n int
	if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM notes WHERE ext_key = $1 AND deleted_at IS NULL`, key).Scan(&n); err != nil || n != 1 {
		t.Fatalf("live references for %s: %d (%v), want exactly 1", key, n, err)
	}
}

// TestACopyRowIsConvertedInPlace: a task the plugin filed in copy mode (create
// + relay comment) becomes the reference with the SAME id; the plugin's relay
// comment is retracted (a tombstone), the owner's comment stays.
func TestACopyRowIsConvertedInPlace(t *testing.T) {
	f := newRefFixture(t)
	key := f.key(t)
	code, out := f.post(t, f.tok, "/plugin/v1/tasks", map[string]any{"key": key,
		"create": map[string]any{"title": "Copied", "body": "a copy"}, "comment": "relayed upstream comment (event 1)"})
	if code != http.StatusCreated {
		t.Fatalf("copy create: HTTP %d %v", code, out["_raw"])
	}
	copyID := int64(out["id"].(float64))
	if _, err := f.notes.AddComment(f.asOwner, notes.Comment{NoteID: copyID, Author: "user", Body: "my own note"}); err != nil {
		t.Fatal(err)
	}

	code, out, id := f.upsert(t, f.tok, key, refSnap("open", "to do", time.Now(), nil), nil)
	if code != http.StatusOK || id != copyID || out["converted"] != true {
		t.Fatalf("converting upsert: HTTP %d %v; want 200 naming the copy row %d, converted:true", code, out["_raw"], copyID)
	}
	var relayLive, relayGone, own int
	if err := f.pool.QueryRow(f.asOwner, `SELECT
		count(*) FILTER (WHERE author LIKE 'plugin:%' AND deleted_at IS NULL),
		count(*) FILTER (WHERE author LIKE 'plugin:%' AND deleted_at IS NOT NULL),
		count(*) FILTER (WHERE author = 'user' AND deleted_at IS NULL)
		FROM note_comments WHERE note_id = $1`, id).Scan(&relayLive, &relayGone, &own); err != nil {
		t.Fatal(err)
	}
	if relayLive != 0 || relayGone != 1 || own != 1 {
		t.Fatalf("after conversion: live relay %d, retracted relay %d, owner's %d; want 0, 1, 1", relayLive, relayGone, own)
	}
}

// TestEveryWriterOnAReferenceIsQueuedNotStored drives each person/agent write
// path at a reference — the owner's status control and Mark complete, the
// machine status and comment routes, the owner's comment, the task_set_status
// and task_comment tools, the worker's native comment and status tools, and
// the dispatch advance — and requires that NONE changed the row and EACH
// queued exactly one write for the plugin. The same writes on an ordinary task
// are stored: the control that the paths work at all.
func TestEveryWriterOnAReferenceIsQueuedNotStored(t *testing.T) {
	f := newRefFixture(t)
	refID := f.mustUpsert(t, f.key(t), refSnap("in_progress", "in progress", time.Now(), nil))
	openRef := f.mustUpsert(t, f.key(t), refSnap("open", "to do", time.Now(), nil))
	plain := f.task(t, "an ordinary task")
	worker := f.byHand(t, notes.Note{ID: refID}, "refworker")

	write := func(id int64) {
		t.Helper()
		ids := strconv.FormatInt(id, 10)
		// The owner's status control.
		req := httptest.NewRequest(http.MethodPatch, "/tasks/"+ids+"/status", strings.NewReader(url.Values{"status": {"ready_for_review"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if rec := f.serve(f.srv.handleNoteStatus, req, pv(id)); rec.Code/100 != 2 {
			t.Fatalf("PATCH /tasks/%d/status: HTTP %d %s", id, rec.Code, rec.Body)
		}
		// The machine status route.
		req = httptest.NewRequest(http.MethodPatch, "/api/tasks/"+ids+"/status", strings.NewReader(`{"status":"open"}`))
		if rec := f.serve(f.srv.handleAPITaskStatus, req, pv(id)); rec.Code/100 != 2 {
			t.Fatalf("PATCH /api/tasks/%d/status: HTTP %d %s", id, rec.Code, rec.Body)
		}
		// The machine comment route.
		req = httptest.NewRequest(http.MethodPost, "/api/tasks/"+ids+"/comments", strings.NewReader(`{"body":"machine report"}`))
		if rec := f.serve(f.srv.handleAPITaskComment, req, pv(id)); rec.Code/100 != 2 {
			t.Fatalf("POST /api/tasks/%d/comments: HTTP %d %s", id, rec.Code, rec.Body)
		}
		// The owner's comment.
		req = httptest.NewRequest(http.MethodPost, "/tasks/"+ids+"/comments", strings.NewReader(url.Values{"body": {"owner says"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if rec := f.serve(f.srv.handleNoteComment, req, pv(id)); rec.Code/100 != 2 {
			t.Fatalf("POST /tasks/%d/comments: HTTP %d %s", id, rec.Code, rec.Body)
		}
		// The all-agents tools, as the chief.
		args, _ := json.Marshal(map[string]any{"id": id, "body": "chief notes"})
		if text := f.srv.toolTaskComment(f.asOwner, callerOfAgent(f.chief), string(args)); strings.Contains(text, `"error"`) {
			t.Fatalf("task_comment on %d: %s", id, text)
		}
		args, _ = json.Marshal(map[string]any{"id": id, "status": "in_progress"})
		if text := f.srv.toolTaskSetStatus(f.asOwner, callerOfAgent(f.chief), string(args)); strings.Contains(text, `"error"`) {
			t.Fatalf("task_set_status on %d: %s", id, text)
		}
	}

	beforeStatus, _, _, beforeComments := f.row(t, refID)
	write(refID)
	// The worker bound to the reference: its native tools.
	bound := worker
	bound.NoteID = &refID
	if text := f.srv.dispatchAgentTool(f.asOwner, bound, "agent_comment_task", `{"Body":"worker report"}`); strings.Contains(text, `"error"`) {
		t.Fatalf("agent_comment_task: %s", text)
	}
	if text := f.srv.dispatchAgentTool(f.asOwner, bound, "agent_set_task_status", `{"Status":"ready_for_review"}`); strings.Contains(text, `"error"`) {
		t.Fatalf("agent_set_task_status: %s", text)
	}
	// The dispatch advance (open → in_progress) on an open reference.
	f.srv.advanceTaskToInProgress(f.asOwner, openRef)

	if st, _, _, c := f.row(t, refID); st != beforeStatus || c != beforeComments {
		t.Fatalf("a write on the reference was STORED: status %q→%q, comments %d→%d", beforeStatus, st, beforeComments, c)
	}
	want := []string{
		"status:ready_for_review:user",
		"status:open:api",
		"comment:machine report:api",
		"comment:owner says:user",
		"comment:chief notes:" + f.chief.Name,
		"status:in_progress:" + f.chief.Name,
		"comment:worker report:" + worker.Name,
		"status:ready_for_review:" + worker.Name,
	}
	if got := f.queued(t, refID); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("queued writes on the reference:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if st, _, _, _ := f.row(t, openRef); st != notes.StatusOpen {
		t.Fatalf("the dispatch advance STORED on a reference: status %q", st)
	}
	if got := f.queued(t, openRef); strings.Join(got, ",") != "status:in_progress:dispatch-advance" {
		t.Fatalf("the dispatch advance on a reference queued %v, want one status:in_progress write", got)
	}

	// Control: the same paths on an ordinary task are stored, and queue nothing.
	_, _, _, plainBefore := f.row(t, plain.ID)
	write(plain.ID)
	if st, _, _, c := f.row(t, plain.ID); st != notes.StatusInProgress || c != plainBefore+3 {
		t.Fatalf("control: an ordinary task's writes were not stored (status %q, comments %d→%d)", st, plainBefore, c)
	}
	if got := f.queued(t, plain.ID); len(got) != 0 {
		t.Fatalf("control: an ordinary task queued %v", got)
	}
}

// TestASnapshotCloseDoesNotStopTheChiefsAgent: the upstream closing a
// reference moves it to complete here (writerPluginReference) and does NOT run
// the complete-stops-the-agent side effect. The positive control: the OWNER's
// complete on a reference, landed upstream (claimed, reported done), does.
func TestASnapshotCloseDoesNotStopTheChiefsAgent(t *testing.T) {
	f := newRefFixture(t)

	// The chief starts an agent on a copy task, which then becomes a reference.
	key := f.key(t)
	n := f.task(t, "closed upstream", key)
	a := f.dispatch(t, n)
	t0 := time.Now().Add(-time.Hour)
	if id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil)); id != n.ID {
		t.Fatalf("the reference for %s is task %d, want the converted %d", key, id, n.ID)
	}
	f.mustUpsert(t, key, refSnap("complete", "done", t0.Add(time.Minute), nil))
	if st, _, _, _ := f.row(t, n.ID); st != notes.StatusComplete {
		t.Fatalf("the upstream close left the reference %q, want complete", st)
	}
	f.mustNotStop(t, a.ID, "a snapshot close")
	if got := f.agentStatus(t, a.ID); !agentLive(got) {
		t.Fatalf("agent %d is %q after the upstream close, want it still live", a.ID, got)
	}

	// Positive control: the owner completes a reference; the plugin applies it.
	key2 := f.key(t)
	n2 := f.task(t, "owner completes", key2)
	a2 := f.dispatch(t, n2)
	f.mustUpsert(t, key2, refSnap("in_progress", "in progress", t0, nil))
	f.markComplete(t, n2.ID)
	if st, _, _, _ := f.row(t, n2.ID); st != notes.StatusInProgress {
		t.Fatalf("the owner's complete on a reference was stored (%q)", st)
	}
	f.mustNotStop(t, a2.ID, "a complete still only queued")
	code, out := f.post(t, f.tok, "/plugin/v1/task-writes/claim", map[string]any{"wait_s": 0, "limit": 20})
	writes, _ := out["writes"].([]any)
	if code != http.StatusOK || len(writes) != 1 {
		t.Fatalf("claim: HTTP %d %v; want the owner's one complete", code, out["_raw"])
	}
	w := writes[0].(map[string]any)
	if w["kind"] != "status" || w["status"] != "complete" || w["key"] != key2 {
		t.Fatalf("claimed %v; want status complete for %s", w, key2)
	}
	wid := strconv.FormatInt(int64(w["id"].(float64)), 10)
	code, out = f.post(t, f.tok, "/plugin/v1/task-writes/"+wid+"/result", map[string]any{"ok": true,
		"reference": refSnap("complete", "done", t0.Add(time.Minute), nil)})
	if code != http.StatusOK || out["state"] != "done" || out["applied"] != true {
		t.Fatalf("result: HTTP %d %v", code, out["_raw"])
	}
	if got := f.sp.stops(a2.ID, 1); got != 1 {
		t.Fatalf("the owner's complete, landed upstream, stopped agent %d %d time(s), want 1", a2.ID, got)
	}
	if st, _, _, _ := f.row(t, n2.ID); st != notes.StatusComplete {
		t.Fatalf("the result's snapshot left the reference %q, want complete", st)
	}
}

// TestTheClaimHandsOutOnlyThisPluginsWritesAndALapsedLeaseReturns: each plugin
// claims only the writes on references IT owns; a leased write is not handed
// out again until its lease lapses, then it is (attempts 2); a result from
// another plugin is 404; a result is idempotent; a write never claimed is 404.
func TestTheClaimHandsOutOnlyThisPluginsWritesAndALapsedLeaseReturns(t *testing.T) {
	f := newRefFixture(t)
	mine := f.mustUpsert(t, f.key(t), refSnap("in_progress", "in progress", time.Now(), nil))
	code, out, theirs := f.upsert(t, f.tok2, f.key(t), refSnap("in_progress", "in progress", time.Now(), nil), nil)
	if code != http.StatusCreated {
		t.Fatalf("second plugin's reference: HTTP %d %v", code, out["_raw"])
	}
	comment := func(id int64, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/tasks/"+strconv.FormatInt(id, 10)+"/comments", strings.NewReader(url.Values{"body": {body}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if rec := f.serve(f.srv.handleNoteComment, req, pv(id)); rec.Code != http.StatusAccepted {
			t.Fatalf("comment on reference %d: HTTP %d, want 202", id, rec.Code)
		}
	}
	comment(mine, "for the first plugin")
	comment(theirs, "for the second plugin")
	claim := func(tok string) []map[string]any {
		t.Helper()
		code, out := f.post(t, tok, "/plugin/v1/task-writes/claim", map[string]any{"wait_s": 0, "limit": 20})
		if code != http.StatusOK {
			t.Fatalf("claim: HTTP %d %v", code, out["_raw"])
		}
		var got []map[string]any
		for _, w := range out["writes"].([]any) {
			got = append(got, w.(map[string]any))
		}
		return got
	}
	second := claim(f.tok2)
	if len(second) != 1 || second[0]["body"] != "for the second plugin" {
		t.Fatalf("the second plugin claimed %v; want only its own reference's write", second)
	}
	first := claim(f.tok)
	if len(first) != 1 || first[0]["body"] != "for the first plugin" || first[0]["attempts"] != float64(1) {
		t.Fatalf("the first plugin claimed %v; want only its own write, attempt 1", first)
	}
	if again := claim(f.tok); len(again) != 0 {
		t.Fatalf("a leased write was handed out again: %v", again)
	}
	// The lease lapses: the write is pending again, for the same plugin only.
	if _, err := f.pool.Exec(f.asOwner, `UPDATE task_ref_writes SET lease_until = now() - interval '1 second' WHERE note_id = $1`, mine); err != nil {
		t.Fatal(err)
	}
	if other := claim(f.tok2); len(other) != 0 {
		t.Fatalf("a lapsed lease went to ANOTHER plugin: %v", other)
	}
	back := claim(f.tok)
	if len(back) != 1 || back[0]["id"] != first[0]["id"] || back[0]["attempts"] != float64(2) {
		t.Fatalf("after the lease lapsed the first plugin claimed %v; want write %v again, attempt 2", back, first[0]["id"])
	}

	wid := strconv.FormatInt(int64(first[0]["id"].(float64)), 10)
	if code, out := f.post(t, f.tok2, "/plugin/v1/task-writes/"+wid+"/result", map[string]any{"ok": true}); code != http.StatusNotFound {
		t.Fatalf("another plugin's result for write %s: HTTP %d %v, want 404", wid, code, out["_raw"])
	}
	for i := 0; i < 2; i++ {
		code, out := f.post(t, f.tok, "/plugin/v1/task-writes/"+wid+"/result", map[string]any{"ok": false, "error": "the upstream refused it"})
		if code != http.StatusOK || out["state"] != "failed" {
			t.Fatalf("result #%d: HTTP %d %v; want 200 failed", i+1, code, out["_raw"])
		}
	}
	// The first result wins: an ok after a failed changes nothing.
	if code, out := f.post(t, f.tok, "/plugin/v1/task-writes/"+wid+"/result", map[string]any{"ok": true}); code != http.StatusOK || out["state"] != "failed" {
		t.Fatalf("a second, different result: HTTP %d %v; want 200 with the stored failed", code, out["_raw"])
	}
	// A write nobody claimed is 404 too.
	comment(mine, "never claimed")
	var unclaimed int64
	if err := f.pool.QueryRow(f.asOwner, `SELECT id FROM task_ref_writes WHERE note_id = $1 AND state = 'pending'`, mine).Scan(&unclaimed); err != nil {
		t.Fatal(err)
	}
	if code, out := f.post(t, f.tok, "/plugin/v1/task-writes/"+strconv.FormatInt(unclaimed, 10)+"/result", map[string]any{"ok": true}); code != http.StatusNotFound {
		t.Fatalf("a result for a write never claimed: HTTP %d %v, want 404", code, out["_raw"])
	}
}

// TestAReferenceNoticeRoutesOncePerEventID: route_to_chief on a reference is
// named by event_id — the same event resent (even with a newer snapshot)
// routes nothing twice; a new event_id routes again. And it is required.
func TestAReferenceNoticeRoutesOncePerEventID(t *testing.T) {
	f := newRefFixture(t)
	key := f.key(t)
	t0 := time.Now().Add(-time.Hour)
	events := func(id int64) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM plugin_messages WHERE task_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	code, out, _ := f.upsert(t, f.tok, key, refSnap("open", "to do", t0, nil), map[string]any{"route_to_chief": true, "notice": "assigned"})
	if code != http.StatusBadRequest {
		t.Fatalf("route_to_chief without event_id: HTTP %d %v, want 400", code, out["_raw"])
	}
	code, out, id := f.upsert(t, f.tok, key, refSnap("open", "to do", t0, nil),
		map[string]any{"route_to_chief": true, "notice": "assigned", "event_id": "upstream-msg-1"})
	if code != http.StatusCreated || out["routed"] != "chief" {
		t.Fatalf("an assigned reference notice: HTTP %d %v; want 201 routed to the chief", code, out["_raw"])
	}
	if got := events(id); got != 1 {
		t.Fatalf("task events after the first notice: %d, want 1", got)
	}
	code, out, _ = f.upsert(t, f.tok, key, refSnap("in_progress", "in progress", t0.Add(time.Minute), nil),
		map[string]any{"route_to_chief": true, "notice": "assigned", "event_id": "upstream-msg-1"})
	if code != http.StatusOK || events(id) != 1 {
		t.Fatalf("the same event_id resent: HTTP %d %v, %d task event(s); want 200 and still 1", code, out["_raw"], events(id))
	}
	f.upsert(t, f.tok, key, refSnap("in_progress", "in progress", t0.Add(2*time.Minute), nil),
		map[string]any{"route_to_chief": true, "notice": "assigned", "event_id": "upstream-msg-2"})
	if got := events(id); got != 2 {
		t.Fatalf("a new event_id: %d task event(s), want 2", got)
	}
}

// TestReferenceRequestsThatAreRefused: the shapes a reference upsert refuses
// (400), and the copy-mode request with an event_id.
func TestReferenceRequestsThatAreRefused(t *testing.T) {
	f := newRefFixture(t)
	key := f.key(t)
	now := time.Now()
	for name, body := range map[string]map[string]any{
		"comment with reference":     {"key": key, "reference": refSnap("open", "", now, nil), "comment": "hi"},
		"create with reference":      {"key": key, "reference": refSnap("open", "", now, nil), "create": map[string]any{"title": "t", "body": "b"}},
		"an upstream status word":    {"key": key, "reference": refSnap("awaiting review", "", now, nil)},
		"no updated_at":              {"key": key, "reference": map[string]any{"title": "t", "status": "open"}},
		"a bad event_id":             {"key": key, "reference": refSnap("open", "", now, nil), "event_id": "has space"},
		"event_id without reference": {"key": key, "create": map[string]any{"title": "t", "body": "b"}, "event_id": "e1"},
	} {
		if code, out := f.post(t, f.tok, "/plugin/v1/tasks", body); code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d %v, want 400", name, code, out["_raw"])
		}
	}
	var n int
	if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM notes WHERE tags @> ARRAY[$1]::text[]`, key).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused requests filed %d task(s) (%v)", n, err)
	}
}

// TestOpeningAStaleReferenceQueuesOneRefresh: GET /api/tasks/{id} on a
// reference whose snapshot is older than 5 minutes queues ONE refresh, however
// often it is opened; a fresh one queues none.
func TestOpeningAStaleReferenceQueuesOneRefresh(t *testing.T) {
	f := newRefFixture(t)
	id := f.mustUpsert(t, f.key(t), refSnap("open", "to do", time.Now(), nil))
	get := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+strconv.FormatInt(id, 10), nil)
		if rec := f.serve(f.srv.handleAPITaskGet, req, pv(id)); rec.Code != http.StatusOK {
			t.Fatalf("GET /api/tasks/%d: HTTP %d", id, rec.Code)
		}
	}
	refreshes := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(f.asOwner, `SELECT count(*) FROM task_ref_writes WHERE note_id = $1 AND kind = 'refresh'`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	get()
	if n := refreshes(); n != 0 {
		t.Fatalf("a fresh reference opened queued %d refresh(es), want 0", n)
	}
	if _, err := f.pool.Exec(f.asOwner, `UPDATE notes SET ext_synced_at = now() - interval '6 minutes' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	get()
	get()
	if n := refreshes(); n != 1 {
		t.Fatalf("a stale reference opened twice queued %d refresh(es), want exactly 1", n)
	}
}

// TestAReferenceIsNeverMergedAway: retiring a merge loser is a status write,
// which on a reference would close the upstream task; the merge is refused.
func TestAReferenceIsNeverMergedAway(t *testing.T) {
	f := newRefFixture(t)
	refID := f.mustUpsert(t, f.key(t), refSnap("open", "to do", time.Now(), nil))
	other := f.task(t, "the other one")
	req := httptest.NewRequest(http.MethodPost, "/tasks/merge", strings.NewReader(url.Values{
		"winner": {strconv.FormatInt(other.ID, 10)}, "loser": {strconv.FormatInt(refID, 10)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := f.serve(f.srv.handleTaskMerge, req, nil); rec.Code != http.StatusConflict {
		t.Fatalf("merging a reference away: HTTP %d %s, want 409", rec.Code, rec.Body)
	}
	if got := f.queued(t, refID); len(got) != 0 {
		t.Fatalf("a refused merge queued %v", got)
	}
	// The other way round is fine.
	f.merge(t, refID, other.ID)
}

// TestThePluginListsItsNamespacesLiveRows: GET /plugin/v1/tasks lists the
// owner's live rows in a namespace — copies and references, with comments.
func TestThePluginListsItsNamespacesLiveRows(t *testing.T) {
	f := newRefFixture(t)
	refKey, copyKey := f.key(t), f.key(t)
	refID := f.mustUpsert(t, refKey, refSnap("in_progress", "in progress", time.Now(), nil))
	if code, out := f.post(t, f.tok, "/plugin/v1/tasks", map[string]any{"key": copyKey,
		"create": map[string]any{"title": "c", "body": "b"}, "comment": "relay"}); code != http.StatusCreated {
		t.Fatalf("copy create: %d %v", code, out["_raw"])
	}
	rec := f.connFixture.call(t, http.MethodGet, "/plugin/v1/tasks?namespace=tracker&live=true", nil, map[string]string{"Authorization": "Bearer " + f.tok})
	var out struct {
		Tasks []struct {
			ID          int64  `json:"id"`
			Key         string `json:"key"`
			Status      string `json:"status"`
			IsReference bool   `json:"is_reference"`
			Comments    []struct {
				Author string `json:"author"`
				Body   string `json:"body"`
			} `json:"comments"`
		} `json:"tasks"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list: HTTP %d %s", rec.Code, rec.Body)
	}
	seen := map[string]string{}
	for _, r := range out.Tasks {
		kind := "copy"
		if r.IsReference {
			kind = "reference"
		}
		if r.Key == copyKey && (len(r.Comments) != 1 || r.Comments[0].Body != "relay") {
			t.Errorf("the copy row's comments: %+v", r.Comments)
		}
		if r.Key == refKey && r.ID != refID {
			t.Errorf("the reference row is %d, want %d", r.ID, refID)
		}
		seen[r.Key] = kind + ":" + r.Status
	}
	if seen[refKey] != "reference:in_progress" || seen[copyKey] != "copy:open" {
		t.Fatalf("listed %v; want %s as a reference in progress and %s as an open copy", seen, refKey, copyKey)
	}
	if rec := f.connFixture.call(t, http.MethodGet, "/plugin/v1/tasks?namespace=gate&live=true", nil,
		map[string]string{"Authorization": "Bearer " + f.tok}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a namespace that is not a plugin task namespace: HTTP %d, want 400", rec.Code)
	}
}
