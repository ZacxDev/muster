package mirror_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
)

// TEAM HOSTS FOR VIEWERS (team sharing 2b.3; docs/design/team-sharing.md A,
// "The chokepoint"): Hosts stays own-only, VisibleHosts adds the principal's
// teams' hosts, and access — through ForOwner — reaches a team host only as
// far as the request's own memberships do, and only for viewing in this
// release.
//
// Synthetic fixtures: team 900341 with ana 900307 (operator), cal 900309
// (viewer) and dee 900310 (no member). Ids no constant in the code equals.
const (
	tTeam = int64(900341)
	tAna  = int64(900307)
	tCal  = int64(900309)
	tDee  = int64(900310)
)

// member is a browser-session principal for id with memberships ms (the shape
// requireSession attaches), signed in just now so a step-up would pass.
func member(t *testing.T, id int64, via authz.Via, ms ...authz.Membership) context.Context {
	t.Helper()
	m, err := authz.NewMemberships(ms)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: id, Role: authz.RoleMember, Via: via, Teams: m})
	return stepup.WithAuthTime(ctx, time.Now())
}

// twoBuild01s connects ana's own build-01 (session "ana-own") and team
// tTeam's build-01 (session "team-shared") to one rig.
func twoBuild01s(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	own, _ := r.store.mint(t, tAna, "build-01")
	ob := &fakeBackend{}
	ob.set("ana-own")
	r.connect(t, "build-01", own, ob, true)
	r.online(t, tAna, "build-01", "ana-own")
	team := r.store.mintTeam(t, tTeam, "build-01")
	tb := &fakeBackend{}
	tb.set("team-shared")
	r.connect(t, "build-01", team, tb, true)
	r.online(t, tTeam, "build-01", "team-shared")
	return r
}

func owners(hs []mirror.HostView) []int64 {
	var out []int64
	for _, h := range hs {
		out = append(out, h.Owner)
	}
	return out
}

// TestHostsNeverListsATeamHost: ana, an operator of the team, with BOTH
// build-01 entries online, gets ONE entry from Hosts — her own — and two from
// VisibleHosts, each carrying its owner. CONTROL: the same memberships on a
// door that carries no team (a personal API token) see only her own, through
// either listing.
func TestHostsNeverListsATeamHost(t *testing.T) {
	r := twoBuild01s(t)
	ana := member(t, tAna, authz.ViaSession, authz.Membership{Team: tTeam, Role: authz.TeamOperator})
	hs := r.svc.Hosts(ana)
	if len(hs) != 1 || hs[0].Owner != tAna || hs[0].Sessions[0] != "ana-own" {
		t.Fatalf("Hosts: %+v; want ana's own build-01 alone", hs)
	}
	vs := r.svc.VisibleHosts(ana)
	if len(vs) != 2 || vs[0].Owner != tAna || vs[1].Owner != tTeam || vs[1].Sessions[0] != "team-shared" {
		t.Fatalf("VisibleHosts owners %v: %+v; want ana's then the team's", owners(vs), vs)
	}
	token := member(t, tAna, authz.ViaToken, authz.Membership{Team: tTeam, Role: authz.TeamOperator})
	if got := owners(r.svc.VisibleHosts(token)); len(got) != 1 || got[0] != tAna {
		t.Fatalf("CONTROL: VisibleHosts on a personal API token: owners %v; want ana's own only", got)
	}
	dee := member(t, tDee, authz.ViaSession)
	if got := r.svc.VisibleHosts(dee); len(got) != 0 {
		t.Fatalf("dee (no member, no host) sees %+v", got)
	}
}

// TestAccessReachesATeamHostOnlyAsFarAsTheMembership: ForOwner aims a call
// at the team's build-01; access lets a viewer WATCH it (a read-only attach,
// its state, a scroll's door) and refuses — with the no-oracle
// ErrNoPrincipal — anyone who is not a member, a door that carries no team,
// and every typing call (an interactive attach, a paste) whatever the role:
// typing is 2b.4's. CONTROL: with no target, ana's call reaches her own
// build-01, and her own typing is untouched.
func TestAccessReachesATeamHostOnlyAsFarAsTheMembership(t *testing.T) {
	r := twoBuild01s(t)
	cal := mirror.ForOwner(member(t, tCal, authz.ViaSession, authz.Membership{Team: tTeam, Role: authz.TeamViewer}), tTeam)
	ana := mirror.ForOwner(member(t, tAna, authz.ViaSession, authz.Membership{Team: tTeam, Role: authz.TeamOperator}), tTeam)
	dee := mirror.ForOwner(member(t, tDee, authz.ViaSession), tTeam)
	token := mirror.ForOwner(member(t, tCal, authz.ViaToken, authz.Membership{Team: tTeam, Role: authz.TeamViewer}), tTeam)

	// The viewer watches: a read-only attach reaches the team's pane.
	_, screen, _, cancel, res := attach(t, cal, r.svc, "build-01", "team-shared", true)
	eventually(t, "the team's pane drawn for its viewer", func() bool { return screen.String() != "" })
	cancel()
	_ = within(t, res, "the viewer's read-only attach")
	if st := r.svc.AgentState(cal, "build-01", "team-shared", ""); errors.Is(st.Unknown, mirror.ErrNoPrincipal) {
		t.Fatalf("a viewer's AgentState was refused: %v", st.Unknown)
	}

	refused := func(who string, err error) {
		t.Helper()
		if !errors.Is(err, mirror.ErrNoPrincipal) {
			t.Errorf("%s: %v; want ErrNoPrincipal", who, err)
		}
	}
	for who, ctx := range map[string]context.Context{"dee (no member)": dee, "a personal API token": token} {
		_, _, _, _, res := attach(t, ctx, r.svc, "build-01", "team-shared", true)
		refused(who+": read-only attach", within(t, res, who))
		if st := r.svc.AgentState(ctx, "build-01", "team-shared", ""); !errors.Is(st.Unknown, mirror.ErrNoPrincipal) {
			t.Errorf("%s: AgentState %+v; want refused", who, st)
		}
	}
	for who, ctx := range map[string]context.Context{"cal (viewer)": cal, "ana (operator)": ana} {
		refused(who+": paste", r.svc.PasteWindow(ctx, "build-01", "team-shared", "", "hello", mirror.PasteActivePane))
		_, _, _, _, res := attach(t, ctx, r.svc, "build-01", "team-shared", false)
		refused(who+": interactive attach", within(t, res, who))
	}

	// CONTROL: no target is ana's own host, typing included.
	own := member(t, tAna, authz.ViaSession, authz.Membership{Team: tTeam, Role: authz.TeamOperator})
	stdin, screen2, _, cancel2, res2 := attach(t, own, r.svc, "build-01", "ana-own", false)
	if _, err := stdin.Write([]byte("x")); err != nil {
		t.Fatalf("ana's own interactive attach: %v", err)
	}
	eventually(t, "ana's own pane drawn", func() bool { return screen2.String() != "" })
	cancel2()
	_ = within(t, res2, "ana's own attach")
	_, _, _, _, res3 := attach(t, own, r.svc, "build-01", "team-shared", true)
	if err := within(t, res3, "ana's untargeted attach to a session only the team lists"); err == nil {
		t.Fatal("an untargeted attach reached the team's session")
	}
}

// managedStore is memStore with mirror_tokens read as the MANAGED table it
// is since 0127: a person's session also lists their teams' tokens
// (ListTokens over every token, as RLS answers a member).
type managedStore struct{ *memStore }

func (m managedStore) ListTokens(ctx context.Context) ([]mirror.Token, error) {
	return m.memStore.ListTokens(authz.System(ctx))
}

// TestHostRanksRankOnlyTheOwnersTokens: a member's token listing holds their
// team's tokens too, and ranking them beside the member's own moved the
// member's own hosts' icons, and listed the team's host as one of theirs
// expected to connect. Now each ranks — and lists — its owner's tokens only:
// ana's own hosts rank 0 and 1 whatever the team minted before them; aimed at
// the team (ForOwner, ana a viewer of it) the team's host ranks 0.
//
// 🔴 RED at the base (fcd4bbfd): ana's first host ranked 1, after the team's.
func TestHostRanksRankOnlyTheOwnersTokens(t *testing.T) {
	ms := newMemStore()
	svc := mirror.NewService(managedStore{ms}, nil)
	ms.mintTeam(t, tTeam, "z-team-host")
	ms.mint(t, tAna, "a-first")
	ms.mint(t, tAna, "a-second")
	ana := member(t, tAna, authz.ViaSession, authz.Membership{Team: tTeam, Role: authz.TeamViewer})
	got := svc.HostRanks(ana)
	if len(got) != 2 || got["a-first"] != 0 || got["a-second"] != 1 {
		t.Fatalf("ana's own ranks: %v; want a-first 0, a-second 1 and nothing of the team's", got)
	}
	live, err := svc.LiveTokenHosts(ana)
	if err != nil || len(live) != 2 || live[0] != "a-first" || live[1] != "a-second" {
		t.Fatalf("ana's expected hosts: %v %v; want her own two", live, err)
	}
	if got := svc.HostRanks(mirror.ForOwner(ana, tTeam)); len(got) != 1 || got["z-team-host"] != 0 {
		t.Fatalf("the team's ranks, aimed at the team: %v; want z-team-host 0 alone", got)
	}
}
