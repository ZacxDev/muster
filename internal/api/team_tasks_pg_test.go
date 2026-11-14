package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// THE TEAM-TASK WALK (team sharing phase 4, slice 4.3; plan §6 isolation
// walk items 1–5). Team T: A manages it, B operates in it, V views it, N is
// in no team. The team's task carries a canary in its body, its comment and
// its attachment's name, and every /tasks route is driven against THE TEAM'S
// ids, through each person's browser session:
//
//   - N sees no canary on any route, is answered no 2xx on any route naming
//     the team's task, and changes nothing;
//   - V reads the canary (the task page, the board, grouped under the team),
//     and every write V sends is refused and changes nothing;
//   - B files, comments and changes status (operator) and is refused the
//     dismiss (manager);
//   - A (CONTROL) dismisses, so each refusal above is about the role.

type teamTaskWalk struct {
	w             *isoWorld
	team          int64
	teamName      string
	v, n          isoUser
	canary        string
	task          int64
	comment       int64
	attach        int64
	routes        []string
	sys           context.Context
	teamRowsQuery string
}

func newTeamTaskWalk(t *testing.T) *teamTaskWalk {
	t.Helper()
	w := newIsoWorld(t)
	w.srv.ext.Users = users.NewPG(w.pool)
	tw := &teamTaskWalk{w: w, sys: w.sys, canary: "tcnry" + w.suffix}
	store := users.NewPG(w.pool)
	by := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	person := func(name string) isoUser {
		var id int64
		if err := w.pool.QueryRow(w.sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+"-"+w.suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return isoUser{name: name, uid: id}
	}
	tw.v, tw.n = person("tt-viewer"), person("tt-outsider")
	// The team's name carries markdown: the board's group heading must show
	// it plain (textplain.Title), never the markers.
	tw.teamName = "**Platform** team " + w.suffix
	team, err := store.CreateTeam(w.sys, by, tw.teamName, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	tw.team = team.ID
	for uid, role := range map[int64]authz.TeamRole{w.b.uid: authz.TeamOperator, tw.v.uid: authz.TeamViewer} {
		if err := store.SetTeamMember(w.sys, by, tw.team, uid, role); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM notes WHERE owner_id = $1`, tw.team)
		_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, tw.team)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE object = $1 OR team_id = $1::bigint`, users.TeamObject(tw.team))
		_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = ANY($1)`, []int64{tw.team, tw.v.uid, tw.n.uid})
	})
	tw.teamRowsQuery = `SELECT COALESCE((SELECT json_agg(x ORDER BY x.id)::text FROM notes x WHERE owner_id = $1), '') ||
		COALESCE((SELECT json_agg(x ORDER BY x.id)::text FROM note_comments x WHERE owner_id = $1), '') ||
		COALESCE((SELECT json_agg(x.filename ORDER BY x.id)::text FROM note_attachments x WHERE owner_id = $1), '') ||
		COALESCE((SELECT json_agg(x ORDER BY x.note_id)::text FROM task_sessions x WHERE owner_id = $1), '')`

	// B FILES the team's task through the real create form (Owner = T) and
	// comments on it through the real comment route.
	rec := tw.post(t, w.b.uid, "POST", "/tasks", "multipart:"+url.Values{"body": {"team task " + tw.canary}, "owner": {strconv.FormatInt(tw.team, 10)}}.Encode())
	if !is2xx(rec.Code) {
		t.Fatalf("B (operator) filing a team task: %d %s", rec.Code, truncate(rec.Body.String(), 200))
	}
	if err := w.pool.QueryRow(w.sys, `SELECT id FROM notes WHERE owner_id = $1 AND body LIKE '%' || $2`, tw.team, tw.canary).Scan(&tw.task); err != nil {
		t.Fatalf("the operator's team task is not owned by the team: %v", err)
	}
	if rec := tw.post(t, w.b.uid, "POST", "/tasks/"+strconv.FormatInt(tw.task, 10)+"/comments", url.Values{"body": {"comment " + tw.canary}}.Encode()); !is2xx(rec.Code) {
		t.Fatalf("B (operator) commenting on the team task: %d %s", rec.Code, truncate(rec.Body.String(), 200))
	}
	var author int64
	if err := w.pool.QueryRow(w.sys, `SELECT id, author_id FROM note_comments WHERE note_id = $1`, tw.task).Scan(&tw.comment, &author); err != nil || author != w.b.uid {
		t.Fatalf("the team task's comment: author_id %d, %v; want B %d", author, err, w.b.uid)
	}
	if err := w.pool.QueryRow(w.sys, `INSERT INTO note_attachments (note_id, owner_id, filename, content_type, size_bytes, data)
		VALUES ($1, $2, $3, 'text/plain', 1, 'x') RETURNING id`, tw.task, tw.team, "file-"+tw.canary+".txt").Scan(&tw.attach); err != nil {
		t.Fatal(err)
	}
	// Every session route under /tasks and /ui/tasks, but the create and the
	// move, which are driven below with their own forms.
	for _, r := range sortedRoutes() {
		c := isolationClasses[r]
		_, path, _ := strings.Cut(r, " ")
		if (c == classParam || c == classList) && (strings.HasPrefix(path, "/tasks") || strings.HasPrefix(path, "/ui/tasks")) &&
			r != "POST /tasks" && r != "POST /tasks/{id}/move" {
			tw.routes = append(tw.routes, r)
		}
	}
	if len(tw.routes) < 15 {
		t.Fatalf("the walk found %d task routes: %v", len(tw.routes), tw.routes)
	}
	return tw
}

func (tw *teamTaskWalk) post(t *testing.T, uid int64, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return tw.w.do(tw.w.h, method, path, body, cred{sessionUser: uid})
}

func (tw *teamTaskWalk) rows(t *testing.T) string {
	t.Helper()
	var s string
	if err := tw.w.pool.QueryRow(tw.sys, tw.teamRowsQuery, tw.team).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (tw *teamTaskWalk) objs() isoObjs {
	o := tw.w.ghost
	o.task, o.comment, o.attach, o.team, o.tag = tw.task, tw.comment, tw.attach, tw.team, "isowrite"
	return o
}

// walk drives every task route against the team's ids as actor's session.
func (tw *teamTaskWalk) walk(actor isoUser) []isoResult {
	var out []isoResult
	for _, r := range tw.routes {
		m, p, b := tw.w.fill(r, tw.objs(), actor)
		rec := tw.w.do(tw.w.h, m, p, b, cred{sessionUser: actor.uid})
		out = append(out, isoResult{route: r, via: actor.name, status: rec.Code, body: rec.Body.String()})
	}
	return out
}

// TestATeamTaskIsWalkedByEveryRole is the walk. See the file header.
func TestATeamTaskIsWalkedByEveryRole(t *testing.T) {
	tw := newTeamTaskWalk(t)
	w := tw.w
	id := strconv.FormatInt(tw.task, 10)

	// --- N, in no team: nothing seen, nothing changed ---
	before := tw.rows(t)
	if !strings.Contains(before, tw.canary) {
		t.Fatalf("fixture: the team's rows carry no canary: %s", before)
	}
	for _, res := range tw.walk(tw.n) {
		if strings.Contains(res.body, tw.canary) {
			t.Errorf("LEAK: N (not a member) %s → %d carries the team's canary\n  body: %s", res.route, res.status, truncate(res.body, 300))
		}
		if namesObject(res.route) && is2xx(res.status) {
			t.Errorf("N (not a member) %s → %d; want no 2xx on the team's task", res.route, res.status)
		}
	}
	if after := tw.rows(t); after != before {
		t.Errorf("MUTATION: N's walk changed the team's rows:\n before %s\n after  %s", before, after)
	}

	// --- V, a viewer: reads the canary, every write refused ---
	sawPage, sawBoard := false, false
	for _, res := range tw.walk(tw.v) {
		method, _, _ := strings.Cut(res.route, " ")
		if method != http.MethodGet && is2xx(res.status) {
			t.Errorf("V (viewer) %s → %d; want every write refused", res.route, res.status)
		}
		switch res.route {
		case "GET /tasks/{id}":
			sawPage = is2xx(res.status) && strings.Contains(res.body, "team task "+tw.canary) && strings.Contains(res.body, "comment "+tw.canary)
		case "GET /ui/tasks":
			sawBoard = strings.Contains(res.body, tw.canary) && strings.Contains(res.body, `data-task-owner-group="`+strconv.FormatInt(tw.team, 10)+`"`)
		}
	}
	if !sawPage {
		t.Errorf("V (viewer) does not read the team task and its comment on GET /tasks/%s", id)
	}
	if !sawBoard {
		t.Errorf("V (viewer) does not see the team task grouped under the team on the board")
	}
	if after := tw.rows(t); after != before {
		t.Errorf("MUTATION: V's (a viewer's) walk changed the team's rows:\n before %s\n after  %s", before, after)
	}
	for who, uid := range map[string]int64{"V (viewer)": tw.v.uid, "N (not a member)": tw.n.uid} {
		rec := tw.post(t, uid, "POST", "/tasks", "multipart:"+url.Values{"body": {"filed by " + who}, "owner": {strconv.FormatInt(tw.team, 10)}}.Encode())
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s filing a team task: %d; want 403", who, rec.Code)
		}
	}
	if after := tw.rows(t); after != before {
		t.Errorf("MUTATION: a viewer's or outsider's create landed in the team:\n before %s\n after  %s", before, after)
	}

	// --- B, an operator: the operator routes pass, the manager route does not ---
	if rec := tw.post(t, w.b.uid, "PATCH", "/tasks/"+id+"/status", url.Values{"status": {"in_progress"}}.Encode()); !is2xx(rec.Code) {
		t.Errorf("B (operator) changing the team task's status: %d %s", rec.Code, truncate(rec.Body.String(), 200))
	}
	if rec := tw.post(t, w.b.uid, "POST", "/tasks/"+id+"/comments", url.Values{"body": {"second"}}.Encode()); !is2xx(rec.Code) {
		t.Errorf("B (operator) commenting on the team task: %d", rec.Code)
	}
	if rec := tw.post(t, w.b.uid, "DELETE", "/tasks/"+id, ""); rec.Code != http.StatusForbidden {
		t.Errorf("B (operator) dismissing the team task: %d; want 403 (dismiss is a manager's)", rec.Code)
	}
	dismissed := func() bool {
		var gone bool
		if err := w.pool.QueryRow(tw.sys, `SELECT deleted_at IS NOT NULL FROM notes WHERE id = $1`, tw.task).Scan(&gone); err != nil {
			t.Fatal(err)
		}
		return gone
	}
	if dismissed() {
		t.Fatalf("B (operator) dismissed the team task")
	}
	// The board groups as B sees it: B's own tasks, then the team's, the
	// team's name shown plain.
	board := tw.post(t, w.b.uid, "GET", "/ui/tasks", "").Body.String()
	plain := "Platform team " + w.suffix
	if !strings.Contains(board, ">"+plain+"</h2>") || strings.Contains(board, "**Platform**") {
		t.Errorf("the board's team group heading is not the team's plain name %q", plain)
	}

	// --- A, the manager (CONTROL): the dismiss B was refused ---
	if rec := tw.post(t, w.a.uid, "DELETE", "/tasks/"+id, ""); !is2xx(rec.Code) || !dismissed() {
		t.Errorf("control: A (manager) dismissing the team task: %d, dismissed %v", rec.Code, dismissed())
	}
}

// TestMovingATaskIntoATeamOverHTTP: A moves A's own task (with a comment)
// into the team through POST /tasks/{id}/move, and afterwards the task and
// its comment are the team's and the viewer reads them. Refused, moving
// nothing: V moving V's own task into a team V only views (403), and B moving
// A's task (B cannot even see it: 404).
func TestMovingATaskIntoATeamOverHTTP(t *testing.T) {
	tw := newTeamTaskWalk(t)
	w := tw.w
	team := url.Values{"team": {strconv.FormatInt(tw.team, 10)}}.Encode()
	var mine int64
	if err := w.pool.QueryRow(w.sys, `INSERT INTO notes (owner_id, title, body) VALUES ($1, 'mine', $2) RETURNING id`, w.a.uid, "own "+tw.canary).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(w.sys, `INSERT INTO note_comments (note_id, owner_id, author, body) VALUES ($1, $2, 'user', 'c')`, mine, w.a.uid); err != nil {
		t.Fatal(err)
	}
	ids := strconv.FormatInt(mine, 10)
	if rec := tw.post(t, w.b.uid, "POST", "/tasks/"+ids+"/move", team); is2xx(rec.Code) {
		t.Errorf("B moving A's task: %d; want refused", rec.Code)
	}
	var vOwn int64
	if err := w.pool.QueryRow(w.sys, `INSERT INTO notes (owner_id, title) VALUES ($1, 'v own') RETURNING id`, tw.v.uid).Scan(&vOwn); err != nil {
		t.Fatal(err)
	}
	if rec := tw.post(t, tw.v.uid, "POST", "/tasks/"+strconv.FormatInt(vOwn, 10)+"/move", team); rec.Code != http.StatusForbidden {
		t.Errorf("V moving V's own task into a team V only views: %d; want 403", rec.Code)
	}
	rec := tw.post(t, w.a.uid, "POST", "/tasks/"+ids+"/move", team)
	if !is2xx(rec.Code) {
		t.Fatalf("A moving A's own task into A's team: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	var noteOwner, commentOwner, vOwner int64
	_ = w.pool.QueryRow(w.sys, `SELECT owner_id FROM notes WHERE id = $1`, mine).Scan(&noteOwner)
	_ = w.pool.QueryRow(w.sys, `SELECT owner_id FROM note_comments WHERE note_id = $1`, mine).Scan(&commentOwner)
	_ = w.pool.QueryRow(w.sys, `SELECT owner_id FROM notes WHERE id = $1`, vOwn).Scan(&vOwner)
	if noteOwner != tw.team || commentOwner != tw.team {
		t.Errorf("after the move the task is owned by %d and its comment by %d; want the team %d for both", noteOwner, commentOwner, tw.team)
	}
	if vOwner != tw.v.uid {
		t.Errorf("V's refused move left V's task owned by %d", vOwner)
	}
	if page := tw.post(t, tw.v.uid, "GET", "/tasks/"+ids, ""); !strings.Contains(page.Body.String(), "own "+tw.canary) {
		t.Errorf("V (viewer) cannot read the task moved into the team: %d", page.Code)
	}
}
