package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/sse"
)

// The reference rules the first audit round asked for (plugin_task_refs.go,
// task_ref_queue.go, notes/pgstore_refs.go). Like plugin_task_refs_pg_test.go
// this file uses only symbols that exist without the rules, so each test goes
// red on the pre-rule tree for a BEHAVIOURAL reason.

// comment posts the owner's comment on task id (202 on a reference).
func (f *refFixture) comment(t *testing.T, id int64, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+strconv.FormatInt(id, 10)+"/comments", strings.NewReader(url.Values{"body": {body}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := f.serve(f.srv.handleNoteComment, req, pv(id)); rec.Code != http.StatusAccepted {
		t.Fatalf("comment on reference %d: HTTP %d, want 202", id, rec.Code)
	}
}

// claim claims up to 20 writes with tok.
func (f *refFixture) claim(t *testing.T, tok string) []map[string]any {
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

// writeRows is task id's writes as "body|status:state:plugin", oldest first.
func (f *refFixture) writeRows(t *testing.T, id int64) []string {
	t.Helper()
	rows, err := f.pool.Query(f.asOwner, `SELECT body || status, state, plugin_id, lease_until IS NULL FROM task_ref_writes WHERE note_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var what, state string
		var plugin int64
		var noLease bool
		if err := rows.Scan(&what, &state, &plugin, &noLease); err != nil {
			t.Fatal(err)
		}
		lease := ""
		if !noLease {
			lease = "+lease"
		}
		out = append(out, fmt.Sprintf("%s:%s%s:%d", what, state, lease, plugin))
	}
	return out
}

func (f *refFixture) holder(t *testing.T, id int64) int64 {
	t.Helper()
	var p int64
	if err := f.pool.QueryRow(f.asOwner, `SELECT ext_plugin_id FROM notes WHERE id = $1`, id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestARotatedPluginAdoptsItsReferencesAndTheirWrites: a plugin is rotated by
// revoking it and creating a new one. The new plugin's upsert for a key the
// REVOKED one held adopts the reference — the same row — with its unfinished
// writes (the claimed one pending again), and claims them. A deleted holder is
// adopted the same way. A LIVE holder is still a 409.
func TestARotatedPluginAdoptsItsReferencesAndTheirWrites(t *testing.T) {
	f := newRefFixture(t)
	key := f.key(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	id := f.mustUpsert(t, key, refSnap("in_progress", "in progress", t0, nil))
	f.comment(t, id, "claimed by the old plugin")
	if got := f.claim(t, f.tok); len(got) != 1 {
		t.Fatalf("the old plugin claimed %v, want its one write", got)
	}
	f.comment(t, id, "still pending")
	f.comment(t, id, "finished")
	// Settle the third directly: a finished write must NOT move.
	if _, err := f.pool.Exec(f.asOwner, `UPDATE task_ref_writes SET state = 'done', finished_at = now()
		WHERE note_id = $1 AND body = 'finished'`, id); err != nil {
		t.Fatal(err)
	}
	done := "finished:done:" + strconv.FormatInt(f.plugin, 10)

	// The control: while the holder is live, another plugin is refused.
	if code, out, _ := f.upsert(t, f.tok2, key, refSnap("in_progress", "in progress", t0, nil), nil); code != http.StatusConflict {
		t.Fatalf("another plugin's upsert while the holder is live: HTTP %d %v, want 409", code, out["_raw"])
	}

	keyQ := "namespace=tracker&live=true&key=" + url.QueryEscape(key)
	if l := f.list(t, f.tok2, keyQ); l.code != http.StatusOK || len(l.Tasks) != 0 {
		t.Fatalf("another plugin's listing of a LIVE holder's key: HTTP %d %s, want 200 and no row", l.code, l.raw)
	}

	if ok, err := f.plugins.Revoke(f.asOwner, f.plugin); err != nil || !ok {
		t.Fatalf("revoke the holder: %v %v", ok, err)
	}
	// The rotated plugin must FIND what it may adopt before it upserts: its
	// reconcile lists, and a notice looks its key up. A listing that showed only
	// its own references left every revoked plugin's reference unfound.
	if l := f.list(t, f.tok2, keyQ); l.code != http.StatusOK || len(l.Tasks) != 1 || l.Tasks[0].ID != id {
		t.Fatalf("the new plugin's key lookup of a revoked holder's reference: HTTP %d %s, want task %d", l.code, l.raw, id)
	}
	if l := f.list(t, f.tok2, "namespace=tracker&live=true&open=true"); !strings.Contains(","+l.keys()+",", ","+key+",") {
		t.Fatalf("the new plugin's reconcile listing %q leaves out the revoked holder's reference %s", l.keys(), key)
	}
	code, out, again := f.upsert(t, f.tok2, key, refSnap("in_progress", "in progress", t0, nil), nil)
	if code != http.StatusOK || again != id {
		t.Fatalf("the new plugin's upsert after the holder was revoked: HTTP %d %v; want 200 naming task %d", code, out["_raw"], id)
	}
	if h := f.holder(t, id); h != f.plugin2 {
		t.Fatalf("the reference is held by plugin %d after the adoption, want %d", h, f.plugin2)
	}
	p2 := strconv.FormatInt(f.plugin2, 10)
	want := []string{"claimed by the old plugin:pending:" + p2, "still pending:pending:" + p2, done}
	if got := f.writeRows(t, id); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the writes after the adoption:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	got := f.claim(t, f.tok2)
	if len(got) != 1 || got[0]["body"] != "claimed by the old plugin" || got[0]["attempts"] != float64(2) {
		t.Fatalf("the new plugin claimed %v; want the old plugin's lapsed write, attempt 2", got)
	}

	// A DELETED holder is adopted too.
	tok3, p3, err := f.plugins.Create(f.asOwner, "third-tracker", "Third tracker", "third-tracker")
	if err != nil {
		t.Fatal(err)
	}
	key2 := f.key(t)
	code, out, id2 := f.upsert(t, tok3, key2, refSnap("open", "to do", t0, nil), nil)
	if code != http.StatusCreated {
		t.Fatalf("the third plugin's reference: HTTP %d %v", code, out["_raw"])
	}
	if _, err := f.pool.Exec(f.asOwner, `DELETE FROM plugins WHERE id = $1`, p3.ID); err != nil {
		t.Fatalf("delete the third plugin: %v", err)
	}
	if code, out, again := f.upsert(t, f.tok2, key2, refSnap("open", "to do", t0, nil), nil); code != http.StatusOK || again != id2 {
		t.Fatalf("an upsert for a deleted plugin's reference: HTTP %d %v; want 200 naming task %d", code, out["_raw"], id2)
	}
	if h := f.holder(t, id2); h != f.plugin2 {
		t.Fatalf("the deleted plugin's reference is held by %d, want %d", h, f.plugin2)
	}
}

// TestAReferencesTitleAndBodyAreNotEditable: an owner edit that would change
// a reference's title or body is refused (409) on BOTH edit paths and changes
// nothing; the edit modal renders them read-only (no form field); the other
// fields stay editable; an ordinary task's title and body are editable.
func TestAReferencesTitleAndBodyAreNotEditable(t *testing.T) {
	f := newRefFixture(t)
	id := f.mustUpsert(t, f.key(t), refSnap("open", "to do", time.Now(), nil))
	_, _, extKey, _ := f.row(t, id)
	plain := f.task(t, "an ordinary task")

	apiEdit := func(id int64, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+strconv.FormatInt(id, 10), strings.NewReader(body))
		return f.serve(f.srv.handleAPITaskEdit, req, pv(id))
	}
	formEdit := func(id int64, v url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/tasks/"+strconv.FormatInt(id, 10)+"/edit", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return f.serve(f.srv.handleNoteEdit, req, pv(id))
	}
	const msg = "this task is a reference; edit it upstream"
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"PATCH title":    apiEdit(id, `{"title":"my own title"}`),
		"PATCH body":     apiEdit(id, `{"body":"my own body"}`),
		"form title":     formEdit(id, url.Values{"title": {"my own title"}}),
		"form body":      formEdit(id, url.Values{"body": {"my own body"}}),
		"form both kept": formEdit(id, url.Values{"title": {"Upstream task"}, "body": {"a new body"}}),
	} {
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), msg) {
			t.Errorf("%s on a reference: HTTP %d %q; want 409 %q", name, rec.Code, rec.Body.String(), msg)
		}
	}
	if _, title, _, _ := f.row(t, id); title != "Upstream task" {
		t.Fatalf("a refused edit changed the reference's title to %q", title)
	}
	var body string
	if err := f.pool.QueryRow(f.asOwner, `SELECT body FROM notes WHERE id = $1`, id).Scan(&body); err != nil || body != "The upstream's description." {
		t.Fatalf("a refused edit changed the reference's body to %q (%v)", body, err)
	}

	// The other fields stay editable on both paths; an unchanged title is no change.
	if rec := apiEdit(id, `{"title":"Upstream task","model":"some-model"}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH of a reference's model (title unchanged): HTTP %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := formEdit(id, url.Values{"directory": {"/work/dir"}, "tag": {extKey}}); rec.Code != http.StatusOK {
		t.Fatalf("the reference's edit form without title/body: HTTP %d %s, want 200", rec.Code, rec.Body)
	}
	var dir string
	if err := f.pool.QueryRow(f.asOwner, `SELECT directory FROM notes WHERE id = $1`, id).Scan(&dir); err != nil || dir != "/work/dir" {
		t.Fatalf("the reference's directory after the form: %q (%v)", dir, err)
	}

	// The modal: read-only, no title/body field.
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/"+strconv.FormatInt(id, 10)+"/edit", nil)
	modal := f.serve(f.srv.handleNoteEditModal, req, pv(id)).Body.String()
	if strings.Contains(modal, `name="title"`) || strings.Contains(modal, `name="body"`) || !strings.Contains(modal, "Upstream task") ||
		!strings.Contains(modal, "Edit them there") {
		t.Fatalf("the reference's edit modal offers an editable title/body or hides them:\n%s", modal)
	}

	// Control: an ordinary task's title and body are editable, and its modal has the fields.
	if rec := apiEdit(plain.ID, `{"title":"renamed","body":"new body"}`); rec.Code != http.StatusOK {
		t.Fatalf("control: PATCH of an ordinary task: HTTP %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodGet, "/ui/tasks/"+strconv.FormatInt(plain.ID, 10)+"/edit", nil)
	if modal := f.serve(f.srv.handleNoteEditModal, req, pv(plain.ID)).Body.String(); !strings.Contains(modal, `name="title"`) || !strings.Contains(modal, `name="body"`) {
		t.Fatalf("control: an ordinary task's edit modal has no title/body field")
	}
}

// TestAnUnchangedSnapshotRewritesNothing: the same snapshot pushed twice (a
// reconcile's resend) rewrites the row once: one task-changed broadcast,
// updated_at unchanged by the second, only the sync time advanced. A changed
// one still applies.
func TestAnUnchangedSnapshotRewritesNothing(t *testing.T) {
	f := newRefFixture(t)
	key := f.key(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	id := f.mustUpsert(t, key, refSnap("open", "to do", t0, nil))

	bus := sse.New(512)
	f.srv.bus = bus
	_, events := bus.Subscribe(f.owner)
	changed := func() int {
		n := 0
		for {
			select {
			case ev := <-events:
				if ev.Name == EventTaskChanged && ev.Data == `{"id":"`+strconv.FormatInt(id, 10)+`"}` {
					n++
				}
			default:
				return n
			}
		}
	}
	stamps := func() (updated, synced time.Time) {
		t.Helper()
		if err := f.pool.QueryRow(f.asOwner, `SELECT updated_at, ext_synced_at FROM notes WHERE id = $1`, id).Scan(&updated, &synced); err != nil {
			t.Fatal(err)
		}
		return
	}
	snap := refSnap("open", "to do", t0.Add(time.Minute), map[string]any{"title": "Renamed upstream",
		"comments": []map[string]any{{"id": "c1", "author": "A. Person", "body": "hello", "created_at": t0.Format(time.RFC3339)}}})
	push := func(i int) {
		t.Helper()
		if code, out, again := f.upsert(t, f.tok, key, snap, nil); code != http.StatusOK || again != id || out["applied"] != true {
			t.Fatalf("upsert #%d: HTTP %d %v", i, code, out["_raw"])
		}
	}
	push(1)
	u1, s1 := stamps()
	time.Sleep(20 * time.Millisecond)
	push(2)
	u2, s2 := stamps()
	if !u2.Equal(u1) {
		t.Errorf("the identical snapshot moved updated_at %s → %s", u1, u2)
	}
	if !s2.After(s1) {
		t.Errorf("the identical snapshot did not advance ext_synced_at (%s → %s)", s1, s2)
	}
	if n := changed(); n != 1 {
		t.Fatalf("%d task-changed broadcast(s) for two identical snapshots, want 1", n)
	}
	// A changed one (the label only) applies and broadcasts again.
	snap["status_label"] = "to do (triaged)"
	f.upsert(t, f.tok, key, snap, nil)
	if n := changed(); n != 1 {
		t.Fatalf("%d task-changed broadcast(s) for a changed snapshot, want 1", n)
	}
}

// listing is GET /plugin/v1/tasks with tok and query, decoded.
type listing struct {
	Tasks []struct {
		ID          int64  `json:"id"`
		Key         string `json:"key"`
		Status      string `json:"status"`
		IsReference bool   `json:"is_reference"`
	} `json:"tasks"`
	Truncated bool   `json:"truncated"`
	NextAfter *int64 `json:"next_after"`
	code      int
	raw       string
}

func (f *refFixture) list(t *testing.T, tok, query string) listing {
	t.Helper()
	rec := f.connFixture.call(t, http.MethodGet, "/plugin/v1/tasks?"+query, nil, map[string]string{"Authorization": "Bearer " + tok})
	var out listing
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	out.code, out.raw = rec.Code, rec.Body.String()
	return out
}

func (l listing) keys() string {
	var ks []string
	for _, r := range l.Tasks {
		ks = append(ks, r.Key)
	}
	return strings.Join(ks, ",")
}

// TestTheListingIsScopedKeyedFilteredAndPaged: GET /plugin/v1/tasks lists this
// plugin's references and the namespace's copy rows — never another plugin's
// reference; key= names at most one row; open=true leaves out complete ones;
// after= pages, with next_after naming the cursor.
func TestTheListingIsScopedKeyedFilteredAndPaged(t *testing.T) {
	f := newRefFixture(t)
	t0 := time.Now().Add(-time.Hour)
	mineKey, theirsKey, doneKey, copyKey := f.key(t), f.key(t), f.key(t), f.key(t)
	mine := f.mustUpsert(t, mineKey, refSnap("open", "to do", t0, nil))
	if code, out, _ := f.upsert(t, f.tok2, theirsKey, refSnap("open", "to do", t0, nil), nil); code != http.StatusCreated {
		t.Fatalf("the other plugin's reference: HTTP %d %v", code, out["_raw"])
	}
	f.mustUpsert(t, doneKey, refSnap("complete", "done", t0, nil))
	if code, out := f.post(t, f.tok, "/plugin/v1/tasks", map[string]any{"key": copyKey,
		"create": map[string]any{"title": "c", "body": "b"}}); code != http.StatusCreated {
		t.Fatalf("copy create: %d %v", code, out["_raw"])
	}

	all := f.list(t, f.tok, "namespace=tracker&live=true")
	if all.code != http.StatusOK || all.keys() != strings.Join([]string{mineKey, doneKey, copyKey}, ",") || all.Truncated || all.NextAfter != nil {
		t.Fatalf("the first plugin's listing: HTTP %d %s; want exactly its two references and the copy row, ascending, untruncated", all.code, all.raw)
	}
	if theirs := f.list(t, f.tok2, "namespace=tracker&live=true"); theirs.keys() != strings.Join([]string{theirsKey, copyKey}, ",") {
		t.Fatalf("the second plugin's listing: %s; want only its own reference and the copy row", theirs.raw)
	}
	if got := f.list(t, f.tok, "namespace=tracker&live=true&key="+url.QueryEscape(theirsKey)); got.code != http.StatusOK || len(got.Tasks) != 0 {
		t.Fatalf("key= naming another plugin's reference: %s; want no rows", got.raw)
	}
	if got := f.list(t, f.tok, "namespace=tracker&live=true&key="+url.QueryEscape(mineKey)); len(got.Tasks) != 1 || got.Tasks[0].ID != mine || !got.Tasks[0].IsReference {
		t.Fatalf("key= naming my reference: %s; want exactly task %d", got.raw, mine)
	}
	if got := f.list(t, f.tok, "namespace=tracker&live=true&open=true"); got.keys() != strings.Join([]string{mineKey, copyKey}, ",") {
		t.Fatalf("open=true: %s; want the complete reference left out", got.raw)
	}
	if got := f.list(t, f.tok, "namespace=tracker&live=true&after="+strconv.FormatInt(mine, 10)); got.keys() != strings.Join([]string{doneKey, copyKey}, ",") {
		t.Fatalf("after=%d: %s; want the rows after it", mine, got.raw)
	}
	for _, bad := range []string{"after=x", "after=-1", "open=yes", "key=other:1", "key=tracker"} {
		if got := f.list(t, f.tok, "namespace=tracker&live=true&"+bad); got.code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d %s, want 400", bad, got.code, got.raw)
		}
	}

	// Paging: 520 more copy rows (SQL, for speed), one page of 500 and the rest.
	if _, err := f.pool.Exec(f.asOwner, `INSERT INTO notes (directory, title, body, tags)
		SELECT '', 'bulk ' || g, 'b', ARRAY['tracker:bulk' || g] FROM generate_series(1, 520) AS g`); err != nil {
		t.Fatal(err)
	}
	p1 := f.list(t, f.tok, "namespace=tracker&live=true")
	if p1.code != http.StatusOK || len(p1.Tasks) != 500 || !p1.Truncated || p1.NextAfter == nil || *p1.NextAfter != p1.Tasks[499].ID {
		t.Fatalf("page 1: HTTP %d, %d rows, truncated %v, next_after %v; want 500, true, the last id", p1.code, len(p1.Tasks), p1.Truncated, p1.NextAfter)
	}
	p2 := f.list(t, f.tok, "namespace=tracker&live=true&after="+strconv.FormatInt(*p1.NextAfter, 10))
	if len(p2.Tasks) != 3+520-500 || p2.Truncated || p2.NextAfter != nil || p2.Tasks[0].ID <= *p1.NextAfter {
		t.Fatalf("page 2: %d rows, truncated %v, next_after %v; want %d, false, null", len(p2.Tasks), p2.Truncated, p2.NextAfter, 3+520-500)
	}
	for i := 1; i < len(p1.Tasks); i++ {
		if p1.Tasks[i].ID <= p1.Tasks[i-1].ID {
			t.Fatalf("page 1 is not ascending at %d", i)
		}
	}
}

// TestDestroyingAnAgentDoesNotReopenAReference: tearing down the last agent
// on an in-progress REFERENCE queues nothing for the upstream and leaves it;
// on an ordinary task the same teardown reopens it (the control).
func TestDestroyingAnAgentDoesNotReopenAReference(t *testing.T) {
	f := newRefFixture(t)
	refID := f.mustUpsert(t, f.key(t), refSnap("in_progress", "in progress", time.Now(), nil))
	a := f.byHand(t, notes.Note{ID: refID}, "refdestroy")
	f.srv.reopenTaskAfterDestroy(f.asOwner, a)
	if got := f.queued(t, refID); len(got) != 0 {
		t.Fatalf("destroying the agent on a reference queued %v for the upstream", got)
	}
	if st, _, _, _ := f.row(t, refID); st != notes.StatusInProgress {
		t.Fatalf("the reference is %q after the teardown, want in_progress", st)
	}

	plain := f.task(t, "an ordinary task")
	f.humanPatch(t, plain.ID, notes.StatusInProgress)
	b := f.byHand(t, plain, "plaindestroy")
	f.srv.reopenTaskAfterDestroy(f.asOwner, b)
	if st, _, _, _ := f.row(t, plain.ID); st != notes.StatusOpen {
		t.Fatalf("control: an ordinary task is %q after its agent's teardown, want open", st)
	}
}

// TestAReferenceIsDispatchableInProgressAndTheAdvanceFollowsTheSnapshot: the
// chief may start an agent on a reference that is in progress upstream (no
// live agent on it); the dispatch advance then queues nothing, since the
// snapshot is not open; a second start is refused (a live agent); a reference
// in review is not dispatchable. The advance on a row whose own status reads
// open while its SNAPSHOT is in progress queues nothing either.
func TestAReferenceIsDispatchableInProgressAndTheAdvanceFollowsTheSnapshot(t *testing.T) {
	f := newRefFixture(t)
	n, err := f.notes.Get(f.asOwner, f.mustUpsert(t, f.key(t), refSnap("in_progress", "in progress", time.Now(), nil)))
	if err != nil {
		t.Fatal(err)
	}
	if r, err := f.srv.chiefDispatchTaskRefusal(f.asOwner, f.owner, n); err != nil || r != "" {
		t.Fatalf("an in-progress reference with no agent is refused: %q (%v)", r, err)
	}
	f.dispatch(t, n)
	if got := f.queued(t, n.ID); len(got) != 0 {
		t.Fatalf("the dispatch advance on an in-progress snapshot queued %v", got)
	}
	if r, err := f.srv.chiefDispatchTaskRefusal(f.asOwner, f.owner, n); err != nil || r == "" {
		t.Fatalf("a reference with a live agent on it is dispatchable again (%v)", err)
	}
	review := f.mustUpsert(t, f.key(t), refSnap("ready_for_review", "in review", time.Now(), nil))
	rn, err := f.notes.Get(f.asOwner, review)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := f.srv.chiefDispatchTaskRefusal(f.asOwner, f.owner, rn); r == "" {
		t.Fatalf("a reference in review is dispatchable")
	}

	// The row reads open, the snapshot in progress: the snapshot decides.
	odd := f.mustUpsert(t, f.key(t), refSnap("in_progress", "in progress", time.Now(), nil))
	if _, err := f.pool.Exec(f.asOwner, `UPDATE notes SET status = 'open' WHERE id = $1`, odd); err != nil {
		t.Fatal(err)
	}
	f.srv.advanceTaskToInProgress(f.asOwner, odd)
	if got := f.queued(t, odd); len(got) != 0 {
		t.Fatalf("the advance on an in-progress snapshot queued %v", got)
	}
}

// TestOneTasksWritesAreHandedOutInOrderAndGivenUpAfterTen: a claim hands out
// at most the EARLIEST unfinished write of each task; the next one comes only
// once it is settled. A write handed out 10 times without a result is failed
// ("gave up after 10 attempts") and stops blocking the next.
func TestOneTasksWritesAreHandedOutInOrderAndGivenUpAfterTen(t *testing.T) {
	f := newRefFixture(t)
	a := f.mustUpsert(t, f.key(t), refSnap("open", "to do", time.Now(), nil))
	b := f.mustUpsert(t, f.key(t), refSnap("open", "to do", time.Now(), nil))
	for _, body := range []string{"a1", "a2", "a3"} {
		f.comment(t, a, body)
	}
	f.comment(t, b, "b1")
	bodies := func(ws []map[string]any) string {
		var out []string
		for _, w := range ws {
			out = append(out, fmt.Sprintf("%v@%v", w["body"], w["attempts"]))
		}
		return strings.Join(out, ",")
	}
	got := f.claim(t, f.tok)
	if bodies(got) != "a1@1,b1@1" {
		t.Fatalf("the first claim handed out %s; want only each task's earliest write (a1@1,b1@1)", bodies(got))
	}
	if again := f.claim(t, f.tok); len(again) != 0 {
		t.Fatalf("a claim while a1 is claimed handed out %s", bodies(again))
	}
	wid := strconv.FormatInt(int64(got[0]["id"].(float64)), 10)
	if code, out := f.post(t, f.tok, "/plugin/v1/task-writes/"+wid+"/result", map[string]any{"ok": true}); code != http.StatusOK {
		t.Fatalf("a1's result: HTTP %d %v", code, out["_raw"])
	}
	if next := f.claim(t, f.tok); bodies(next) != "a2@1" {
		t.Fatalf("after a1 settled the claim handed out %s, want a2@1", bodies(next))
	}
	lapse := func(attempts int) {
		t.Helper()
		if _, err := f.pool.Exec(f.asOwner, `UPDATE task_ref_writes SET attempts = $2, lease_until = now() - interval '1 second'
			WHERE note_id = $1 AND body = 'a2'`, a, attempts); err != nil {
			t.Fatal(err)
		}
	}
	// Nine attempts lapsed: handed out a tenth time.
	lapse(9)
	if next := f.claim(t, f.tok); bodies(next) != "a2@10" {
		t.Fatalf("a2 lapsed after 9 attempts: the claim handed out %s, want a2@10", bodies(next))
	}
	// Ten lapsed: given up, and a3 comes.
	lapse(10)
	if next := f.claim(t, f.tok); bodies(next) != "a3@1" {
		t.Fatalf("a2 lapsed after 10 attempts: the claim handed out %s, want a3@1", bodies(next))
	}
	var state, why string
	if err := f.pool.QueryRow(f.asOwner, `SELECT state, error FROM task_ref_writes WHERE note_id = $1 AND body = 'a2'`, a).Scan(&state, &why); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || why != "gave up after 10 attempts" {
		t.Fatalf("a2 after 10 attempts: %s %q; want failed \"gave up after 10 attempts\"", state, why)
	}
}

// getFailsStore is a task store whose every read fails.
type getFailsStore struct{ notes.Store }

func (getFailsStore) Get(context.Context, int64) (notes.Note, error) {
	return notes.Note{}, errors.New("the read failed")
}

// TestAFailedReadFailsAConditionalStatusWriteClosed: on the agent / conditional
// path the reference check reads the row; when that read fails the write is
// refused with the error, never let through to a local write.
func TestAFailedReadFailsAConditionalStatusWriteClosed(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0), ext: Extensions{Notes: getFailsStore{}}}
	for name, ctx := range map[string]context.Context{
		"agent":       notes.ForAgent(context.Background()),
		"conditional": notes.OnlyFrom(context.Background(), notes.StatusInProgress),
	} {
		queued, err := s.queueStatusIfReference(ctx, writerDestroyReopen, 7, notes.StatusOpen)
		if err == nil || queued {
			t.Errorf("%s: a failed read answered queued=%v err=%v; want the read's error", name, queued, err)
		}
	}
}
