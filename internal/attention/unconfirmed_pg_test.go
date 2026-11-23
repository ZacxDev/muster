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

// TestUnconfirmedIsTheOwnersFailedDeliveriesHoweverOld: an answer whose
// delivery failed a month ago is listed; a delivered one, an open ask and
// another owner's failed one are not. RED before Unconfirmed existed (it does
// not compile); the owner condition is pinned by the other owner's row.
func TestUnconfirmedIsTheOwnersFailedDeliveriesHoweverOld(t *testing.T) {
	s, pool, dsn := pgStore(t)
	agent2, agent3 := agentFor(t, pool, 2), agentFor(t, pool, 3)
	old := answered(t, s, 2, agent2, "left")
	fine := answered(t, s, 2, agent2, "right")
	theirs := answered(t, s, 3, agent3, "up")
	_ = mustAsk(t, s, 2, New{Asker: session, Question: "still open"})
	for _, id := range []int64{old.ID, theirs.ID} {
		dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET delivery = 'unconfirmed', delivery_detail = 'the paste was not seen', resolved_at = now() - interval '30 days' WHERE id = `+strconv.FormatInt(id, 10))
	}
	dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET delivery = 'delivered' WHERE id = `+strconv.FormatInt(fine.ID, 10))
	got, err := s.Unconfirmed(as(2))
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	if !slices.Equal(ids, []int64{old.ID}) {
		t.Fatalf("owner 2's unconfirmed: %v, want only %d (not the delivered %d nor owner 3's %d)", ids, old.ID, fine.ID, theirs.ID)
	}
	// Control: owner 3's own list holds theirs.
	if got, _ := s.Unconfirmed(as(3)); len(got) != 1 || got[0].ID != theirs.ID {
		t.Fatalf("control: owner 3's unconfirmed: %+v", got)
	}
}

// TestUnconfirmedLeavesOutATeamsAnswerItsMemberCanRead: a member reads the
// team's failed answer through row-level security (the control), yet their
// pinned list holds only their own — the query's owner condition decides.
// RED with the owner condition removed (mutation-checked).
func TestUnconfirmedLeavesOutATeamsAnswerItsMemberCanRead(t *testing.T) {
	s, pool, dsn := pgStore(t)
	ctx := context.Background()
	dbtest.OwnerExec(t, dsn, `SELECT setval(pg_get_serial_sequence('public.users', 'id'), (SELECT max(id) FROM public.users))`)
	us := users.NewPG(pool)
	var member int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', 'Una Unconfirmed') RETURNING id`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	team, err := us.CreateTeam(ctx, users.Actor{UserID: 1, Via: authz.ViaSession}, "Retriers "+strconv.FormatInt(time.Now().UnixNano()%1e9, 36), member)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := us.Account(authz.System(ctx), member)
	if err != nil {
		t.Fatal(err)
	}
	me := authz.WithPrincipal(ctx, authz.Principal{UserID: member, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
	asTeam := authz.WithPrincipal(ctx, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})
	create := func(c context.Context, q string) Ask {
		a, err := s.Create(c, New{Asker: session, Question: q})
		if err != nil {
			t.Fatal(err)
		}
		dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET status = 'answered', answer = 'x', answered_by = 'operator', resolved_at = now() - interval '9 days',
			delivery = 'unconfirmed', delivery_detail = 'the paste was not seen' WHERE id = `+strconv.FormatInt(a.ID, 10))
		return a
	}
	mine, teams := create(me, "mine, failed"), create(asTeam, "the team's, failed")
	if a, err := s.Get(me, teams.ID); err != nil || a.ID != teams.ID {
		t.Fatalf("control: the member reads the team's failed answer: %+v %v", a, err)
	}
	got, err := s.Unconfirmed(me)
	if err != nil || len(got) != 1 || got[0].ID != mine.ID {
		t.Fatalf("the member's unconfirmed: %+v %v; want only their own %d (not the team's %d)", got, err, mine.ID, teams.ID)
	}
}

// TestTheSweepClosesALapsedAskAtItsExpiry: ExpireDue writes resolved_at as
// the ask's expiry, not the sweep's clock, so a lapsed card keeps its place on
// the Done tab (it already read as expired at that time before the sweep).
// RED with resolved_at = now(): the swept ask's ClosedAt jumps to the sweep.
func TestTheSweepClosesALapsedAskAtItsExpiry(t *testing.T) {
	s, _, dsn := pgStore(t)
	a := mustAsk(t, s, 2, New{Asker: session, Question: "lapsing"})
	expiry := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET created_at = '`+expiry.Add(-time.Hour).Format(time.RFC3339)+`', expires_at = '`+
		expiry.Format(time.RFC3339)+`' WHERE id = `+strconv.FormatInt(a.ID, 10))
	before, err := s.Closed(as(2), ClosedPage{})
	if err != nil || len(before) != 1 || !before[0].ClosedAt().Equal(expiry) {
		t.Fatalf("before the sweep: %+v %v; want it closed at its expiry %v", before, err, expiry)
	}
	if _, err := s.ExpireDue(as(2)); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(as(2), a.ID)
	if err != nil || after.Status != Expired || !after.ResolvedAt.Equal(expiry) {
		t.Fatalf("after the sweep: %s resolved at %v (%v); want expired at %v", after.Status, after.ResolvedAt, err, expiry)
	}
}
