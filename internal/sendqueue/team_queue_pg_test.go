package sendqueue

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// A TEAM WINDOW'S QUEUE (team sharing 2b.4), against real Postgres. Synthetic
// fixture: team 50 ("q-team"), user 2 an operator of it, user 3 a viewer of
// it. The team's window has the SAME address as user 2's own (win): host
// names are per owner, so the queue tells the two apart by owner alone.

const qTeam int64 = 50

// teamRig plants the team row and returns the store and pool.
func teamRig(t *testing.T) (*PGStore, *pgxpool.Pool, string) {
	t.Helper()
	s, pool := pgStore(t)
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role, kind, display_name) OVERRIDING SYSTEM VALUE VALUES ($1, 'member', 'team', 'q-team')
		ON CONFLICT (id) DO NOTHING`, qTeam)
	return s, pool, dsn
}

// asMember is uid's request principal holding role in qTeam.
func asMember(t *testing.T, uid int64, role authz.TeamRole) context.Context {
	t.Helper()
	m, err := authz.NewMemberships([]authz.Membership{{Team: qTeam, Role: role}})
	if err != nil {
		t.Fatal(err)
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession, Teams: m})
}

// plantTeamRow writes a queued row into the team's window, queued by user 2,
// as the login role. Its id.
func plantTeamRow(t *testing.T, pool *pgxpool.Pool, dsn, body string) int64 {
	t.Helper()
	dbtest.OwnerExec(t, dsn, `INSERT INTO send_queue (owner_id, queued_by, mirror_host, mirror_session, mirror_window, body, stepup_at)
		VALUES ($1, 2, $2, $3, $4, $5, now())`, qTeam, win.Host, win.Session, win.Window, body)
	var id int64
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT id FROM send_queue WHERE body = $1`, body).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAMembersOwnQueueNeverReadsTheTeamsSameNamedWindow: user 2's OWN queue
// for win — its list, its head, a paste's turn stamp — holds only user 2's
// row, though user 2's principal reads the team's rows (0127) and the team's
// window has the same address. CONTROL: the team's row is there, readable as
// user 2 by id.
//
// 🔴 RED at the base: the list and the head read by address alone, so the
// team's row was listed in the member's own window, and a Send into the
// member's own window stamped the team's queue.
func TestAMembersOwnQueueNeverReadsTheTeamsSameNamedWindow(t *testing.T) {
	s, pool, dsn := teamRig(t)
	member := asMember(t, 2, authz.TeamOperator)
	teamID := plantTeamRow(t, pool, dsn, "q: the team's")
	own, err := s.Enqueue(member, mirrorItem("q: user 2's own"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(member, teamID); err != nil || got.Owner != qTeam {
		t.Fatalf("CONTROL: the team's row as user 2: %+v %v", got, err)
	}
	items, err := s.List(member, win)
	if err != nil {
		t.Fatal(err)
	}
	if b := bodies(items); len(b) != 1 || b[0] != "queued:q: user 2's own" {
		t.Fatalf("user 2's own queue for win lists %q; want only user 2's own row", b)
	}
	if h, ok, err := s.HeadOf(member, win); err != nil || !ok || h.ID != own.ID {
		t.Fatalf("user 2's own head for win: %+v %v %v; want row %d", h.Item, ok, err, own.ID)
	}
	if err := s.NotePaste(member, win, 4242); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT after_since FROM send_queue WHERE id = $1`, teamID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("a paste into user 2's OWN window stamped the team's row: after_since %d", after)
	}
}

// TestATeamsQueueIsAddressedByItsOwner: Target.Owner addresses the team's
// queue. An operator lists it and queues into it (the row is the team's,
// queued_by the operator); a viewer cannot queue into it (row-level
// security's insert arm, at operator); a failed team row is retried into the
// TEAM's queue; and CancelQueuedBy cancels what user 2 queued into the team —
// that team's, or every team's — never user 2's own rows.
func TestATeamsQueueIsAddressedByItsOwner(t *testing.T) {
	s, pool, dsn := teamRig(t)
	member := asMember(t, 2, authz.TeamOperator)
	teamWin := win
	teamWin.Owner = qTeam
	planted := plantTeamRow(t, pool, dsn, "q: planted")
	items, err := s.List(member, teamWin)
	if err != nil || len(items) != 1 || items[0].ID != planted || items[0].QueuedBy != 2 || items[0].Queuer() != 2 {
		t.Fatalf("the team's queue as its operator: %+v %v; want the planted row, queued by 2", items, err)
	}
	queued, err := s.Enqueue(member, Item{Target: teamWin, Body: "q: queued by 2", StepUpAt: time.Now()})
	if err != nil || queued.Owner != qTeam || queued.QueuedBy != 2 {
		t.Fatalf("user 2 queueing into the team's window: %+v %v; want owner %d, queued by 2", queued, err, qTeam)
	}
	if _, err := s.Enqueue(asMember(t, 3, authz.TeamViewer), Item{Target: teamWin, Body: "q: a viewer's", StepUpAt: time.Now()}); err == nil {
		t.Fatal("a VIEWER queued into the team's window")
	}
	// A failed team row is retried into the team's queue.
	if _, err := pool.Exec(authz.System(context.Background()), `UPDATE send_queue SET state = 'failed', claimed_at = now(), finished_at = now() WHERE id = $1`, planted); err != nil {
		t.Fatal(err)
	}
	retried, err := s.Retry(member, planted, time.Now(), 0)
	if err != nil || retried.Owner != qTeam || retried.QueuedBy != 2 {
		t.Fatalf("user 2's retry of the team's failed row: %+v %v; want a row of the team's", retried, err)
	}
	own, err := s.Enqueue(member, mirrorItem("q: user 2's own"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.CancelQueuedBy(context.Background(), qTeam, 2, time.Now(), "q: cancelled")
	if err != nil || n != 2 {
		t.Fatalf("CancelQueuedBy(team, 2): %d %v; want the two team rows user 2 queued", n, err)
	}
	if got, _ := s.Get(member, own.ID); got.State != Queued {
		t.Fatalf("CancelQueuedBy touched user 2's OWN row: %s", got.State)
	}
	if got, _ := s.Get(member, queued.ID); got.State != Cancelled || got.Detail != "q: cancelled" {
		t.Fatalf("user 2's team row after CancelQueuedBy: %s %q", got.State, got.Detail)
	}
	again, err := s.Enqueue(member, Item{Target: teamWin, Body: "q: once more", StepUpAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.CancelQueuedBy(context.Background(), 0, 2, time.Now(), "q: every team"); err != nil || n != 1 {
		t.Fatalf("CancelQueuedBy(0, 2): %d %v; want 1", n, err)
	}
	if got, _ := s.Get(member, again.ID); got.State != Cancelled {
		t.Fatalf("user 2's team row after CancelQueuedBy(0): %s", got.State)
	}
	if got, _ := s.Get(member, own.ID); got.State != Queued {
		t.Fatalf("CancelQueuedBy(0) touched user 2's OWN row: %s", got.State)
	}
}
