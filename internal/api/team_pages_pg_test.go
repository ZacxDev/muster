package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notifypref"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// A TEAM'S PAGES through the real server against a real Postgres (migration
// 0162; team_pages.go): a team agent's page records the person who started
// the agent; every member sees it in their Pages list marked with its team,
// opens it and answers it; only that person is offered — and allowed — the
// managing controls; any member's Share makes the link; a team agent's
// share request always asks a person. Every name is invented.
//
// The people, each a distinct id: creator (operator of A, started the agent),
// viewer (viewer of A), both (operator of A, viewer of B), onlyB (operator of
// B), outsider (no team).
type teamPageFixture struct {
	*pageFixture
	users                                  *users.PGStore
	dispatch                               *plugins.PGStore
	creator, viewer, both, onlyB, outsider int64
	teamA, teamB                           int64
	teamAgent                              agents.Agent
}

func newTeamPageFixture(t *testing.T) *teamPageFixture {
	t.Helper()
	f := &teamPageFixture{pageFixture: &pageFixture{pushes: &pagePushes{}}}
	f.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		f.pages = pages.NewPG(pool)
		if _, err := f.pages.WithLinkKey([]byte(strings.Repeat("k", pages.LinkKeySize))); err != nil {
			t.Fatal(err)
		}
		f.users = users.NewPG(pool)
		f.dispatch = plugins.NewPG(pool)
		ext.Users = f.users
		ext.ChiefDispatch = f.dispatch
		ext.NotifyPrefs = notifypref.NewPG(pool)
		ext.Pages = f.pages
		ext.Attention = attention.NewPG(pool)
		ext.PublicURL = pagesPublicURL
		ext.Push = f.pushes
	})
	f.creator, f.viewer, f.both, f.onlyB, f.outsider = f.member(t, "creator"), f.member(t, "viewer"), f.member(t, "both"), f.member(t, "onlyb"), f.member(t, "outsider")
	sys := authz.System(context.Background())
	by := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	stamp := strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
	a, err := f.users.CreateTeam(sys, by, "Alpha "+stamp, f.creator)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.users.CreateTeam(sys, by, "Beta "+stamp, f.onlyB)
	if err != nil {
		t.Fatal(err)
	}
	f.teamA, f.teamB = a.ID, b.ID
	t.Cleanup(func() {
		for _, team := range []int64{f.teamA, f.teamB} {
			for _, q := range []string{`DELETE FROM pages WHERE owner_id = $1`, `DELETE FROM attention_asks WHERE owner_id = $1`,
				`DELETE FROM notify_preferences WHERE owner_id = $1`, `DELETE FROM agents WHERE owner_id = $1`,
				`DELETE FROM team_members WHERE team_id = $1`, `DELETE FROM audit_log WHERE team_id = $1 OR user_id = $1`, `DELETE FROM users WHERE id = $1`} {
				_, _ = f.pool.Exec(sys, q, team)
			}
		}
	})
	for _, m := range []struct {
		team, user int64
		role       authz.TeamRole
	}{
		{f.teamA, f.creator, authz.TeamOperator}, {f.teamA, f.viewer, authz.TeamViewer}, {f.teamA, f.both, authz.TeamOperator},
		{f.teamB, f.both, authz.TeamViewer}, {f.teamB, f.onlyB, authz.TeamOperator},
	} {
		if err := f.users.SetTeamMember(sys, by, m.team, m.user, m.role); err != nil {
			t.Fatal(err)
		}
	}
	// Team A's agent, started on the creator's say-so (the chief's
	// reservation names them, as agent_dispatch's does).
	f.teamAgent, err = agents.NewPG(f.pool).Create(f.asTeam(f.teamA), agents.Agent{Name: "tpw-" + stamp, Namespace: "devpod-tpw-" + stamp,
		Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := plugins.NewDispatchObject()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.dispatch.ReserveChiefDispatch(f.asTeam(f.teamA), obj, map[string]any{DetailRequestedByUser: f.creator}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.dispatch.RecordChiefDispatchAgent(f.asTeam(f.teamA), obj, f.teamAgent.ID); err != nil {
		t.Fatal(err)
	}
	f.srv.sessionUserOverride = func(*http.Request) int64 { return f.session }
	return f
}

// asTeam is team's acting principal through its agent's door.
func (f *teamPageFixture) asTeam(team int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})
}

// teamTool calls a page tool as team A's agent, through its door.
func (f *teamPageFixture) teamTool(t *testing.T, name string, args any) map[string]any {
	t.Helper()
	return f.toolAs(t, f.asTeam(f.teamA), callerOfAgent(f.teamAgent), name, args)
}

// teamPage is a page team A's agent publishes, with a second version.
func (f *teamPageFixture) teamPage(t *testing.T, title string) int64 {
	t.Helper()
	out := f.teamTool(t, pageCreateTool, map[string]any{"title": title, "blocks": json.RawMessage(`[{"type":"question","id":"q1","label":"Which day?"}]`)})
	id := pnum(out["page_id"])
	if id <= 0 {
		t.Fatalf("page_create: %v", out)
	}
	if out := f.teamTool(t, pageUpdateTool, map[string]any{"page_id": id, "title": title + " v2",
		"blocks": json.RawMessage(`[{"type":"question","id":"q1","label":"Which day, really?"}]`)}); pnum(out["version"]) != 2 {
		t.Fatalf("page_update: %v", out)
	}
	return id
}

// as is uid's browser request.
func (f *teamPageFixture) req(t *testing.T, uid int64, method, path string, body string, hdr map[string]string) (int, string) {
	t.Helper()
	f.session = uid
	h := merge(f.operator(), hdr)
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	w := f.call(t, method, path, rd, h)
	return w.Code, w.Body.String()
}

var formHdr = map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

// TestATeamAgentsPageRecordsItsCreatorAndIsListedForMembers: page_create by
// team A's agent makes a TEAM page recording the person who started the
// agent; every member of A finds it in their Pages list marked "Team: <A's
// name>"; a member of B only and a member of no team find nothing, and its
// screen is their 404.
func TestATeamAgentsPageRecordsItsCreatorAndIsListedForMembers(t *testing.T) {
	f := newTeamPageFixture(t)
	id := f.teamPage(t, "Offsite plan")
	p, err := f.pages.Page(f.asTeam(f.teamA), id)
	if err != nil || p.OwnerID != f.teamA || p.Creator.ByUser != f.creator {
		t.Fatalf("the team agent's page: %+v %v; want owner %d (the team), creator %d", p, err, f.teamA, f.creator)
	}
	name := f.srv.teamNames(context.Background(), []int64{f.teamA})[f.teamA]
	mark := `data-page-team="` + strconv.FormatInt(f.teamA, 10) + `">Team: ` + name + `</span>`
	for who, uid := range map[string]int64{"creator": f.creator, "viewer": f.viewer, "member of both": f.both} {
		code, body := f.req(t, uid, http.MethodGet, "/pages", "", nil)
		if code != http.StatusOK || !strings.Contains(body, `data-page-id="`+strconv.FormatInt(id, 10)+`"`) || !strings.Contains(body, mark) {
			t.Errorf("%s's Pages list: %d; want the team page marked %q:\n%.3000s", who, code, mark, body)
		}
	}
	for who, uid := range map[string]int64{"member of B only": f.onlyB, "member of no team": f.outsider} {
		if _, body := f.req(t, uid, http.MethodGet, "/pages", "", nil); strings.Contains(body, `data-page-id="`+strconv.FormatInt(id, 10)+`"`) {
			t.Errorf("%s's Pages list shows team A's page", who)
		}
		if code, _ := f.req(t, uid, http.MethodGet, "/pages/"+strconv.FormatInt(id, 10), "", nil); code != http.StatusNotFound {
			t.Errorf("%s opening team A's page: %d; want 404", who, code)
		}
	}
}

// TestOnlyTheCreatorIsOfferedOrAllowedToManageATeamsPage: a member who is
// not the creator (a viewer; an operator in both teams) is drawn no Delete,
// no "Show this version", no Revoke and no ⋯ menu, and each of those routes
// answers them 403 with the server's own words, the page left whole. The
// creator is drawn all of them and deletes the page.
func TestOnlyTheCreatorIsOfferedOrAllowedToManageATeamsPage(t *testing.T) {
	f := newTeamPageFixture(t)
	id := f.teamPage(t, "Budget")
	sid := strconv.FormatInt(id, 10)
	l, _, err := f.pages.ShareLink(f.asTeam(f.teamA), id, "tpw", true)
	if err != nil {
		t.Fatal(err)
	}
	managing := []string{"data-page-delete", "data-version-current", "data-link-revoke", "data-page-share-menu"}
	for who, uid := range map[string]int64{"viewer": f.viewer, "operator (not the creator)": f.both} {
		code, body := f.req(t, uid, http.MethodGet, "/pages/"+sid, "", nil)
		if code != http.StatusOK || !strings.Contains(body, "data-page-share-box") {
			t.Fatalf("%s opening the team page: %d\n%.2000s", who, code, body)
		}
		for _, m := range managing {
			if strings.Contains(body, m) {
				t.Errorf("%s is drawn %s on the team's page", who, m)
			}
		}
		for _, r := range []struct{ path, body, want string }{
			{"/pages/" + sid + "/delete", "", TeamPageCreatorOnly}, {"/pages/" + sid + "/rollback", "version=1", TeamPageCreatorOnly},
			{"/pages/" + sid + "/links/" + strconv.FormatInt(l.ID, 10) + "/revoke", "", TeamPageRevokeOnly},
			{"/pages/" + sid + "/share/rotate", "", TeamPageCreatorOnly}, {"/pages/" + sid + "/share/revoke", "", TeamPageRevokeOnly},
		} {
			code, got := f.req(t, uid, http.MethodPost, r.path, r.body, formHdr)
			if code != http.StatusForbidden || strings.TrimSpace(got) != r.want {
				t.Errorf("%s POST %s: %d %q; want 403 %q", who, r.path, code, got, r.want)
			}
		}
		if p, err := f.pages.Page(f.asTeam(f.teamA), id); err != nil || p.CurrentVersion != 2 {
			t.Fatalf("after %s's attempts the page reads %+v %v; want it whole at version 2", who, p, err)
		}
		if got, err := f.pages.Link(f.asTeam(f.teamA), id, l.ID); err != nil || got.State != pages.LinkLive {
			t.Fatalf("after %s's attempts the link is %+v %v; want it live", who, got, err)
		}
	}
	// CONTROL: the creator is drawn every one, and their delete works.
	code, body := f.req(t, f.creator, http.MethodGet, "/pages/"+sid, "", nil)
	for _, m := range managing {
		if code != http.StatusOK || !strings.Contains(body, m) {
			t.Errorf("the creator is not drawn %s (%d)", m, code)
		}
	}
	if code, _ := f.req(t, f.creator, http.MethodPost, "/pages/"+sid+"/delete", "", formHdr); code != http.StatusSeeOther {
		t.Fatalf("the creator's delete: %d; want 303", code)
	}
	if _, err := f.pages.Page(f.asTeam(f.teamA), id); err == nil {
		t.Fatal("the creator's delete left the page")
	}
}

// TestAnyMemberSharesATeamsPage: a VIEWER's Share makes the page's link and
// hands its address (JSON, as the button's script asks); the link is the
// team's, made by that person by name.
func TestAnyMemberSharesATeamsPage(t *testing.T) {
	f := newTeamPageFixture(t)
	id := f.teamPage(t, "Invite")
	code, body := f.req(t, f.viewer, http.MethodPost, "/pages/"+strconv.FormatInt(id, 10)+"/share", "", map[string]string{"Accept": "application/json"})
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	if code != http.StatusOK || out["created"] != true || !strings.HasPrefix(out["url"].(string), pagesPublicURL+pages.SharePrefix) {
		t.Fatalf("a viewer's Share: %d %s", code, body)
	}
	links, err := f.pages.Links(f.asTeam(f.teamA), id)
	if err != nil || len(links) != 1 || links[0].State != pages.LinkLive || links[0].ByAgent || links[0].RequestedBy != f.srv.personName(context.Background(), f.viewer) {
		t.Fatalf("the team page's links after a viewer's Share: %+v %v", links, err)
	}
}

// TestATeamAgentAlwaysAsksEvenWithTheTeamsSettingOn: with "let my agents
// share without asking" stored TRUE on the TEAM's own row — and read true
// (the guard is what stands between it and a live link) — a team agent's
// page_share_request files a pending link and a card, never a live link.
// CONTROL: a person's own agent, its owner's setting on, goes live at once.
func TestATeamAgentAlwaysAsksEvenWithTheTeamsSettingOn(t *testing.T) {
	f := newTeamPageFixture(t)
	id := f.teamPage(t, "Press")
	if _, err := f.pool.Exec(authz.System(context.Background()), `INSERT INTO notify_preferences (owner_id, pages_agent_share) VALUES ($1, true)
		ON CONFLICT (owner_id) DO UPDATE SET pages_agent_share = true`, f.teamA); err != nil {
		t.Fatal(err)
	}
	if !f.srv.pageAgentShareOn(f.asTeam(f.teamA), f.teamA) {
		t.Fatal("instrument: the team's stored setting does not read true; the guard below would be untested")
	}
	out := f.teamTool(t, pageShareRequestTool, map[string]any{"page_id": id})
	if out["status"] != "pending" || pnum(out["ask_id"]) <= 0 {
		t.Fatalf("a team agent's share with the team's setting on: %v; want a pending link and a card", out)
	}
	if links, _ := f.pages.Links(f.asTeam(f.teamA), id); len(links) != 1 || links[0].State != pages.LinkPending {
		t.Fatalf("the team page's links: %+v; want one pending", links)
	}
	// CONTROL: a person's own agent with their setting on.
	own := f.newAgent(t, f.both, "tpo-"+strconv.FormatInt(time.Now().UnixNano()%1e9, 36))
	if err := notifypref.NewPG(f.pool).SetPagesAgentShare(f.as(f.both, authz.ViaSession), f.both, true); err != nil {
		t.Fatal(err)
	}
	pid := f.create(t, own, "Mine", `[{"type":"question","id":"q1","label":"Hi?"}]`)
	if out := f.tool(t, own, pageShareRequestTool, map[string]any{"page_id": pid}); out["status"] != "live" {
		t.Fatalf("a person's agent with their setting on: %v; want live", out)
	}
}

// TestAMemberAnswersTheTeamAgentsShareCard: a VIEWER of the team approves the
// team agent's share card from their browser, through the one answer path
// (answerAskFrom): the link goes live AND the card is recorded answered, with
// the viewer as answered_by_user — though 0127 lets a viewer write no
// attention_asks row through the ordinary path (operator decision D1). A
// member of team B only cannot reach it.
func TestAMemberAnswersTheTeamAgentsShareCard(t *testing.T) {
	f := newTeamPageFixture(t)
	id := f.teamPage(t, "Launch")
	out := f.teamTool(t, pageShareRequestTool, map[string]any{"page_id": id})
	ask := pnum(out["ask_id"])
	if ask <= 0 {
		t.Fatalf("share request: %v", out)
	}
	session := func(uid int64) context.Context {
		return authz.WithPrincipal(context.Background(), f.srv.principalFor(context.Background(), uid, authz.ViaSession))
	}
	if _, err := f.srv.answerAskFrom(session(f.onlyB), ask, pageShareApproveLabel, attention.AnswerSource{}); err == nil {
		t.Error("a member of B only answered team A's share card")
	}
	if l, _ := f.pages.LinkByAsk(f.asTeam(f.teamA), ask); l.State != pages.LinkPending {
		t.Fatalf("after B's tap the link is %s; want pending", l.State)
	}
	a, err := f.srv.answerAskFrom(session(f.viewer), ask, pageShareApproveLabel, attention.AnswerSource{})
	if err != nil || a.Status != attention.Answered {
		t.Fatalf("a viewer approving the team's share card: %+v %v; want it answered", a, err)
	}
	if l, _ := f.pages.LinkByAsk(f.asTeam(f.teamA), ask); l.State != pages.LinkLive {
		t.Fatalf("after the viewer's tap the link is %s; want live", l.State)
	}
	var status, via string
	var by *int64
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT status, coalesce(answered_via, ''), answered_by_user FROM attention_asks WHERE id = $1`, ask).
		Scan(&status, &via, &by); err != nil {
		t.Fatal(err)
	}
	if status != "answered" || via != attention.ViaBrowser || by == nil || *by != f.viewer {
		t.Fatalf("the card as recorded: status %s via %q by %v; want answered, from the browser, by the viewer %d", status, via, by, f.viewer)
	}
	// Once answered, nobody answers it again.
	if _, err := f.srv.answerAskFrom(session(f.both), ask, pageShareDenyLabel, attention.AnswerSource{}); err == nil {
		t.Error("the card was answered twice")
	}
}

// TestAManagerStepsInOnATeamsPage: while the creator is a member, a MANAGER of
// the team is drawn Revoke (a link, and ⋯ Revoke link, without New link) and
// revokes the live link a viewer made; Delete, Show this version and New link
// stay the creator's (not drawn, 403). Once the creator leaves the team the
// manager is drawn and may use every one of them; a viewer still none.
func TestAManagerStepsInOnATeamsPage(t *testing.T) {
	f := newTeamPageFixture(t)
	id := f.teamPage(t, "Retro")
	sid := strconv.FormatInt(id, 10)
	sys := authz.System(context.Background())
	by := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	if err := f.users.SetTeamMember(sys, by, f.teamA, f.outsider, authz.TeamManager); err != nil {
		t.Fatal(err)
	}
	mgr := f.outsider
	if code, body := f.req(t, f.viewer, http.MethodPost, "/pages/"+sid+"/share", "", map[string]string{"Accept": "application/json"}); code != http.StatusOK {
		t.Fatalf("a viewer's Share: %d %s", code, body)
	}
	_, screen := f.req(t, mgr, http.MethodGet, "/pages/"+sid, "", nil)
	for _, m := range []string{"data-link-revoke", "data-page-share-menu", `data-page-share-action="revoke"`} {
		if !strings.Contains(screen, m) {
			t.Errorf("a manager (the creator present) is not drawn %s", m)
		}
	}
	for _, m := range []string{"data-page-delete", "data-version-current", `data-page-share-action="rotate"`} {
		if strings.Contains(screen, m) {
			t.Errorf("a manager (the creator present) is drawn %s", m)
		}
	}
	if code, got := f.req(t, mgr, http.MethodPost, "/pages/"+sid+"/rollback", "version=1", formHdr); code != http.StatusForbidden || strings.TrimSpace(got) != TeamPageCreatorOnly {
		t.Errorf("a manager's rollback while the creator is a member: %d %q; want 403 %q", code, got, TeamPageCreatorOnly)
	}
	code, body := f.req(t, mgr, http.MethodPost, "/pages/"+sid+"/share/revoke", "", map[string]string{"Accept": "application/json"})
	if code != http.StatusOK || !strings.Contains(body, `"revoked":true`) {
		t.Fatalf("a manager revoking the viewer's link while the creator is a member: %d %s", code, body)
	}
	// The creator leaves.
	if err := f.users.RemoveTeamMember(sys, by, f.teamA, f.creator); err != nil {
		t.Fatal(err)
	}
	_, screen = f.req(t, mgr, http.MethodGet, "/pages/"+sid, "", nil)
	for _, m := range []string{"data-page-delete", "data-version-current", `data-page-share-action="rotate"`} {
		if !strings.Contains(screen, m) {
			t.Errorf("a manager, the creator gone, is not drawn %s", m)
		}
	}
	_, vscreen := f.req(t, f.viewer, http.MethodGet, "/pages/"+sid, "", nil)
	for _, m := range []string{"data-page-delete", "data-version-current", "data-page-share-menu"} {
		if strings.Contains(vscreen, m) {
			t.Errorf("a viewer, the creator gone, is drawn %s", m)
		}
	}
	if code, got := f.req(t, f.viewer, http.MethodPost, "/pages/"+sid+"/rollback", "version=1", formHdr); code != http.StatusForbidden || strings.TrimSpace(got) != TeamPageCreatorOnly {
		t.Errorf("a viewer's rollback, the creator gone: %d %q; want 403", code, got)
	}
	if code, _ := f.req(t, mgr, http.MethodPost, "/pages/"+sid+"/rollback", "version=1", formHdr); code != http.StatusSeeOther {
		t.Fatalf("a manager's rollback, the creator gone: %d; want 303", code)
	}
	if p, err := f.pages.Page(f.asTeam(f.teamA), id); err != nil || p.CurrentVersion != 1 {
		t.Fatalf("after the manager's rollback the page is %+v %v; want version 1", p, err)
	}
	if code, _ := f.req(t, mgr, http.MethodPost, "/pages/"+sid+"/delete", "", formHdr); code != http.StatusSeeOther {
		t.Fatalf("a manager's delete, the creator gone: %d; want 303", code)
	}
}

// TestAMemberRespondsFromTheOwnerViewAsThemselves: a viewer's submit from the
// team page's owner view is recorded as theirs (responder_user), and another
// member's screen says who sent it, by name; the creator's own reads "you".
// A file from the owner view is refused to members by name.
func TestAMemberRespondsFromTheOwnerViewAsThemselves(t *testing.T) {
	f := newTeamPageFixture(t)
	id := f.teamPage(t, "Lunch")
	sid := strconv.FormatInt(id, 10)
	code, body := f.req(t, f.viewer, http.MethodPost, "/pages/"+sid+"/versions/2/submit", `{"component":"q1","data":{"answer":"Friday"}}`,
		map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK {
		t.Fatalf("a viewer's submit: %d %s", code, body)
	}
	rs, err := f.pages.Responses(f.asTeam(f.teamA), id, 0, 10)
	if err != nil || len(rs) != 1 || !rs[0].ByOwner || rs[0].ResponderUser != f.viewer {
		t.Fatalf("the recorded response: %+v %v; want the viewer's (%d)", rs, err, f.viewer)
	}
	_, screen := f.req(t, f.creator, http.MethodGet, "/pages/"+sid, "", nil)
	want := `data-response-by="member" data-response-by-user="` + strconv.FormatInt(f.viewer, 10) + `"`
	if !strings.Contains(screen, want) || !strings.Contains(screen, "Sent by "+f.srv.personName(context.Background(), f.viewer)) {
		t.Errorf("the creator's screen does not name the viewer as the sender (%s)", want)
	}
	if strings.Contains(screen, "Sent by you") {
		t.Error("the creator's screen calls the viewer's response theirs")
	}
	code, got := f.req(t, f.viewer, http.MethodPost, "/pages/"+sid+"/versions/2/upload", "x", map[string]string{"Content-Type": "multipart/form-data; boundary=x"})
	if code != http.StatusForbidden || !strings.Contains(got, TeamPageNoFile) {
		t.Errorf("a member's file from the owner view: %d %q; want 403 %q", code, got, TeamPageNoFile)
	}
}

// TestEveryPageRouteHasATeamRule is teamPageRoutes' ledger: every route in
// routes.golden whose pattern names a page ("pages/{id}") has a rule, and
// every rule is a registered route.
func TestEveryPageRouteHasATeamRule(t *testing.T) {
	b, err := os.ReadFile("testdata/routes.golden")
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	var named []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		registered[l] = true
		if strings.Contains(l, " /pages/{id}") {
			named = append(named, l)
		}
	}
	if len(named) < 10 {
		t.Fatalf("instrument check FAILED: %d page routes found in routes.golden", len(named))
	}
	for _, r := range named {
		if _, ok := teamPageRoutes[r]; !ok {
			t.Errorf("UNCLASSIFIED page route %s: add it to teamPageRoutes (team_pages.go) with what a team member may do through it", r)
		}
	}
	for r := range teamPageRoutes {
		if !registered[r] {
			t.Errorf("teamPageRoutes lists %s, which is not a registered route", r)
		}
	}
	if DetailRequestedByUser != "requested_by_user" {
		t.Errorf("DetailRequestedByUser = %q; plugins.AgentStartedBy reads requested_by_user", DetailRequestedByUser)
	}
	_ = url.Values{}
}
