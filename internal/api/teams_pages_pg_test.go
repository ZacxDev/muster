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

// TestOnlyATeamsManagerReachesItsPage (phase 2a): /teams/{id} and its writes
// answer the team's MANAGER — and everyone else (an operator of the team, a
// non-member, an admin who is not a member, the machine hook token) the same
// 404 a team that does not exist gets. A cross-origin write from the manager's
// own session is refused (the CSRF rule). The manager's add, re-role and
// removal each write an audit row owned by the team naming the manager, and
// the last manager cannot demote or remove themselves. CONTROLS: the manager's
// GET shows the members (so the others' empty answers are about who asks),
// and an admin's /admin/teams still can.
func TestOnlyATeamsManagerReachesItsPage(t *testing.T) {
	w := newIsoWorld(t)
	sys := authz.System(context.Background())
	store := users.NewPG(w.pool)
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	team, err := store.CreateTeam(sys, admin, "pages-team-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(sys, `DELETE FROM team_members WHERE team_id = $1`, team.ID)
		_, _ = w.pool.Exec(sys, `DELETE FROM audit_log WHERE team_id = $1`, team.ID)
		_, _ = w.pool.Exec(sys, `DELETE FROM users WHERE id = $1`, team.ID)
	})
	if err := store.SetTeamMember(sys, admin, team.ID, w.b.uid, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(team.ID, 10)
	page := "/teams/" + id

	// The manager's own view (CONTROL).
	if rec := w.do(w.h, "GET", page, "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `data-member-id="`+strconv.FormatInt(w.b.uid, 10)+`"`) {
		t.Fatalf("control: the manager's page: %d, members not shown\n%.400s", rec.Code, rec.Body.String())
	}
	// Everyone else: the same 404 as a team that does not exist.
	ghost := w.do(w.h, "GET", "/teams/9000000000", "", cred{sessionUser: w.a.uid})
	for who, c := range map[string]cred{
		"an operator of the team":         {sessionUser: w.b.uid},
		"the admin (not a member)":        {sessionUser: w.op.uid},
		"the machine hook token":          {hook: true},
		"the manager's own agent's token": {agentToken: w.a.token},
	} {
		for _, req := range []struct{ method, path, body string }{
			{"GET", page, ""},
			{"POST", page + "/members", url.Values{"user": {strconv.FormatInt(w.b.uid, 10)}, "role": {"manager"}}.Encode()},
			{"POST", page + "/members/" + strconv.FormatInt(w.b.uid, 10) + "/remove", ""},
		} {
			rec := w.do(w.h, req.method, req.path, req.body, c)
			if req.method == "GET" && c.sessionUser > 0 && rec.Code != ghost.Code {
				t.Errorf("%s %s %s: %d, but a team that does not exist answers %d", who, req.method, req.path, rec.Code, ghost.Code)
			}
			if rec.Code < 400 {
				t.Errorf("%s %s %s: %d; want a refusal", who, req.method, req.path, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "pages-team-") {
				t.Errorf("%s %s %s names the team", who, req.method, req.path)
			}
		}
	}
	if r, _ := membership(t, w, team.ID, w.b.uid); r != "operator" {
		t.Fatalf("the refused writes changed B's membership to %q", r)
	}

	// A cross-origin write from the manager's own session: refused.
	req := url.Values{"user": {strconv.FormatInt(w.b.uid, 10)}, "role": {"viewer"}}.Encode()
	if rec := w.doOrigin(w.h, "POST", page+"/members", req, w.a.uid, "https://elsewhere.example"); rec.Code != http.StatusForbidden {
		t.Errorf("a cross-origin write from the manager's session: %d; want 403", rec.Code)
	}
	if r, _ := membership(t, w, team.ID, w.b.uid); r != "operator" {
		t.Fatalf("the cross-origin write changed B's membership to %q", r)
	}

	// The manager's writes, each audited as the manager and owned by the team.
	if rec := w.do(w.h, "POST", page+"/members", req, cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the manager re-roling B: %d %.300s", rec.Code, rec.Body.String())
	}
	if r, _ := membership(t, w, team.ID, w.b.uid); r != "viewer" {
		t.Fatalf("after the manager's re-role B is %q", r)
	}
	// The last manager cannot demote or remove themselves.
	self := url.Values{"user": {strconv.FormatInt(w.a.uid, 10)}, "role": {"operator"}}.Encode()
	if rec := w.do(w.h, "POST", page+"/members", self, cred{sessionUser: w.a.uid}); rec.Code != http.StatusConflict {
		t.Errorf("the last manager demoting themselves: %d; want 409", rec.Code)
	}
	if rec := w.do(w.h, "POST", page+"/members/"+strconv.FormatInt(w.a.uid, 10)+"/remove", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusConflict {
		t.Errorf("the last manager removing themselves: %d; want 409", rec.Code)
	}
	if rec := w.do(w.h, "POST", page+"/members/"+strconv.FormatInt(w.b.uid, 10)+"/remove", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("the manager removing B: %d", rec.Code)
	}
	var rows []string
	r, err := w.pool.Query(sys, `SELECT action || ' by ' || user_id || ' ' || coalesce(detail->>'role', '') FROM audit_log
		WHERE team_id = $1 AND user_id = $2 ORDER BY id`, team.ID, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var s string
		_ = r.Scan(&s)
		rows = append(rows, s)
	}
	r.Close()
	a := strconv.FormatInt(w.a.uid, 10)
	if want := []string{"team.member.set by " + a + " viewer", "team.member.remove by " + a + " viewer"}; strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Errorf("the manager's audit rows = %v; want %v", rows, want)
	}
}

func membership(t *testing.T, w *isoWorld, team, user int64) (string, bool) {
	t.Helper()
	var role string
	err := w.pool.QueryRow(authz.System(context.Background()), `SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2`, team, user).Scan(&role)
	return role, err == nil
}

// doOrigin is w.do for user's browser session with a chosen Origin: a
// cross-site form post from that session.
func (w *isoWorld) doOrigin(h http.Handler, method, path, body string, user int64, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "muster.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Cookie", sessionCookieFor(w.srv))
	r.Header.Set("Origin", origin)
	r.Header.Set(isoUserHeader, strconv.FormatInt(user, 10))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
