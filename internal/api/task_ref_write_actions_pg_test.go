package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// THE OWNER'S RETRY AND DISMISS OF A FAILED REFERENCE WRITE end to end
// (task_ref_write_actions.go; notes.PGStore.RetryRefWrite / DismissRefWrite;
// migration 0075): the real routes and doors, real Postgres, two members, a
// plugin and an agent of the first.
//
// ⚠ THIS FILE USES ONLY SYMBOLS THAT EXIST WITHOUT THE FEATURE (routes are
// driven over HTTP, rows read back as to_jsonb), so it compiles on the
// pre-change tree and goes red there for a BEHAVIOURAL reason — the routes are
// unrouted, there is no dismissed_at, a retried write is handed out beside one
// in flight — rather than for a build error.

type refActFixture struct {
	*connFixture
	a, b              int64
	asA, asB          context.Context
	tokA, tokB        string // personal tokens
	plugTok, plugTok2 string
	plugin, plugin2   int64
	plugins           *plugins.PGStore
	agentTok          string
	chiefName         string
}

func newRefActFixture(t *testing.T) *refActFixture {
	t.Helper()
	f := &refActFixture{}
	f.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		f.plugins = plugins.NewPG(pool)
		ext.Plugins, ext.Notes = f.plugins, notes.NewPG(pool)
		ext.APITokens, ext.Users = apitokens.NewPG(pool), users.NewPG(pool)
	})
	if err := notes.SetPluginTaskNamespaces([]string{"tracker"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = notes.SetPluginTaskNamespaces(nil) })
	f.a, f.b = f.member(t), f.member(t)
	f.asA, f.asB = asUser(f.a), asUser(f.b)
	// The session acts as whoever X-Test-User names (password mode).
	f.srv.sessionUserOverride = func(r *http.Request) int64 {
		if r.Header.Get("X-Test-User") == "b" {
			return f.b
		}
		return f.a
	}
	var err error
	if f.tokA, _, err = apitokens.NewPG(f.pool).Create(f.asA, "a", 0, apitokens.ScopeFull); err != nil {
		t.Fatal(err)
	}
	if f.tokB, _, err = apitokens.NewPG(f.pool).Create(f.asB, "b", 0, apitokens.ScopeFull); err != nil {
		t.Fatal(err)
	}
	tok, p, err := f.plugins.Create(f.asA, "example-tracker", "Example tracker", "example-tracker")
	if err != nil {
		t.Fatal(err)
	}
	tok2, p2, err := f.plugins.Create(f.asA, "other-tracker", "Other tracker", "other-tracker")
	if err != nil {
		t.Fatal(err)
	}
	f.plugTok, f.plugin, f.plugTok2, f.plugin2 = tok, p.ID, tok2, p2.ID
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.agentTok, f.chiefName = "refact-chief-token-"+suffix, "refact-chief-"+suffix
	if _, err := agents.NewPG(f.pool).Create(f.asA, agents.Agent{Name: f.chiefName, Namespace: "devpod-" + f.chiefName,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: f.agentTok}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *refActFixture) member(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(authz.System(f.ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"refact-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM notes WHERE owner_id = $1`, `DELETE FROM plugins WHERE owner_id = $1`,
			`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM api_tokens WHERE user_id = $1`,
			`DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = f.pool.Exec(sys, q, id)
		}
	})
	return id
}

// do sends one request; hdr adds headers; body is JSON (a map) or a form
// (url.Values) or nil.
func (f *refActFixture) do(t *testing.T, method, path string, body any, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	var rd io.Reader
	h := map[string]string{}
	switch b := body.(type) {
	case nil:
	case url.Values:
		rd, h["Content-Type"] = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	default:
		raw, _ := json.Marshal(b)
		rd, h["Content-Type"] = strings.NewReader(string(raw)), "application/json"
	}
	rec := f.call(t, method, path, rd, merge(h, hdr))
	return rec.Code, rec.Body.String(), rec.Header()
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

// sessionA / sessionB are a signed-in browser on muster's own page (htmx).
func (f *refActFixture) sessionA() map[string]string { return f.operator() }
func (f *refActFixture) sessionB() map[string]string {
	return merge(f.operator(), map[string]string{"X-Test-User": "b"})
}

// plainForm is sessionA without htmx: a form post a browser makes without JS.
func (f *refActFixture) plainForm() map[string]string {
	return map[string]string{"Cookie": sessionCookieFor(f.srv), "Origin": "http://example.com"}
}

// reference files a reference for a fresh key through plugin tok.
func (f *refActFixture) reference(t *testing.T, tok string) (int64, string) {
	t.Helper()
	key := "tracker:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	return f.upsert(t, tok, key), key
}

func (f *refActFixture) upsert(t *testing.T, tok, key string) int64 {
	t.Helper()
	code, body, _ := f.do(t, http.MethodPost, "/plugin/v1/tasks", map[string]any{"key": key,
		"reference": refSnap("open", "open", time.Now(), nil)}, bearer(tok))
	var out struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if (code != http.StatusOK && code != http.StatusCreated) || out.ID == 0 {
		t.Fatalf("reference upsert for %s: HTTP %d %s", key, code, body)
	}
	return out.ID
}

// comment queues a comment on reference id as A; the write's id.
func (f *refActFixture) comment(t *testing.T, id int64, text string) int64 {
	t.Helper()
	code, body, _ := f.do(t, http.MethodPost, "/api/tasks/"+strconv.FormatInt(id, 10)+"/comments",
		map[string]any{"body": text}, bearer(f.tokA))
	if code != http.StatusAccepted {
		t.Fatalf("comment on reference %d: HTTP %d %s; want 202 queued", id, code, body)
	}
	var wid int64
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT max(id) FROM task_ref_writes WHERE note_id = $1 AND body = $2`,
		id, text).Scan(&wid); err != nil {
		t.Fatal(err)
	}
	return wid
}

// claim is one claim by plugin tok: the write ids handed out, in order.
func (f *refActFixture) claim(t *testing.T, tok string) []int64 {
	t.Helper()
	code, body, _ := f.do(t, http.MethodPost, "/plugin/v1/task-writes/claim", map[string]any{"wait_s": 0, "limit": 20}, bearer(tok))
	if code != http.StatusOK {
		t.Fatalf("claim: HTTP %d %s", code, body)
	}
	var out struct {
		Writes []struct {
			ID int64 `json:"id"`
		} `json:"writes"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, w := range out.Writes {
		ids = append(ids, w.ID)
	}
	return ids
}

func (f *refActFixture) result(t *testing.T, tok string, wid int64, ok bool) {
	t.Helper()
	body := map[string]any{"ok": ok}
	if !ok {
		body["error"] = "refused upstream"
	}
	if code, out, _ := f.do(t, http.MethodPost, "/plugin/v1/task-writes/"+strconv.FormatInt(wid, 10)+"/result", body, bearer(tok)); code != http.StatusOK {
		t.Fatalf("result for write %d: HTTP %d %s", wid, code, out)
	}
}

// failed queues a comment on id, has plugin tok claim and fail it; its id.
func (f *refActFixture) failed(t *testing.T, tok string, id int64, text string) int64 {
	t.Helper()
	wid := f.comment(t, id, text)
	if got := f.claim(t, tok); len(got) != 1 || got[0] != wid {
		t.Fatalf("claim for the write to fail: %v, want [%d]", got, wid)
	}
	f.result(t, tok, wid, false)
	return wid
}

// row is write wid as stored (to_jsonb, so a column the tree lacks is absent).
func (f *refActFixture) row(t *testing.T, wid int64) map[string]any {
	t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT to_jsonb(w) FROM task_ref_writes w WHERE id = $1`, wid).Scan(&raw); err != nil {
		t.Fatalf("write %d: %v", wid, err)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (f *refActFixture) rowText(t *testing.T, wid int64) string {
	t.Helper()
	raw, _ := json.Marshal(f.row(t, wid))
	return string(raw)
}

// refActPath is the task page's action route (there is no /api twin).
func refActPath(task, wid int64, action string) string {
	return "/tasks/" + strconv.FormatInt(task, 10) + "/ref-writes/" + strconv.FormatInt(wid, 10) + "/" + action
}

// TestAnOwnerRetriesAFailedReferenceWrite: a failed write is listed with its
// actions (page and machine read); the machine retry puts it back to pending
// with attempts, error, lease and finish cleared, and the next claim hands it
// out again; the task page's plain form does the same and answers 303 back to
// the task page.
func TestAnOwnerRetriesAFailedReferenceWrite(t *testing.T) {
	f := newRefActFixture(t)
	id, _ := f.reference(t, f.plugTok)
	wid := f.failed(t, f.plugTok, id, "my reply")
	ids, wids := strconv.FormatInt(id, 10), strconv.FormatInt(wid, 10)

	code, page, _ := f.do(t, http.MethodGet, "/tasks/"+ids, nil, f.plainForm())
	if code != http.StatusOK || !strings.Contains(page, `data-ref-write-retry="`+wids+`"`) ||
		!strings.Contains(page, `data-ref-write-dismiss="`+wids+`"`) {
		t.Errorf("the task page (HTTP %d) does not offer Retry and Dismiss on failed write %d", code, wid)
	}
	if !strings.Contains(page, "failed: refused upstream") {
		t.Errorf("the task page does not show failed write %d with its error", wid)
	}
	// There is no machine door (D1): the /api spelling is unrouted.
	if code, _, _ := f.do(t, http.MethodPost, "/api"+refActPath(id, wid, "retry"), nil, bearer(f.tokA)); code != http.StatusNotFound &&
		code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api%s: HTTP %d, want it unrouted", refActPath(id, wid, "retry"), code)
	}

	code, body, _ := f.do(t, http.MethodPost, refActPath(id, wid, "retry"), url.Values{}, f.sessionA())
	if code != http.StatusOK || !strings.Contains(body, `id="task-`+ids+`"`) {
		t.Fatalf("page retry (htmx): HTTP %d %s; want 200 and the card", code, truncate(body, 200))
	}
	r := f.row(t, wid)
	if r["state"] != "pending" || r["attempts"] != float64(0) || r["error"] != "" || r["lease_until"] != nil || r["finished_at"] != nil {
		t.Fatalf("after the retry the write is %v; want pending, attempts 0, no error, lease or finish", r)
	}
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != wid {
		t.Fatalf("the claim after a retry handed out %v, want [%d]", got, wid)
	}
	if a := f.row(t, wid)["attempts"]; a != float64(1) {
		t.Errorf("the retried write was handed out at attempts %v, want 1", a)
	}

	// The task page's plain form (no JavaScript): 303 back to the task page.
	f.result(t, f.plugTok, wid, false)
	code, _, hdr := f.do(t, http.MethodPost, refActPath(id, wid, "retry"), url.Values{}, f.plainForm())
	if code != http.StatusSeeOther || hdr.Get("Location") != "/tasks/"+ids {
		t.Fatalf("the page's retry form: HTTP %d → %q; want 303 → /tasks/%d", code, hdr.Get("Location"), id)
	}
	if st := f.row(t, wid)["state"]; st != "pending" {
		t.Fatalf("after the page's retry the write is %v, want pending", st)
	}
	// Cross-origin: refused like every operator write, and nothing changes.
	f.claim(t, f.plugTok)
	f.result(t, f.plugTok, wid, false)
	before := f.rowText(t, wid)
	if code, _, _ := f.do(t, http.MethodPost, refActPath(id, wid, "retry"), url.Values{},
		map[string]string{"Cookie": sessionCookieFor(f.srv), "Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Errorf("a cross-origin retry: HTTP %d, want 403", code)
	}
	if after := f.rowText(t, wid); after != before {
		t.Errorf("a refused cross-origin retry changed the write:\n before %s\n after  %s", before, after)
	}
}

// TestAnOwnerDismissesAFailedReferenceWrite: the page's dismiss (htmx) answers
// the card without the write, the row keeps state failed with dismissed_at,
// it is gone from the page and the machine read, and it can be neither
// retried nor dismissed again (409, unchanged). The machine dismiss answers
// the write with dismissed_at.
func TestAnOwnerDismissesAFailedReferenceWrite(t *testing.T) {
	f := newRefActFixture(t)
	id, _ := f.reference(t, f.plugTok)
	wid := f.failed(t, f.plugTok, id, "dismiss me")
	ids, wids := strconv.FormatInt(id, 10), strconv.FormatInt(wid, 10)

	// From the task page itself (htmx names it), so the card comes back in the
	// detail shape that carries the writes.
	code, card, _ := f.do(t, http.MethodPost, refActPath(id, wid, "dismiss"), url.Values{},
		merge(f.sessionA(), map[string]string{"HX-Current-URL": "http://example.com/tasks/" + ids}))
	if code != http.StatusOK || !strings.Contains(card, `data-task-ref-section`) || strings.Contains(card, `data-ref-write="`+wids+`"`) {
		t.Fatalf("the page's dismiss: HTTP %d; want 200 and the card without write %d\n%s", code, wid, truncate(card, 400))
	}
	r := f.row(t, wid)
	if r["state"] != "failed" || r["dismissed_at"] == nil {
		t.Fatalf("after the dismiss the write is %v; want still failed, with dismissed_at", r)
	}
	if _, page, _ := f.do(t, http.MethodGet, "/tasks/"+ids, nil, f.plainForm()); strings.Contains(page, `data-ref-write="`+wids+`"`) {
		t.Errorf("the task page still shows dismissed write %d", wid)
	}
	before := f.rowText(t, wid)
	for _, action := range []string{"retry", "dismiss"} {
		if code, body, _ := f.do(t, http.MethodPost, refActPath(id, wid, action), url.Values{}, f.sessionA()); code != http.StatusConflict {
			t.Errorf("%s of a DISMISSED write: HTTP %d %s, want 409", action, code, body)
		}
	}
	if after := f.rowText(t, wid); after != before {
		t.Errorf("a refused action on a dismissed write changed it:\n before %s\n after  %s", before, after)
	}

	wid2 := f.failed(t, f.plugTok, id, "dismiss me by machine")
	// The plain form (no JavaScript): 303 back to the task page.
	code, _, hdr := f.do(t, http.MethodPost, refActPath(id, wid2, "dismiss"), url.Values{}, f.plainForm())
	if r := f.row(t, wid2); code != http.StatusSeeOther || hdr.Get("Location") != "/tasks/"+ids || r["state"] != "failed" || r["dismissed_at"] == nil {
		t.Fatalf("the page's dismiss form: HTTP %d → %q, write %v; want 303 → /tasks/%d, failed with dismissed_at", code, hdr.Get("Location"), r, id)
	}
}

// TestOnlyAFailedReferenceWriteCanBeRetriedOrDismissed: a pending, a claimed
// and a done write each answer 409 to both actions on both doors, and none
// changes.
func TestOnlyAFailedReferenceWriteCanBeRetriedOrDismissed(t *testing.T) {
	f := newRefActFixture(t)
	idDone, _ := f.reference(t, f.plugTok)
	done := f.comment(t, idDone, "applied")
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != done {
		t.Fatalf("claim: %v, want [%d]", got, done)
	}
	f.result(t, f.plugTok, done, true)
	idClaimed, _ := f.reference(t, f.plugTok)
	claimed := f.comment(t, idClaimed, "in flight")
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != claimed {
		t.Fatalf("claim: %v, want [%d]", got, claimed)
	}
	idPending, _ := f.reference(t, f.plugTok)
	pending := f.comment(t, idPending, "waiting")

	for _, c := range []struct {
		name      string
		task, wid int64
		state     string
	}{{"pending", idPending, pending, "pending"}, {"claimed", idClaimed, claimed, "claimed"}, {"done", idDone, done, "done"}} {
		if st := f.row(t, c.wid)["state"]; st != c.state {
			t.Fatalf("fixture: the %s write is %v", c.name, st)
		}
		before := f.rowText(t, c.wid)
		for _, action := range []string{"retry", "dismiss"} {
			if code, body, _ := f.do(t, http.MethodPost, refActPath(c.task, c.wid, action), url.Values{}, f.sessionA()); code != http.StatusConflict {
				t.Errorf("htmx %s of a %s write: HTTP %d %s, want 409", action, c.name, code, body)
			}
			if code, body, _ := f.do(t, http.MethodPost, refActPath(c.task, c.wid, action), url.Values{}, f.plainForm()); code != http.StatusConflict {
				t.Errorf("plain-form %s of a %s write: HTTP %d %s, want 409", action, c.name, code, truncate(body, 200))
			}
		}
		if after := f.rowText(t, c.wid); after != before {
			t.Errorf("a refused action on the %s write changed it:\n before %s\n after  %s", c.name, before, after)
		}
	}
}

// TestARetriedWriteIsNotHandedOutBesideOneInFlight: write 1 fails; write 2,
// queued after it, is claimed. Write 1 back at pending (here by SQL, so this
// pins the CLAIM's rule on any tree) must not be handed out while write 2 is
// claimed — the plugin would apply two of one task's writes at once — and is
// handed out once write 2 is settled.
func TestARetriedWriteIsNotHandedOutBesideOneInFlight(t *testing.T) {
	f := newRefActFixture(t)
	id, _ := f.reference(t, f.plugTok)
	w1 := f.failed(t, f.plugTok, id, "first")
	w2 := f.comment(t, id, "second")
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != w2 {
		t.Fatalf("claim: %v, want [%d] (a failed write blocks nothing)", got, w2)
	}
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE task_ref_writes SET state = 'pending', attempts = 0, error = '',
		finished_at = NULL WHERE id = $1`, w1); err != nil {
		t.Fatal(err)
	}
	if got := f.claim(t, f.plugTok); len(got) != 0 {
		t.Fatalf("with write %d claimed, the claim handed out %v; want nothing (one of a task's writes at a time)", w2, got)
	}
	f.result(t, f.plugTok, w2, true)
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != w1 {
		t.Fatalf("after write %d settled the claim handed out %v, want [%d]", w2, got, w1)
	}
}

// TestARetryKeepsTheTasksWriteOrder, through the route: (1) a write retried
// while a later one is claimed waits for it; (2) two failed writes retried
// newest first are still handed out oldest first, one at a time; (3) a write
// that hit the attempts cap starts again from zero rather than being failed
// again by the next claim.
func TestARetryKeepsTheTasksWriteOrder(t *testing.T) {
	f := newRefActFixture(t)
	retry := func(task, wid int64) {
		t.Helper()
		if code, body, _ := f.do(t, http.MethodPost, refActPath(task, wid, "retry"), url.Values{}, f.sessionA()); code != http.StatusOK {
			t.Fatalf("retry of write %d: HTTP %d %s", wid, code, body)
		}
	}
	// (1)
	id, _ := f.reference(t, f.plugTok)
	w1 := f.failed(t, f.plugTok, id, "one")
	w2 := f.comment(t, id, "two")
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != w2 {
		t.Fatalf("claim: %v, want [%d]", got, w2)
	}
	retry(id, w1)
	if got := f.claim(t, f.plugTok); len(got) != 0 {
		t.Fatalf("(1) the retried write was handed out beside claimed write %d: %v", w2, got)
	}
	f.result(t, f.plugTok, w2, true)
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != w1 {
		t.Fatalf("(1) after write %d settled: %v, want [%d]", w2, got, w1)
	}
	f.result(t, f.plugTok, w1, true)

	// (2)
	id2, _ := f.reference(t, f.plugTok)
	a := f.failed(t, f.plugTok, id2, "older")
	b := f.failed(t, f.plugTok, id2, "newer")
	retry(id2, b)
	retry(id2, a)
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != a {
		t.Fatalf("(2) the claim after retrying both handed out %v, want only the older [%d]", got, a)
	}
	f.result(t, f.plugTok, a, true)
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != b {
		t.Fatalf("(2) after the older settled: %v, want [%d]", got, b)
	}
	f.result(t, f.plugTok, b, true)

	// (3) The cap: a write handed out 10 times without a result.
	id3, _ := f.reference(t, f.plugTok)
	capped := f.comment(t, id3, "capped")
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE task_ref_writes SET attempts = $2 WHERE id = $1`,
		capped, notes.RefWriteMaxAttempts); err != nil {
		t.Fatal(err)
	}
	if got := f.claim(t, f.plugTok); len(got) != 0 {
		t.Fatalf("(3) fixture: the capped write was handed out: %v", got)
	}
	if r := f.row(t, capped); r["state"] != "failed" {
		t.Fatalf("(3) fixture: the capped write is %v, want failed", r)
	}
	retry(id3, capped)
	if got := f.claim(t, f.plugTok); len(got) != 1 || got[0] != capped {
		t.Fatalf("(3) the retried capped write: claim handed out %v, want [%d]", got, capped)
	}
	if r := f.row(t, capped); r["attempts"] != float64(1) {
		t.Errorf("(3) the retried capped write went out at attempts %v, want 1", r["attempts"])
	}
}

// TestARetriedWriteGoesToThePluginHoldingTheReferenceNow: plugin 1's write
// fails, plugin 1 is revoked, plugin 2 adopts the reference (adoption moves
// only unfinished writes). The retry hands the write to plugin 2.
func TestARetriedWriteGoesToThePluginHoldingTheReferenceNow(t *testing.T) {
	f := newRefActFixture(t)
	id, key := f.reference(t, f.plugTok)
	wid := f.failed(t, f.plugTok, id, "before the rotation")
	if _, err := f.plugins.Revoke(f.asA, f.plugin); err != nil {
		t.Fatal(err)
	}
	if again := f.upsert(t, f.plugTok2, key); again != id {
		t.Fatalf("the adopting upsert named task %d, want %d", again, id)
	}
	if code, body, _ := f.do(t, http.MethodPost, refActPath(id, wid, "retry"), url.Values{}, f.sessionA()); code != http.StatusOK {
		t.Fatalf("retry: HTTP %d %s", code, body)
	}
	if p := f.row(t, wid)["plugin_id"]; p != float64(f.plugin2) {
		t.Fatalf("the retried write is for plugin %v, want the holder %d", p, f.plugin2)
	}
	if got := f.claim(t, f.plugTok2); len(got) != 1 || got[0] != wid {
		t.Fatalf("the holder's claim: %v, want [%d]", got, wid)
	}
}

// TestRefWriteRetryAndDismissAreTheOwnersOnly: an agent's own token (either
// header), the chief asserted through the service credential, and a plugin
// token are each refused (401) on both machine routes, an agent's token on the
// page routes too, and the write stays failed.
func TestRefWriteRetryAndDismissAreTheOwnersOnly(t *testing.T) {
	f := newRefActFixture(t)
	id, _ := f.reference(t, f.plugTok)
	wid := f.failed(t, f.plugTok, id, "not yours to retry")
	before := f.rowText(t, wid)
	creds := []struct {
		name string
		hdr  map[string]string
	}{
		{"the agent's token (Bearer)", bearer(f.agentTok)},
		{"the agent's token (X-Muster-Token)", map[string]string{"X-Muster-Token": f.agentTok}},
		{"the chief through the service token", map[string]string{"Authorization": "Bearer " + connTestServiceToken, "X-Muster-Actor": f.chiefName}},
		{"a plugin token", bearer(f.plugTok)},
	}
	for _, action := range []string{"retry", "dismiss"} {
		for _, c := range creds {
			if code, body, _ := f.do(t, http.MethodPost, refActPath(id, wid, action), url.Values{},
				merge(c.hdr, map[string]string{"HX-Request": "true", "Origin": "http://example.com"})); code != http.StatusUnauthorized {
				t.Errorf("%s with %s: HTTP %d %s, want 401", action, c.name, code, truncate(body, 200))
			}
		}
		if code, body, _ := f.do(t, http.MethodPost, refActPath(id, wid, action), url.Values{},
			merge(bearer(f.agentTok), map[string]string{"HX-Request": "true", "Origin": "http://example.com"})); code != http.StatusUnauthorized {
			t.Errorf("page %s with the agent's token: HTTP %d %s, want 401", action, code, truncate(body, 200))
		}
	}
	if after := f.rowText(t, wid); after != before {
		t.Errorf("a refused caller changed the write:\n before %s\n after  %s", before, after)
	}
	// The control: the owner's own token on the same route does reach it.
	if code, body, _ := f.do(t, http.MethodPost, refActPath(id, wid, "retry"), url.Values{}, f.sessionA()); code != http.StatusOK {
		t.Fatalf("CONTROL: the owner's retry: HTTP %d %s", code, body)
	}
}

// TestRefWriteRetryAndDismissAreOwnerIsolated: B (token and session) naming A's
// write gets exactly the ghost's answer; B naming B's OWN task with A's write
// id, and A naming another of A's tasks with it, are 404 — and A's write is
// unchanged by every one of them.
func TestRefWriteRetryAndDismissAreOwnerIsolated(t *testing.T) {
	f := newRefActFixture(t)
	id, _ := f.reference(t, f.plugTok)
	wid := f.failed(t, f.plugTok, id, "A's write")
	code, body, _ := f.do(t, http.MethodPost, "/api/tasks", map[string]any{"title": "B's task", "body": "b"}, bearer(f.tokB))
	var bTask struct {
		ID int64 `json:"id"`
	}
	if _ = json.Unmarshal([]byte(body), &bTask); code != http.StatusOK && code != http.StatusCreated || bTask.ID == 0 {
		t.Fatalf("B's task: HTTP %d %s", code, body)
	}
	otherA, _ := f.reference(t, f.plugTok)
	before := f.rowText(t, wid)
	const ghost = 9_000_000_000
	for _, action := range []string{"retry", "dismiss"} {
		for _, door := range []struct {
			name string
			hdr  map[string]string
			body any
		}{{"B's session (htmx)", f.sessionB(), url.Values{}},
			{"B's plain form", merge(f.plainForm(), map[string]string{"X-Test-User": "b"}), url.Values{}}} {
			code, body, _ := f.do(t, http.MethodPost, refActPath(id, wid, action), door.body, door.hdr)
			gcode, gbody, _ := f.do(t, http.MethodPost, refActPath(ghost, ghost, action), door.body, door.hdr)
			if code != http.StatusNotFound || code != gcode || body != gbody {
				t.Errorf("%s %s of A's write: HTTP %d %q; the ghost's is %d %q (want the same 404)", door.name, action, code,
					truncate(body, 120), gcode, truncate(gbody, 120))
			}
			if code, body, _ := f.do(t, http.MethodPost, refActPath(bTask.ID, wid, action), door.body, door.hdr); code != http.StatusNotFound {
				t.Errorf("%s %s of A's write under B's own task: HTTP %d %s, want 404", door.name, action, code, truncate(body, 120))
			}
		}
		if code, body, _ := f.do(t, http.MethodPost, refActPath(otherA, wid, action), url.Values{}, f.sessionA()); code != http.StatusNotFound {
			t.Errorf("A's %s of its write under ANOTHER of A's tasks: HTTP %d %s, want 404", action, code, body)
		}
	}
	if after := f.rowText(t, wid); after != before {
		t.Errorf("another owner's (or another task's) action changed A's write:\n before %s\n after  %s", before, after)
	}
	// The control: A's own retry of it is a 200.
	if code, body, _ := f.do(t, http.MethodPost, refActPath(id, wid, "retry"), url.Values{}, f.sessionA()); code != http.StatusOK {
		t.Fatalf("CONTROL: A's own retry: HTTP %d %s", code, body)
	}
}

// status queues a status write on reference id as A; the write's id.
func (f *refActFixture) status(t *testing.T, id int64, status string) int64 {
	t.Helper()
	code, body, _ := f.do(t, http.MethodPatch, "/api/tasks/"+strconv.FormatInt(id, 10)+"/status",
		map[string]any{"status": status}, bearer(f.tokA))
	if code != http.StatusAccepted {
		t.Fatalf("status %s on reference %d: HTTP %d %s; want 202 queued", status, id, code, body)
	}
	var wid int64
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT max(id) FROM task_ref_writes WHERE note_id = $1 AND kind = 'status' AND status = $2`,
		id, status).Scan(&wid); err != nil {
		t.Fatal(err)
	}
	return wid
}

// settle has plugin tok claim exactly wid and report ok.
func (f *refActFixture) settle(t *testing.T, tok string, wid int64, ok bool) {
	t.Helper()
	if got := f.claim(t, tok); len(got) != 1 || got[0] != wid {
		t.Fatalf("claim: %v, want [%d]", got, wid)
	}
	f.result(t, tok, wid, ok)
}

// TestASupersededStatusWriteIsNotRetried: a failed STATUS write with a LATER
// status write on the same task already done upstream is refused a retry
// (409, unchanged) — it would undo the newer status — and the page offers it
// Dismiss but not Retry. A failed comment queued before that done status, and
// a failed status write that is the latest, stay retryable; the superseded
// one can still be dismissed.
func TestASupersededStatusWriteIsNotRetried(t *testing.T) {
	f := newRefActFixture(t)
	id, _ := f.reference(t, f.plugTok)
	ids := strconv.FormatInt(id, 10)
	older := f.status(t, id, notes.StatusInProgress)
	f.settle(t, f.plugTok, older, false)
	comment := f.failed(t, f.plugTok, id, "a comment before the newer status")
	newer := f.status(t, id, notes.StatusReadyForReview)
	f.settle(t, f.plugTok, newer, true)
	latest := f.status(t, id, notes.StatusOpen)
	f.settle(t, f.plugTok, latest, false)
	olderID, commentID, latestID := strconv.FormatInt(older, 10), strconv.FormatInt(comment, 10), strconv.FormatInt(latest, 10)

	_, page, _ := f.do(t, http.MethodGet, "/tasks/"+ids, nil, f.plainForm())
	if strings.Contains(page, `data-ref-write-retry="`+olderID+`"`) {
		t.Errorf("the page offers Retry on superseded status write %d", older)
	}
	for _, want := range []string{`data-ref-write-dismiss="` + olderID + `"`, `data-ref-write-retry="` + commentID + `"`,
		`data-ref-write-retry="` + latestID + `"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the task page lacks %s", want)
		}
	}

	before := f.rowText(t, older)
	code, body, _ := f.do(t, http.MethodPost, refActPath(id, older, "retry"), url.Values{}, f.sessionA())
	if code != http.StatusConflict || !strings.Contains(body, "already applied upstream") {
		t.Errorf("retry of the superseded status write: HTTP %d %q; want 409 naming the newer status upstream", code, truncate(body, 200))
	}
	if after := f.rowText(t, older); after != before {
		t.Errorf("a refused retry changed the superseded write:\n before %s\n after  %s", before, after)
	}
	for _, w := range []int64{comment, latest} {
		if code, body, _ := f.do(t, http.MethodPost, refActPath(id, w, "retry"), url.Values{}, f.sessionA()); code != http.StatusOK {
			t.Errorf("retry of write %d (not superseded): HTTP %d %s, want 200", w, code, truncate(body, 200))
		}
	}
	if code, body, _ := f.do(t, http.MethodPost, refActPath(id, older, "dismiss"), url.Values{}, f.sessionA()); code != http.StatusOK {
		t.Errorf("dismiss of the superseded status write: HTTP %d %s, want 200", code, truncate(body, 200))
	}
}
