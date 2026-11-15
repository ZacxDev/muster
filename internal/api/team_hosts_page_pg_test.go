package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// /teams/{id}/hosts (team sharing 2b.2) over a real Postgres.

// teamHostsReq runs one of the page's handlers for user's browser session,
// signed in fresh (within the step-up window) or not.
func teamHostsReq(w *isoWorld, h http.HandlerFunc, method, path string, form url.Values, user int64, fresh bool, pathVals map[string]string) *httptest.ResponseRecorder {
	ctx := authz.WithPrincipal(context.Background(), w.srv.principalFor(context.Background(), user, authz.ViaSession))
	if fresh {
		ctx = stepup.WithAuthTime(ctx, time.Now())
	} else {
		ctx = stepup.WithAuthTime(ctx, time.Now().Add(-2*stepup.Window()))
	}
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode())).WithContext(ctx)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	for k, v := range pathVals {
		r.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// TestTheTeamHostsPageIsTheManagersAndRunsAsTheTeam: only the team's
// MANAGER reaches the page (an operator gets the 404 a missing team gets);
// minting needs a fresh sign-in (a stale one is refused with the step-up
// header, and mints nothing); a fresh mint is the TEAM's token, shown once
// and audited as the manager; when the operator then leaves, the token is
// marked rotate and the page names him, read from the team's audit row; the
// revoke needs a fresh sign-in, revokes the team's token and is audited; and
// a replace with nothing held is refused.
func TestTheTeamHostsPageIsTheManagersAndRunsAsTheTeam(t *testing.T) {
	w := newIsoWorld(t)
	sys := authz.System(context.Background())
	store := users.NewPG(w.pool)
	admin := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	team, err := store.CreateTeam(sys, admin, "hosts-team-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM mirror_tokens WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE team_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = w.pool.Exec(sys, q, team.ID)
		}
	})
	if err := store.SetTeamMember(sys, admin, team.ID, w.b.uid, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	w.srv.ext.MirrorTokens = mirror.NewPG(w.pool)
	w.srv.ext.Mirror = mirror.NewService(mirror.NewPG(w.pool), nil)
	id := strconv.FormatInt(team.ID, 10)
	page := "/teams/" + id + "/hosts"
	ids := map[string]string{"id": id}
	tokens := func() (live, revoked int) {
		t.Helper()
		if err := w.pool.QueryRow(sys, `SELECT count(*) FILTER (WHERE revoked_at IS NULL), count(*) FILTER (WHERE revoked_at IS NOT NULL)
			FROM mirror_tokens WHERE owner_id = $1`, team.ID).Scan(&live, &revoked); err != nil {
			t.Fatal(err)
		}
		return live, revoked
	}
	audits := func(action string) int {
		t.Helper()
		var n int
		if err := w.pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE team_id = $1 AND action = $2 AND user_id = $3`, team.ID, action, w.a.uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if rec := teamHostsReq(w, w.srv.handleTeamHostsPage, "GET", page, nil, w.b.uid, true, ids); rec.Code != http.StatusNotFound {
		t.Fatalf("an operator's GET: %d; want the 404 a missing team gets", rec.Code)
	}
	if rec := teamHostsReq(w, w.srv.handleTeamHostMint, "POST", page, url.Values{"host": {"build-01"}}, w.b.uid, true, ids); rec.Code != http.StatusNotFound {
		t.Fatalf("an operator's fresh mint: %d; want 404", rec.Code)
	}
	if rec := teamHostsReq(w, w.srv.handleTeamHostsPage, "GET", page, nil, w.a.uid, false, ids); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `id="team-host-tokens-none"`) {
		t.Fatalf("CONTROL: the manager's GET: %d\n%.400s", rec.Code, rec.Body.String())
	}
	// A stale sign-in mints nothing.
	if rec := teamHostsReq(w, w.srv.handleTeamHostMint, "POST", page, url.Values{"host": {"build-01"}}, w.a.uid, false, ids); rec.Code != http.StatusForbidden ||
		rec.Header().Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a stale mint: %d (step-up %q); want 403 asking for a fresh sign-in", rec.Code, rec.Header().Get("X-Muster-Step-Up"))
	}
	if live, _ := tokens(); live != 0 {
		t.Fatalf("a stale mint minted %d token(s)", live)
	}
	// A fresh mint: the team's token, shown once.
	rec := teamHostsReq(w, w.srv.handleTeamHostMint, "POST", page, url.Values{"host": {"build-01"}}, w.a.uid, true, ids)
	shown := regexp.MustCompile(`id="team-host-token">(mmt_[A-Za-z0-9_-]{43})<`).FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || shown == nil {
		t.Fatalf("a fresh mint: %d, token shown %v\n%.600s", rec.Code, shown != nil, rec.Body.String())
	}
	if live, _ := tokens(); live != 1 {
		t.Fatalf("a fresh mint left %d live team token(s); want 1", live)
	}
	got, err := mirror.NewPG(w.pool).Authenticate(sys, shown[1])
	if err != nil || got.OwnerID != team.ID || got.Host != "build-01" {
		t.Fatalf("the shown token authenticates as %+v, %v; want team %d's build-01", got, err, team.ID)
	}
	if n := audits("team.host.token.create"); n != 1 {
		t.Fatalf("%d team.host.token.create rows by the manager; want 1", n)
	}
	if strings.Contains(teamHostsReq(w, w.srv.handleTeamHostsPage, "GET", page, nil, w.a.uid, false, ids).Body.String(), shown[1]) {
		t.Fatal("the token is shown again on a later GET")
	}
	// The operator leaves: the token is marked, and the page names him.
	if err := store.RemoveTeamMemberAsManager(sys, users.Actor{UserID: w.a.uid, Via: authz.ViaSession}, team.ID, w.b.uid); err != nil {
		t.Fatal(err)
	}
	var benName string
	if err := w.pool.QueryRow(sys, `SELECT display_name FROM users WHERE id = $1`, w.b.uid).Scan(&benName); err != nil {
		t.Fatal(err)
	}
	body := teamHostsReq(w, w.srv.handleTeamHostsPage, "GET", page, nil, w.a.uid, false, ids).Body.String()
	if !strings.Contains(body, `data-rotate="true"`) || !strings.Contains(body, "rotate: "+benName+" left on ") {
		t.Fatalf("after the operator left, the page does not name the rotate reason (want %q):\n%.900s", "rotate: "+benName+" left on", body)
	}
	// Revoke: stale refused, fresh revokes.
	tid := strconv.FormatInt(got.ID, 10)
	revokeVals := map[string]string{"id": id, "token": tid}
	if rec := teamHostsReq(w, w.srv.handleTeamHostRevoke, "POST", page+"/"+tid+"/revoke", nil, w.a.uid, false, revokeVals); rec.Code != http.StatusForbidden {
		t.Fatalf("a stale revoke: %d; want 403", rec.Code)
	}
	if live, _ := tokens(); live != 1 {
		t.Fatal("a stale revoke revoked the token")
	}
	if rec := teamHostsReq(w, w.srv.handleTeamHostRevoke, "POST", page+"/"+tid+"/revoke", nil, w.a.uid, true, revokeVals); rec.Code != http.StatusSeeOther {
		t.Fatalf("a fresh revoke: %d\n%.400s", rec.Code, rec.Body.String())
	}
	if live, revoked := tokens(); live != 0 || revoked != 1 {
		t.Fatalf("after the revoke: %d live, %d revoked; want 0 and 1", live, revoked)
	}
	if n := audits("team.host.token.revoke"); n != 1 {
		t.Fatalf("%d team.host.token.revoke rows by the manager; want 1", n)
	}
	// Nothing held: the replace is refused.
	if rec := teamHostsReq(w, w.srv.handleTeamHostReplace, "POST", page+"/replace", url.Values{"host": {"build-01"}, "addr": {"203.0.113.7"}}, w.a.uid, true, ids); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "No connection from that address is waiting") {
		t.Fatalf("a replace with nothing held: %d", rec.Code)
	}
}

// heldPageHub is the real mirror service with two HELD attempts on its card
// for "build-01", recording each confirm it is asked for.
type heldPageHub struct {
	*mirror.Service
	held      []mirror.HeldReplacement
	confirmed []string
}

func (h *heldPageHub) HeldReplacements(context.Context) []mirror.HeldReplacement { return h.held }

func (h *heldPageHub) ConfirmReplacement(_ context.Context, host, addr string) bool {
	h.confirmed = append(h.confirmed, host+" "+addr)
	for _, x := range h.held {
		if x.Host == host && x.Addr == addr {
			return true
		}
	}
	return false
}

// TestTheReplaceConfirmNamesOneHeldMachine (#685 round 0): with TWO machines
// held for one team host, the card lists each by its address, with its own
// "Replace with this machine" form carrying that address — and no form
// without one. A confirm that names no address is refused (409) and asks the
// hub nothing; a confirm of one address confirms exactly that address.
func TestTheReplaceConfirmNamesOneHeldMachine(t *testing.T) {
	w := newIsoWorld(t)
	sys := authz.System(context.Background())
	store := users.NewPG(w.pool)
	team, err := store.CreateTeam(sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "held-team-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM team_members WHERE team_id = $1`, `DELETE FROM audit_log WHERE team_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = w.pool.Exec(sys, q, team.ID)
		}
	})
	now := time.Now()
	hub := &heldPageHub{Service: mirror.NewService(mirror.NewPG(w.pool), nil), held: []mirror.HeldReplacement{
		{Host: "build-01", Addr: "192.0.2.99", First: now.Add(-time.Minute), Last: now, Attempts: 30},
		{Host: "build-01", Addr: "203.0.113.7", First: now.Add(-time.Minute), Last: now, Attempts: 2},
	}}
	w.srv.ext.MirrorTokens = mirror.NewPG(w.pool)
	w.srv.ext.Mirror = hub
	id := strconv.FormatInt(team.ID, 10)
	page := "/teams/" + id + "/hosts"
	ids := map[string]string{"id": id}
	body := teamHostsReq(w, w.srv.handleTeamHostsPage, "GET", page, nil, w.a.uid, false, ids).Body.String()
	for _, a := range []string{"192.0.2.99", "203.0.113.7"} {
		if !strings.Contains(body, `data-held-addr="`+a+`"`) || !strings.Contains(body, `name="addr" value="`+a+`"`) {
			t.Fatalf("the card does not list held machine %s with its own form:\n%.1500s", a, body)
		}
	}
	if n, forms := strings.Count(body, `name="addr"`), strings.Count(body, `action="/teams/`+id+`/hosts/replace"`); n != 2 || forms != 2 {
		t.Fatalf("the card has %d replace form(s) and %d address field(s); want one form per held machine, each naming its address", forms, n)
	}
	if rec := teamHostsReq(w, w.srv.handleTeamHostReplace, "POST", page+"/replace", url.Values{"host": {"build-01"}}, w.a.uid, true, ids); rec.Code != http.StatusConflict {
		t.Fatalf("a confirm naming no machine: %d; want 409", rec.Code)
	}
	if len(hub.confirmed) != 0 {
		t.Fatalf("a confirm naming no machine asked the hub to confirm %v", hub.confirmed)
	}
	if rec := teamHostsReq(w, w.srv.handleTeamHostReplace, "POST", page+"/replace", url.Values{"host": {"build-01"}, "addr": {"203.0.113.7"}}, w.a.uid, true, ids); rec.Code != http.StatusSeeOther {
		t.Fatalf("a confirm of one machine: %d", rec.Code)
	}
	if len(hub.confirmed) != 1 || hub.confirmed[0] != "build-01 203.0.113.7" {
		t.Fatalf("the hub was asked to confirm %v; want exactly build-01 203.0.113.7", hub.confirmed)
	}
}
