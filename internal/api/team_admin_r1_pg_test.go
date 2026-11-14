package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
)

// Slice 4.5's audit round 0/1 fixes (team_admin.go): the team chief page's
// restart takes the agent lifecycle's refusal before it stops anything, a
// team's chief starts no agent yet, a manager's audit row is written once the
// outcome is known, and the agent page offers Rotate credentials to those the
// route lets rotate.

// TestARefusedTeamChiefRestartLeavesTheChiefRunning: the team T is NOT on the
// server pool (dropped from MUSTER_TEAMS_ON_SERVER_POOL), so its chief could
// not start again. A's restart from /teams/{id}/chief is refused with the
// pool's reason — 409, before anything is stopped (and, listed but with the
// claude-code kind off, with the kind's reason, equally unstopped): the provisioner is never
// reached (no Stop), and no chief.restart row is written. The page then
// offers neither Start nor Restart and says why, while Turn off stays. The
// CONTROL is the same restart with T listed: a Stop and a Start as the team,
// and one chief.restart row whose outcome is "succeeded".
//
// 🔴 RED at 23eb2f32: the restart answered 303, stopped the chief (and
// called Start, which a real provisioner refuses for want of an account) and
// wrote a chief.restart row.
func TestARefusedTeamChiefRestartLeavesTheChiefRunning(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	plane := poolPlane(t, w)
	plane.TeamsOnServerPool = nil
	t.Cleanup(func() { plane.TeamsOnServerPool = nil })
	base := "/teams/" + strconv.FormatInt(tw.team, 10) + "/chief"

	rec := w.do(w.h, "POST", base+"/restart", "", cred{sessionUser: w.a.uid})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), htmlText(TeamChiefPoolRefusal)) {
		t.Fatalf("A restarting a delisted team's chief: %d %q; want 409 with the pool refusal", rec.Code, truncate(rec.Body.String(), 300))
	}
	if n := w.prov.actedOn(tw.chief.ID, tw.team); n != 0 {
		t.Fatalf("the refused restart reached the provisioner %d time(s) (a Stop of a chief that could not start again)", n)
	}
	if n := tw.teamAudits(t, "chief.restart", w.a.uid); n != 0 {
		t.Errorf("audit: a refused restart wrote %d chief.restart row(s); want none", n)
	}
	page := w.do(w.h, "GET", base, "", cred{sessionUser: w.a.uid}).Body.String()
	if strings.Contains(page, base+"/restart") || strings.Contains(page, base+"/provision") ||
		!strings.Contains(page, `id="team-chief-refusal"`) || !strings.Contains(page, base+"/stop") {
		t.Errorf("the delisted team's chief page: Restart offered %v, Start offered %v, reason shown %v, Turn off offered %v; "+
			"want neither Start nor Restart, the reason, and Turn off",
			strings.Contains(page, base+"/restart"), strings.Contains(page, base+"/provision"),
			strings.Contains(page, `id="team-chief-refusal"`), strings.Contains(page, base+"/stop"))
	}

	// Listed, but the deployment has the claude-code kind off: refused too,
	// with the kind's reason, and nothing stopped.
	plane.TeamsOnServerPool = map[int64]bool{tw.team: true}
	kinds := w.srv.ext.Kinds
	w.srv.ext.Kinds = claudeOff{kinds.(isoKinds)}
	rec = w.do(w.h, "POST", base+"/restart", "", cred{sessionUser: w.a.uid})
	w.srv.ext.Kinds = kinds
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), htmlText(TeamChiefKindRefusal)) {
		t.Fatalf("A restarting the chief with the claude-code kind off: %d %q; want 409 with the kind refusal", rec.Code, truncate(rec.Body.String(), 300))
	}
	if n := w.prov.actedOn(tw.chief.ID, tw.team); n != 0 {
		t.Fatalf("the kind-refused restart reached the provisioner %d time(s)", n)
	}

	if rec := w.do(w.h, "POST", base+"/restart", "", cred{sessionUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: A restarting a listed team's chief: %d %q", rec.Code, truncate(rec.Body.String(), 300))
	}
	if n := w.prov.actedOn(tw.chief.ID, tw.team); n != 2 {
		t.Errorf("control: the restart acted as the team %d time(s); want 2 (a Stop and a Start)", n)
	}
	if n := tw.teamAuditsWhere(t, "chief.restart", w.a.uid, "succeeded"); n != 1 {
		t.Errorf("control: %d succeeded chief.restart row(s) by A; want 1", n)
	}
}

// TestATeamsChiefCannotDispatchYet: agent_dispatch from a TEAM's chief is
// refused whatever it asks for — a plain pod/claude-code start included —
// saying team dispatch arrives in a later release, and nothing is created.
// The CONTROL is a person's chief with the same arguments: not refused so.
//
// 🔴 RED at 23eb2f32: a team chief whose team was listed in
// MUSTER_CHIEF_DISPATCH_OWNERS (and on the server pool) went on into the
// dispatch flow as the team (an approval ask to the team, then the start).
func TestATeamsChiefCannotDispatchYet(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	w.srv.ext.TaskRouting = &TaskRouting{DispatchOwners: map[int64]bool{tw.team: true, w.a.uid: true}}
	w.srv.ext.ChiefDispatch = plugins.NewPG(w.pool)
	// The team is on the server pool, so nothing ELSE refuses the start.
	plane := poolPlane(t, w)
	plane.TeamsOnServerPool = map[int64]bool{tw.team: true}
	t.Cleanup(func() { plane.TeamsOnServerPool = nil })
	count := func() int {
		var n int
		if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM agents WHERE owner_id = $1`, tw.team).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	in := agentDispatchIn{Target: "pod/" + agents.KindClaudeCode, Prompt: "write the release notes", RequestKey: "team-later-1", OwnerAsked: true}
	const later = "team dispatch arrives in a later release"
	if got := w.srv.toolAgentDispatch(tw.teamCtx, tw.chief, in); !strings.Contains(got, later) {
		t.Errorf("a team chief's pod/claude-code dispatch: %s; want it refused (%q)", truncate(got, 300), later)
	}
	if n := count(); n != before {
		t.Errorf("the refused dispatch created %d team agent(s)", n-before)
	}
	personCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: w.a.uid, Role: authz.RoleMember, Via: authz.ViaAgent})
	personChief := agents.Agent{ID: tw.chief.ID + 1000000, Name: agents.ChiefNameFor(w.a.uid), OwnerID: w.a.uid, Kind: agents.KindClaudeCode}
	in.RequestKey = "team-later-2"
	if got := w.srv.toolAgentDispatch(personCtx, personChief, in); strings.Contains(got, later) {
		t.Errorf("control: a person's chief got the team refusal: %s", truncate(got, 300))
	}
}

// failingUpdate is a driver whose re-apply (Update) fails.
type failingUpdate struct{ *provision.Noop }

func (failingUpdate) Update(context.Context, provision.Spec) error {
	return errors.New("injected: the instance re-apply failed")
}

// TestARotationWhoseReapplyFailsIsStillAudited: A rotates a RUNNING team
// agent's credentials through the real adapter and store, and the re-apply of
// its instance fails. The new credentials are committed (the old token no
// longer identifies the agent), the answer says the re-apply failed, and the
// team's audit log holds ONE agent.credentials.rotate row by A whose outcome is
// "failed" and which records rotated true, reapplied false.
//
// 🔴 RED at 23eb2f32: the row was written only after a fully successful
// rotation, so a rotation that changed the credentials and then failed its
// re-apply left no audit row at all.
func TestARotationWhoseReapplyFailsIsStillAudited(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	store := agents.NewPG(w.pool)
	adapter, err := agentprovision.New(agentprovision.Config{Driver: failingUpdate{provision.MustNewNoop()}, Store: store,
		Spec:               agentspec.Config{ImageRepo: "registry.example.test/muster/agent-runtime", APIBaseURL: "http://muster.example.test:8105"},
		KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	w.srv.ext.Provisioner = adapter
	worker := tw.teamWorker(t, "trfail")
	if _, err := w.pool.Exec(w.sys, `UPDATE agents SET status = 'running' WHERE id = $1`, worker.ID); err != nil {
		t.Fatal(err)
	}
	oldTok := "old-hooks-trfail-" + w.suffix
	rec := w.do(w.h, "POST", "/agents/"+strconv.FormatInt(worker.ID, 10)+"/credentials/rotate", "", cred{sessionUser: w.a.uid})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("A rotating, the re-apply failing: %d %q; want 500", rec.Code, truncate(rec.Body.String(), 300))
	}
	if got, err := store.GetByHooksToken(w.sys, oldTok); err == nil {
		t.Errorf("the OLD hooks token still identifies agent %d: the credentials were not committed", got.ID)
	}
	var n int
	if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM audit_log WHERE team_id = $1 AND action = 'agent.credentials.rotate'
		AND user_id = $2 AND detail->>'outcome' = 'failed' AND detail->>'rotated' = 'true' AND detail->>'reapplied' = 'false'`,
		tw.team, w.a.uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit: %d rotate row(s) by A recording the committed rotation and its failed re-apply; want 1 (rows of the action: %d)",
			n, tw.teamAudits(t, "agent.credentials.rotate", w.a.uid))
	}
}

// docPost is POST path as a browser FORM submit (a document navigation, no
// htmx) by user uid's session.
func docPost(w *isoWorld, path string, uid int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader("")).WithContext(context.Background())
	r.Host = "muster.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Cookie", sessionCookieFor(w.srv))
	r.Header.Set("Origin", "http://muster.test")
	r.Header.Set(isoUserHeader, strconv.FormatInt(uid, 10))
	r.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

// TestTheAgentPageOffersRotateCredentialsToThoseWhoMayRotate: a team agent's
// page offers "Rotate credentials" — a form to the rotate route marked for the
// step-up, with the "Confirm it's you" dialog on the page — to A, a manager of
// the team, and to nobody below: B (operator) and V (viewer) see no such form
// and no dialog. A person's own agent offers it to its owner. A's form submit
// answers a page saying the old token no longer works, and neither the page
// nor the answer ever carries the agent's token. The team page links the
// team's chief page.
//
// 🔴 RED at 23eb2f32: there was no rotation control (round 0's gap).
func TestTheAgentPageOffersRotateCredentialsToThoseWhoMayRotate(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	worker := tw.teamWorker(t, "tpage")
	token := "old-hooks-tpage-" + w.suffix
	rotate := "/agents/" + strconv.FormatInt(worker.ID, 10) + "/credentials/rotate"
	page := func(uid int64) string {
		t.Helper()
		rec := w.do(w.h, "GET", "/agents/"+worker.Name, "", cred{sessionUser: uid})
		if rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
			t.Fatalf("user %d opening the team agent's page: %d", uid, rec.Code)
		}
		if loc := rec.Header().Get("Location"); rec.Code == http.StatusSeeOther {
			rec = w.do(w.h, "GET", loc, "", cred{sessionUser: uid})
		}
		return rec.Body.String()
	}
	offered := func(body string) bool {
		return strings.Contains(body, `action="`+rotate+`" data-step-up-form`) && strings.Contains(body, `id="term-confirm"`)
	}
	if body := page(w.a.uid); !offered(body) || strings.Contains(body, token) {
		t.Errorf("A (manager) on the team agent's page: offered %v, token drawn %v; want offered, never the token", offered(body), strings.Contains(body, token))
	}
	for _, who := range []isoUser{w.b, tw.v} {
		if body := page(who.uid); strings.Contains(body, rotate) || strings.Contains(body, `id="term-confirm"`) {
			t.Errorf("%s on the team agent's page is offered Rotate credentials", who.name)
		}
	}
	mine, err := agents.NewPG(w.pool).Create(asUser(w.b.uid), agents.Agent{Name: "brot-" + w.suffix, Namespace: "ns-brot-" + w.suffix,
		Status: agents.StatusStopped, Kind: agents.KindClaudeCode, CCAccount: isoEnvAccount, HooksToken: "b-own-" + w.suffix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.pool.Exec(w.sys, `DELETE FROM agents WHERE id = $1`, mine.ID) })
	rec := w.do(w.h, "GET", "/agents/"+mine.Name, "", cred{sessionUser: w.b.uid})
	if loc := rec.Header().Get("Location"); rec.Code == http.StatusSeeOther {
		rec = w.do(w.h, "GET", loc, "", cred{sessionUser: w.b.uid})
	}
	if body := rec.Body.String(); !strings.Contains(body, `action="/agents/`+strconv.FormatInt(mine.ID, 10)+`/credentials/rotate"`) {
		t.Errorf("control: B on B's own agent's page is not offered Rotate credentials")
	}

	got := docPost(w, rotate, w.a.uid)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), htmlText(RotateDoneStopped)) || strings.Contains(got.Body.String(), token) {
		t.Errorf("A's form submit: %d, says the old token is dead %v, token drawn %v; want 200 saying so, never the token",
			got.Code, strings.Contains(got.Body.String(), htmlText(RotateDoneStopped)), strings.Contains(got.Body.String(), token))
	}

	teamPage := w.do(w.h, "GET", "/teams/"+strconv.FormatInt(tw.team, 10), "", cred{sessionUser: w.a.uid})
	if !strings.Contains(teamPage.Body.String(), `href="/teams/`+strconv.FormatInt(tw.team, 10)+`/chief"`) {
		t.Errorf("the team page (%d) does not link the team's chief page", teamPage.Code)
	}
}
