package pages

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// A TEAM'S PAGES (migration 0165), as the database decides them, under forced
// row-level security as the application role. This file uses only what the
// store had before 0165 (it reads; it records no creator), so it runs — and
// fails — against the tree before it too: there no member reads a team's page.
//
// The fixture's people and teams, each id distinct (so no assertion can pass
// because two of them are the same number):
//
//	creator     operator of A — started the agent that makes A's page
//	viewerA     viewer of A only
//	both        operator of A, viewer of B
//	onlyB       operator of B only
//	outsider    member of no team
//	team A, team B
type teamFixture struct {
	s                                       *PGStore
	creator, viewerA, both, onlyB, outsider int64
	teamA, teamB                            int64
	store                                   *users.PGStore
	dsn                                     string
}

func newTeamFixture(t *testing.T) *teamFixture {
	t.Helper()
	s, pool := pgStore(t)
	dsn := dbtest.DSN(t)
	f := &teamFixture{s: s, store: users.NewPG(pool), dsn: dsn}
	// Other tests here insert users at fixed ids, past the identity's
	// sequence: move it beyond them first.
	dbtest.OwnerExec(t, dsn, `SELECT setval(pg_get_serial_sequence('public.users', 'id'), (SELECT max(id) FROM public.users))`)
	for _, x := range []struct {
		id   *int64
		name string
	}{{&f.creator, "Cora Creator"}, {&f.viewerA, "Vic Viewer"}, {&f.both, "Bo Both"}, {&f.onlyB, "Bea OnlyB"}, {&f.outsider, "Otto Outsider"}} {
		if err := pool.QueryRow(authz.System(context.Background()), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, x.name).Scan(x.id); err != nil {
			t.Fatal(err)
		}
	}
	by := users.Actor{UserID: 1, Via: authz.ViaSession}
	stamp := strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
	ctx := context.Background()
	a, err := f.store.CreateTeam(ctx, by, "Alpha "+stamp, f.creator)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.store.CreateTeam(ctx, by, "Beta "+stamp, f.onlyB)
	if err != nil {
		t.Fatal(err)
	}
	f.teamA, f.teamB = a.ID, b.ID
	for _, m := range []struct {
		team, user int64
		role       authz.TeamRole
	}{
		{f.teamA, f.creator, authz.TeamOperator}, {f.teamA, f.viewerA, authz.TeamViewer},
		{f.teamA, f.both, authz.TeamOperator}, {f.teamB, f.both, authz.TeamViewer}, {f.teamB, f.onlyB, authz.TeamOperator},
	} {
		if err := f.store.SetTeamMember(ctx, by, m.team, m.user, m.role); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// person is uid's browser-session principal with the memberships the users
// store reads NOW — as every request builds it.
func (f *teamFixture) person(t *testing.T, uid int64) context.Context {
	t.Helper()
	a, err := f.store.Account(authz.System(context.Background()), uid)
	if err != nil {
		t.Fatal(err)
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: a.Role, Via: authz.ViaSession, Teams: a.Teams})
}

// asTeam is team's acting principal through its agent's door: how a team
// agent's page_create runs.
func asTeam(team int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})
}

// teamPage is a page team A's agent made, with a second version, a live link
// and a viewer's response through it.
func (f *teamFixture) teamPage(t *testing.T, team int64, title string) Page {
	t.Helper()
	ctx := asTeam(team)
	b, n := blocksOf(t, qBlocks)
	p, _, err := f.s.Create(ctx, Creator{AgentID: 900 + team, Label: "team-worker"}, title, b, n)
	if err != nil {
		t.Fatal(err)
	}
	if p.OwnerID != team {
		t.Fatalf("fixture: the team's page is owned by %d, want the team %d", p.OwnerID, team)
	}
	if _, _, err := f.s.AddVersion(ctx, Creator{AgentID: 900 + team}, p.ID, title+" v2", b, n); err != nil {
		t.Fatal(err)
	}
	l, err := f.s.CreateLink(ctx, p.ID, LinkLive, "team-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AddResponse(ctx, l, 2, "q1", KindSubmit, []byte(`{"answer":"from a stranger"}`), ""); err != nil {
		t.Fatal(err)
	}
	return p
}

// reach is what ctx reads of page id: the page, its versions, links and
// responses (counts), and whether it is in the caller's list.
type reach struct {
	page            bool
	versions, links int
	responses       int
	listed          bool
}

func (f *teamFixture) reach(t *testing.T, ctx context.Context, id int64) reach {
	t.Helper()
	var r reach
	if _, err := f.s.Page(ctx, id); err == nil {
		r.page = true
	} else if !errors.Is(err, ErrNotFound) {
		t.Fatalf("read page %d: %v", id, err)
	}
	vs, err := f.s.Versions(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := f.s.Links(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := f.s.Responses(ctx, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.s.Pages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.versions, r.links, r.responses = len(vs), len(ls), len(rs)
	r.listed = slices.ContainsFunc(list, func(x Summary) bool { return x.ID == id })
	return r
}

// TestATeamsPageIsReadByItsMembersAndNobodyElse: team A's page — its
// versions, links and responses — is read by every member of A (a viewer, an
// operator, a member of both teams, the creator), and by nobody else: not a
// member of B only, not a member of no team. CONTROL: the team itself reads
// all of it, so a zero is about the reader.
func TestATeamsPageIsReadByItsMembersAndNobodyElse(t *testing.T) {
	f := newTeamFixture(t)
	p := f.teamPage(t, f.teamA, "Alpha plan")
	all := reach{page: true, versions: 2, links: 1, responses: 1, listed: true}
	if got := f.reach(t, asTeam(f.teamA), p.ID); got != all {
		t.Fatalf("control: the team itself reads %+v of its page; want %+v", got, all)
	}
	for name, uid := range map[string]int64{"creator": f.creator, "viewer of A": f.viewerA, "member of both": f.both} {
		if got := f.reach(t, f.person(t, uid), p.ID); got != all {
			t.Errorf("%s reads %+v of team A's page; want %+v", name, got, all)
		}
	}
	for name, uid := range map[string]int64{"member of B only": f.onlyB, "member of no team": f.outsider} {
		if got := f.reach(t, f.person(t, uid), p.ID); got != (reach{}) {
			t.Errorf("%s reads %+v of team A's page; want nothing", name, got)
		}
	}
	// Team B's agent reads none of team A's page either.
	if got := f.reach(t, asTeam(f.teamB), p.ID); got != (reach{}) {
		t.Errorf("team B's agent reads %+v of team A's page; want nothing", got)
	}
}

// TestTheTeamsPageReachesOnlyThroughAPersonsOwnDoor: the creator, reading
// through a door where no person is present (their personal API token, their
// own agent), sees none of the team's page — only the browser session and the
// paired phone carry a person's teams (authz TeamScope). CONTROL: the same
// person through their paired phone reads it.
func TestTheTeamsPageReachesOnlyThroughAPersonsOwnDoor(t *testing.T) {
	f := newTeamFixture(t)
	p := f.teamPage(t, f.teamA, "Alpha doors")
	a, err := f.store.Account(authz.System(context.Background()), f.creator)
	if err != nil {
		t.Fatal(err)
	}
	door := func(via authz.Via) context.Context {
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.creator, Role: a.Role, Via: via, Teams: a.Teams})
	}
	if got := f.reach(t, door(authz.ViaDevice), p.ID); !got.page {
		t.Fatalf("control: the creator's paired phone reads %+v of the team's page", got)
	}
	for _, via := range []authz.Via{authz.ViaToken, authz.ViaAgent, authz.ViaMirrorHost, authz.ViaPageLink} {
		if got := f.reach(t, door(via), p.ID); got != (reach{}) {
			t.Errorf("the creator through the %s door reads %+v of the team's page; want nothing", via, got)
		}
	}
}

// TestARemovedMemberReadsNothingAtOnce: a member removed from team A reads
// none of its page on their very next request (the memberships are read per
// request; nothing is cached). CONTROL: they read it the moment before.
func TestARemovedMemberReadsNothingAtOnce(t *testing.T) {
	f := newTeamFixture(t)
	p := f.teamPage(t, f.teamA, "Alpha removal")
	if got := f.reach(t, f.person(t, f.viewerA), p.ID); !got.page {
		t.Fatalf("control: the viewer reads %+v before removal", got)
	}
	if err := f.store.RemoveTeamMember(context.Background(), users.Actor{UserID: 1, Via: authz.ViaSession}, f.teamA, f.viewerA); err != nil {
		t.Fatal(err)
	}
	if got := f.reach(t, f.person(t, f.viewerA), p.ID); got != (reach{}) {
		t.Errorf("a removed member reads %+v of the team's page; want nothing", got)
	}
}

// TestAPersonsOwnPageStaysPrivateFromTheirTeam: a member's OWN page (their
// own agent's) is read by nobody on their teams — not a fellow member, not
// the team's agent. CONTROL: its owner reads it.
func TestAPersonsOwnPageStaysPrivateFromTheirTeam(t *testing.T) {
	f := newTeamFixture(t)
	b, n := blocksOf(t, qBlocks)
	own, _, err := f.s.Create(f.person(t, f.both), agentA, "Bo's own", b, n)
	if err != nil {
		t.Fatal(err)
	}
	if own.OwnerID != f.both {
		t.Fatalf("fixture: the page is owned by %d, want %d", own.OwnerID, f.both)
	}
	if got := f.reach(t, f.person(t, f.both), own.ID); !got.page {
		t.Fatalf("control: its owner reads %+v", got)
	}
	for name, ctx := range map[string]context.Context{
		"a fellow member of A": f.person(t, f.creator), "a fellow member of B": f.person(t, f.onlyB),
		"team A's agent": asTeam(f.teamA), "team B's agent": asTeam(f.teamB),
	} {
		if got := f.reach(t, ctx, own.ID); got != (reach{}) {
			t.Errorf("%s reads %+v of a member's own page; want nothing", name, got)
		}
	}
}
