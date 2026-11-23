package attention

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// TestConfirmAsksForDraftsOrdersCapsAndScopesToTheOwner: one read for several
// drafts gives each draft its OWN owner's confirm asks — open ones first, then
// newest; an open ask past its expiry counts as closed — cut at n, with the
// uncapped count. Another owner's card on the same draft id is not one of
// them, a draft with none is absent, and n = 0 lists none but still counts.
//
// Draft ids are far from any ask id, so a query that matched one against the
// other could not pass.
func TestConfirmAsksForDraftsOrdersCapsAndScopesToTheOwner(t *testing.T) {
	s, _, dsn := pgStore(t)
	const owner, stranger = int64(2), int64(3)
	const many, one, none = int64(910001), int64(910002), int64(910003)
	card := func(uid, draft int64) Ask {
		return mustAsk(t, s, uid, New{Asker: Asker{System: true, Label: SystemAskerLabel}, Options: []string{"Confirm", "Refuse"},
			StepUp: &StepUpCard{Tool: "plugin_draft_decide", Title: "Post to X? · Draft #" + strconv.FormatInt(draft, 10), Why: "W", Preview: "P", Details: "D", DraftID: draft}})
	}
	ctx := as(owner)
	openOld := card(owner, many)
	answeredOld := card(owner, many)
	openNew := card(owner, many)
	cancelled := card(owner, many)
	answeredNew := card(owner, many)
	lapsed := card(owner, many)
	single := card(owner, one)
	theirs := card(stranger, many) // newest of all, and another owner's
	if _, err := s.Answer(ctx, answeredOld.ID, "Confirm"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Answer(ctx, answeredNew.ID, "Refuse"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET created_at = now() - interval '2 minutes', expires_at = now() - interval '1 minute' WHERE id = `+strconv.FormatInt(lapsed.ID, 10))

	ids := func(as []Ask) []int64 {
		out := []int64{}
		for _, a := range as {
			out = append(out, a.ID)
		}
		return out
	}
	got, err := s.ConfirmAsksForDrafts(ctx, []int64{many, one, none, 0}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{openNew.ID, openOld.ID, lapsed.ID}; !slices.Equal(ids(got[many].Asks), want) || got[many].Total != 6 {
		t.Errorf("draft %d: %v of %d; want %v of 6 (open first, newest; the lapsed one closed)", many, ids(got[many].Asks), got[many].Total, want)
	}
	if want := []int64{single.ID}; !slices.Equal(ids(got[one].Asks), want) || got[one].Total != 1 {
		t.Errorf("draft %d: %v of %d; want %v of 1", one, ids(got[one].Asks), got[one].Total, want)
	}
	if c, ok := got[none]; ok {
		t.Errorf("a draft with no confirm ask read %+v", c)
	}
	if len(got) != 2 {
		t.Errorf("read %d drafts, want 2: %+v", len(got), got)
	}
	for _, a := range got[many].Asks {
		if a.ID == theirs.ID || a.Owner != owner {
			t.Errorf("another owner's card %d is listed", a.ID)
		}
	}
	// The whole list, uncut: every one of the owner's, in order.
	all, err := s.ConfirmAsksForDrafts(ctx, []int64{many}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{openNew.ID, openOld.ID, lapsed.ID, answeredNew.ID, cancelled.ID, answeredOld.ID}; !slices.Equal(ids(all[many].Asks), want) {
		t.Errorf("uncut: %v; want %v", ids(all[many].Asks), want)
	}
	// The stranger reads their own card only.
	if s2, err := s.ConfirmAsksForDrafts(as(stranger), []int64{many}, 3); err != nil || !slices.Equal(ids(s2[many].Asks), []int64{theirs.ID}) || s2[many].Total != 1 {
		t.Errorf("the stranger read %+v, %v; want only %d", s2, err, theirs.ID)
	}
	// n = 0: nothing listed, the count kept.
	if z, err := s.ConfirmAsksForDrafts(ctx, []int64{many}, 0); err != nil || len(z[many].Asks) != 0 || z[many].Total != 6 {
		t.Errorf("n = 0: %+v, %v", z, err)
	}
	// No draft: nothing read.
	if e, err := s.ConfirmAsksForDrafts(ctx, nil, 3); err != nil || len(e) != 0 {
		t.Errorf("no drafts: %+v, %v", e, err)
	}
}

// TestConfirmAsksForDraftsLeavesOutATeamsAndTheChiefs: the member reads the
// team's card on the draft through row-level security (the control proves it
// READABLE, so it is the query's owner condition that keeps it out), and a
// question to the chief bound to the draft is no owner page's: the member's
// confirm asks of the draft are their own card alone. RED with the owner
// condition removed: the team's card is listed.
func TestConfirmAsksForDraftsLeavesOutATeamsAndTheChiefs(t *testing.T) {
	s, pool, dsn := pgStore(t)
	ctx := context.Background()
	dbtest.OwnerExec(t, dsn, `SELECT setval(pg_get_serial_sequence('public.users', 'id'), (SELECT max(id) FROM public.users))`)
	us := users.NewPG(pool)
	var member int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', 'Cora Confirmer') RETURNING id`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	team, err := us.CreateTeam(ctx, users.Actor{UserID: 1, Via: authz.ViaSession}, "Confirmers "+strconv.FormatInt(time.Now().UnixNano()%1e9, 36), member)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := us.Account(authz.System(ctx), member)
	if err != nil {
		t.Fatal(err)
	}
	me := authz.WithPrincipal(ctx, authz.Principal{UserID: member, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
	asTeam := authz.WithPrincipal(ctx, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})
	const draft = int64(920001)
	card := New{Asker: Asker{System: true, Label: SystemAskerLabel}, Options: []string{"Confirm", "Refuse"},
		StepUp: &StepUpCard{Tool: "plugin_draft_decide", Title: "Post to X? · Draft #920001", Why: "W", Preview: "P", Details: "D", DraftID: draft}}
	mine, err := s.Create(me, card)
	if err != nil {
		t.Fatal(err)
	}
	teams, err := s.Create(asTeam, card)
	if err != nil {
		t.Fatal(err)
	}
	chief, err := s.Create(me, New{Asker: session, Question: "to my chief", ToChief: true})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET step_up = '{"tool":"plugin_draft_decide","title":"T","why":"W","preview":"P","details":"D","draft_id":920001}' WHERE id = `+strconv.FormatInt(chief.ID, 10))
	if a, err := s.Get(me, teams.ID); err != nil || a.ID != teams.ID {
		t.Fatalf("control: the member reads the team's card %d: %+v %v", teams.ID, a, err)
	}
	got, err := s.ConfirmAsksForDrafts(me, []int64{draft}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[draft].Asks) != 1 || got[draft].Asks[0].ID != mine.ID || got[draft].Total != 1 {
		t.Errorf("the member's confirm asks of draft %d: %+v; want only their own %d (not the team's %d or the chief's %d)", draft, got[draft], mine.ID, teams.ID, chief.ID)
	}
	if tg, err := s.ConfirmAsksForDrafts(asTeam, []int64{draft}, 10); err != nil || len(tg[draft].Asks) != 1 || tg[draft].Asks[0].ID != teams.ID {
		t.Errorf("control: the team's own read: %+v %v; want %d", tg, err, teams.ID)
	}
}
