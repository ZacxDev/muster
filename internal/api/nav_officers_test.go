package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE SIDEBAR'S OFFICERS GROUP, the server's half (nav_officers.go, and
// AgentCardView.Officer in cardViewIndexed).

// TestTheSidebarsOfficerFieldIsTheServersBindingRule: the card of a declared
// officer's instance names its officer; an agent NAMED exactly like an
// instance but owned by someone else (officerOf's owner rule), one named in
// the officer namespace for no declared officer, and an ordinary agent name
// none — so the sidebar never groups an agent by its name.
func TestTheSidebarsOfficerFieldIsTheServersBindingRule(t *testing.T) {
	r := newOfficerRig(t)
	inst := r.addInstance(r.a, 1, 831)
	r.store.mu.Lock()
	r.store.rows = append(r.store.rows,
		// The SAME name pattern as r.a's second instance, but another owner's.
		agents.Agent{ID: 832, Name: officers.InstanceName(r.a.ID, 2), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 2},
		// In the officer namespace, for an officer nobody declares.
		agents.Agent{ID: 833, Name: officers.InstanceName("audit:nobody", 1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1},
	)
	r.store.mu.Unlock()
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	cards, _, err := r.s.agentsListEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for _, c := range cards {
		got[c.ID] = c.Officer
	}
	want := map[int64]string{inst.ID: r.a.Key(), 832: "", 833: "", officerOrdinaryID: "", officerChiefID: ""}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("agent %d is not listed", id)
			continue
		}
		if g != w {
			t.Errorf("agent %d: Officer = %q, want %q", id, g, w)
		}
	}
	if r.a.Key() == "" {
		t.Fatal("the fixture officer has no key")
	}
}

// navCountingReads is navOfficerReads that counts its calls, answering fixed
// maps or failing.
type navCountingReads struct {
	counts, spends, unreads       int
	countErr, spendErr, unreadErr error
	queued                        map[builtin.OfficerRef]int
	spent                         map[builtin.OfficerRef]float64
	unread                        map[builtin.OfficerRef]int
	countOwners                   [][]int64
}

func (c *navCountingReads) UnreadCounts(context.Context, []int64, time.Time) (map[builtin.OfficerRef]int, error) {
	c.unreads++
	if c.unreadErr != nil {
		return nil, c.unreadErr
	}
	return c.unread, nil
}

func (c *navCountingReads) ItemCounts(_ context.Context, owners []int64, _ []string) (map[builtin.OfficerRef]int, error) {
	c.counts++
	c.countOwners = append(c.countOwners, append([]int64(nil), owners...))
	if c.countErr != nil {
		return nil, c.countErr
	}
	return c.queued, nil
}

func (c *navCountingReads) SpentOn(context.Context, []int64, time.Time) (map[builtin.OfficerRef]float64, error) {
	c.spends++
	if c.spendErr != nil {
		return nil, c.spendErr
	}
	return c.spent, nil
}

const navTeam int64 = 900

// navRosterServer is a server whose officer runtime holds n builtin env
// officers of user 1 (every second one with a daily budget), one of team
// navTeam, one of user 77 (whom the viewer cannot see), one DB-declared
// officer of user 1 that does not run, and two DB-declared officers the viewer
// cannot see (user 77's, and team 555's, a team they are not in).
func navRosterServer(t *testing.T, n int) *Server {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	var env []officers.Officer
	for i := 0; i < n; i++ {
		o := officers.Officer{ID: "scout:area" + string(rune('a'+i)), Owner: 1, MaxInstances: 1, Backend: officers.BackendBuiltin}
		if i%2 == 0 {
			o.Budget.DailyUSD = 7.5 + float64(i)
		}
		env = append(env, o)
	}
	env = append(env,
		officers.Officer{ID: "ops:watch", Owner: navTeam, MaxInstances: 1, Backend: officers.BackendBuiltin, Budget: officers.Budget{DailyUSD: 9}},
		officers.Officer{ID: "hidden:other", Owner: 77, MaxInstances: 1, Backend: officers.BackendBuiltin, Budget: officers.Budget{DailyUSD: 3}},
	)
	s.officerRT = &officerRuntime{reg: officers.NewRegistry(env), env: env, now: func() time.Time { return time.Date(2000, 1, 2, 3, 0, 0, 0, time.UTC) },
		loaded: true, declared: map[string]declaredState{
			declKey(1, "draft:desk"): {row: builtin.DeclaredRow{Owner: 1, DefInfo: builtin.DefInfo{Officer: "draft:desk"}, TypeName: "Draft desk",
				Fields: officers.Declaration{InstanceSlug: "o-draftdesk"}}, reason: "fixture: it does not run"},
			// Another user's declared officer, and one of a team the viewer
			// is NOT in: neither is the viewer's to see.
			declKey(77, "other:desk"): {row: builtin.DeclaredRow{Owner: 77, DefInfo: builtin.DefInfo{Officer: "other:desk"}, TypeName: "Other desk",
				Fields: officers.Declaration{InstanceSlug: "o-otherdesk"}}, reason: "fixture"},
			declKey(555, "strange:team"): {row: builtin.DeclaredRow{Owner: 555, DefInfo: builtin.DefInfo{Officer: "strange:team"}, TypeName: "Strange team",
				Fields: officers.Declaration{InstanceSlug: "o-strangeteam"}}, reason: "fixture"},
		}}
	return s
}

func navViewer(t *testing.T) context.Context {
	t.Helper()
	ms, err := authz.NewMemberships([]authz.Membership{{Team: navTeam, Role: authz.TeamViewer}})
	if err != nil {
		t.Fatal(err)
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession, Teams: ms})
}

// TestTheOfficerRosterReadsInBatches: however many officers, ONE queued-count
// read, ONE spend read and ONE "new" read (no per-officer query) — measured at 2 and at 9
// officers; the roster is every officer the viewer can see (their own, their
// team's, the declared one that does not run) and nobody else's.
func TestTheOfficerRosterReadsInBatches(t *testing.T) {
	for _, n := range []int{2, 9} {
		s := navRosterServer(t, n)
		reads := &navCountingReads{}
		got := s.navOfficerRoster(navViewer(t), reads)
		if reads.counts != 1 || reads.spends != 1 || reads.unreads != 1 {
			t.Errorf("%d officers: %d queued-count reads, %d spend reads and %d new-count reads, want 1, 1 and 1", n, reads.counts, reads.spends, reads.unreads)
		}
		if want := n + 2; len(got) != want {
			t.Errorf("%d officers: roster of %d, want %d (own, the team's, the declared one)", n, len(got), want)
		}
		for _, o := range got {
			switch o.ID {
			case "hidden:other", "other:desk", "strange:team":
				t.Errorf("officer %s (not the viewer's, nor a team of theirs) is in the roster", o.ID)
			}
		}
	}
}

// TestTheRosterComposesQueueSpendAndHoldFromTheReads: the figures are the
// reads' own; held is the cap's own predicate (OverBudget, at the budget
// itself too); the team's officer is labelled and links with owner=; the
// declared row that does not run has no budget and no queue figure of its own
// beyond the count read.
func TestTheRosterComposesQueueSpendAndHoldFromTheReads(t *testing.T) {
	s := navRosterServer(t, 3) // scout:areaa ($7.50), scout:areab (none), scout:areac ($9.50)
	reads := &navCountingReads{
		queued: map[builtin.OfficerRef]int{{Owner: 1, Officer: "scout:areaa"}: 4, {Owner: navTeam, Officer: "ops:watch"}: 6},
		unread: map[builtin.OfficerRef]int{{Owner: 1, Officer: "scout:areaa"}: 3, {Owner: navTeam, Officer: "ops:watch"}: 7},
		spent: map[builtin.OfficerRef]float64{
			{Owner: 1, Officer: "scout:areaa"}:     6.25,
			{Owner: 1, Officer: "scout:areac"}:     9.5, // exactly at its budget: held
			{Owner: navTeam, Officer: "ops:watch"}: 11.75,
		},
	}
	got := map[string]ui.NavOfficer{}
	for _, o := range s.navOfficerRoster(navViewer(t), reads) {
		got[o.ID] = o
	}
	a := got["scout:areaa"]
	if a.Queued == nil || *a.Queued != 4 || a.Spent == nil || *a.Spent != 6.25 || a.Budget != 7.5 || a.Held || a.Unread != 3 {
		t.Errorf("scout:areaa = %+v", a)
	}
	b := got["scout:areab"]
	if b.Queued == nil || *b.Queued != 0 || b.Spent != nil || b.Budget != 0 || b.Held || b.Unread != 0 {
		t.Errorf("scout:areab (no budget, nothing queued) = %+v", b)
	}
	if c := got["scout:areac"]; c.Spent == nil || !c.Held {
		t.Errorf("scout:areac at its budget is not held: %+v", c)
	}
	w := got["ops:watch"]
	if w.TeamOwner != navTeam || w.Team == "" || w.Href != "/officers/ops:watch?owner=900" || !w.Held || w.Queued == nil || *w.Queued != 6 || w.Unread != 7 {
		t.Errorf("ops:watch = %+v", w)
	}
	d := got["draft:desk"]
	if d.Key != "o-draftdesk" || d.Name != "Draft desk" || d.Budget != 0 || d.Spent != nil || d.Href != "/officers/draft:desk" {
		t.Errorf("draft:desk = %+v", d)
	}
}

// TestAFailedReadDrawsNoFigure: a failed spend read leaves every officer with
// no spend and no hold (never a zero), a failed count read with no queue
// figure — and the roster is still drawn.
func TestAFailedReadDrawsNoFigure(t *testing.T) {
	s := navRosterServer(t, 3)
	reads := &navCountingReads{countErr: errors.New("fixture: count refused"), spendErr: errors.New("fixture: spend refused"),
		unreadErr: errors.New("fixture: new counts refused")}
	got := s.navOfficerRoster(navViewer(t), reads)
	if len(got) == 0 {
		t.Fatal("no roster on a failed read")
	}
	for _, o := range got {
		if o.Spent != nil || o.Held || o.Queued != nil || o.Unread != 0 {
			t.Errorf("%s drawn with a figure after a failed read: %+v", o.ID, o)
		}
	}
}

// TestNoOfficerStoreNoRoster: a server that keeps no officers (no built-in
// store: ui.Features.Officers false) draws no group, whatever the runtime
// holds.
func TestNoOfficerStoreNoRoster(t *testing.T) {
	s := navRosterServer(t, 2)
	if got := s.navOfficers(navViewer(t)); got != nil {
		t.Errorf("roster with the officer store off: %+v", got)
	}
}
