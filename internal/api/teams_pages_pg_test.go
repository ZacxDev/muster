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

// TestTheManagerPageAddsNoOneByIDAndItsEmailFormRevealsNothing (#657 round 1
// 🔴): the member-row form's `user` id may only RE-ROLE an existing member.
// Naming a person who is NOT a member is refused EXACTLY as naming an id no
// one has — same status, same body — with no membership row, no audit row and
// no trace of that person's email on the page. Adding is the email form's,
// and its answer for an email someone signs in with is the same as for one
// nobody does (the registered person is added; the other changes nothing) —
// and as for a DISABLED account's, which adds no one either.
// RED at 75139c60: the id form added the non-member (303) and showed their
// email. CONTROL: the same id form re-roles an existing member.
func TestTheManagerPageAddsNoOneByIDAndItsEmailFormRevealsNothing(t *testing.T) {
	w := newIsoWorld(t)
	sys := authz.System(context.Background())
	store := users.NewPG(w.pool)
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	team, err := store.CreateTeam(sys, admin, "consent-team-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTeamMember(sys, admin, team.ID, w.b.uid, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	outsiderEmail := "outsider-" + w.suffix + "@iso.test"
	var outsider int64
	if err := w.pool.QueryRow(sys, `INSERT INTO users (role, email, display_name) VALUES ('member', $1, 'outsider') RETURNING id`, outsiderEmail).Scan(&outsider); err != nil {
		t.Fatal(err)
	}
	disabledEmail := "disabled-" + w.suffix + "@iso.test"
	var disabled int64
	if err := w.pool.QueryRow(sys, `INSERT INTO users (role, email, display_name, disabled_at) VALUES ('member', $1, 'disabled', now()) RETURNING id`,
		disabledEmail).Scan(&disabled); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(sys, `DELETE FROM team_members WHERE team_id = $1`, team.ID)
		_, _ = w.pool.Exec(sys, `DELETE FROM audit_log WHERE team_id = $1`, team.ID)
		_, _ = w.pool.Exec(sys, `DELETE FROM users WHERE id IN ($1, $2, $3)`, team.ID, outsider, disabled)
	})
	page := "/teams/" + strconv.FormatInt(team.ID, 10)
	manager := cred{sessionUser: w.a.uid}
	audits := func() int {
		var n int
		_ = w.pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE team_id = $1`, team.ID).Scan(&n)
		return n
	}
	before := audits()

	byID := func(id int64) *httptest.ResponseRecorder {
		return w.do(w.h, "POST", page+"/members", url.Values{"user": {strconv.FormatInt(id, 10)}, "role": {"operator"}}.Encode(), manager)
	}
	nonMember, nobody := byID(outsider), byID(9_000_000_001)
	if nonMember.Code == http.StatusSeeOther || nonMember.Code != nobody.Code || nonMember.Body.String() != nobody.Body.String() {
		t.Errorf("the id form naming a NON-MEMBER answered %d, naming NOBODY %d (bodies equal: %v); want one refusal for both",
			nonMember.Code, nobody.Code, nonMember.Body.String() == nobody.Body.String())
	}
	if r, in := membership(t, w, team.ID, outsider); in {
		t.Errorf("the id form ADDED the non-member (as %s)", r)
	}
	if audits() != before {
		t.Errorf("the refused id form wrote %d audit row(s)", audits()-before)
	}
	if page := w.do(w.h, "GET", page, "", manager); strings.Contains(page.Body.String(), outsiderEmail) {
		t.Error("the manager's page shows the non-member's email")
	}
	// CONTROL: the id form re-roles an existing member.
	if rec := byID(w.b.uid); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: re-roling a member by id: %d", rec.Code)
	} else if r, _ := membership(t, w, team.ID, w.b.uid); r != "operator" {
		t.Fatalf("control: after the re-role the member is %q", r)
	}

	// The email form: the same answer for a registered and an unregistered email.
	byEmail := func(email string) *httptest.ResponseRecorder {
		return w.do(w.h, "POST", page+"/members", url.Values{"email": {email}, "role": {"viewer"}}.Encode(), manager)
	}
	// A DISABLED account's email (#657 round 2): answered as an unregistered
	// one, with no row and no audit entry.
	auditsBefore := audits()
	disabledRec, nobodyRec := byEmail(disabledEmail), byEmail("no-such-"+w.suffix+"@iso.test")
	if disabledRec.Code != nobodyRec.Code || disabledRec.Header().Get("Location") != nobodyRec.Header().Get("Location") ||
		disabledRec.Body.String() != nobodyRec.Body.String() {
		t.Errorf("the email form answers a DISABLED account %d %q and an unregistered email %d %q; want the same answer",
			disabledRec.Code, disabledRec.Header().Get("Location"), nobodyRec.Code, nobodyRec.Header().Get("Location"))
	}
	if r, in := membership(t, w, team.ID, disabled); in {
		t.Errorf("the email form ADDED the disabled account (as %s)", r)
	}
	if audits() != auditsBefore {
		t.Errorf("the disabled account's email wrote %d audit row(s)", audits()-auditsBefore)
	}
	registered, unregistered := byEmail(outsiderEmail), byEmail("no-such-"+w.suffix+"@iso.test")
	if registered.Code != unregistered.Code || registered.Header().Get("Location") != unregistered.Header().Get("Location") ||
		registered.Body.String() != unregistered.Body.String() {
		t.Errorf("the email form answers a registered email %d %q and an unregistered one %d %q; want the same answer",
			registered.Code, registered.Header().Get("Location"), unregistered.Code, unregistered.Header().Get("Location"))
	}
	if r, in := membership(t, w, team.ID, outsider); !in || r != "viewer" {
		t.Errorf("the email form did not add the registered person (in=%v role=%q)", in, r)
	}
}
