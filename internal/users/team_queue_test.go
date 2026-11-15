package users

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// A MEMBER'S QUEUED SENDS END WITH THEIR ROLE (team sharing 2b.4;
// team_queue.go), against real Postgres. The rows are planted under System
// with queued_by written by hand, and read back directly, so the cases name
// no 2b.4 symbol.

// queueRow plants a queued send_queue row owned by owner, queued by by, and
// returns its id. As the LOGIN role (dbtest.OwnerExec): the actor trigger
// forces queued_by for the application role only, so the column is written
// as given.
func queueRow(t *testing.T, pool *pgxpool.Pool, owner, by int64, body string) int64 {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO send_queue (owner_id, queued_by, mirror_host, mirror_session, mirror_window, body, stepup_at)
		VALUES ($1, $2, 'build-01', 'main', '', $3, now())`, owner, by, body)
	var id int64
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT id FROM send_queue WHERE body = $1`, body).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// queueState is row id's state and detail.
func queueState(t *testing.T, pool *pgxpool.Pool, id int64) (string, string) {
	t.Helper()
	var st, d string
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT state, detail FROM send_queue WHERE id = $1`, id).Scan(&st, &d); err != nil {
		t.Fatal(err)
	}
	return st, d
}

// TestLeavingATeamCancelsYourQueuedSendsToIt: removing ben cancels what he
// queued into the TEAM's windows, in the removal's transaction, saying so;
// dropping him to viewer does the same with its own words. CONTROLS: his own
// personal row, and the manager's row on the same team, stay queued; a
// second team's row of his is left to that team.
//
// 🔴 RED at the base: nothing cancelled a member's team rows (only the
// delivery check would have refused them).
func TestLeavingATeamCancelsYourQueuedSendsToIt(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM send_queue`) })
	rt := newRotateTeam(t, s, pool, "q")
	other, err := s.CreateTeam(ctx, admin, "rotate-q-other", rt.ana)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamMember(ctx, admin, other.ID, rt.ben, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}

	// Removed.
	bens := queueRow(t, pool, rt.team, rt.ben, "q: ben's team row")
	anas := queueRow(t, pool, rt.team, rt.ana, "q: the manager's team row")
	personal := queueRow(t, pool, rt.ben, rt.ben, "q: ben's own row")
	elsewhere := queueRow(t, pool, other.ID, rt.ben, "q: ben's row on another team")
	if err := s.RemoveTeamMemberAsManager(ctx, Actor{UserID: rt.ana, Via: authz.ViaSession}, rt.team, rt.ben); err != nil {
		t.Fatal(err)
	}
	if st, d := queueState(t, pool, bens); st != "cancelled" || d != "Cancelled: the person who queued it is no longer a member of the team." {
		t.Fatalf("ben's team row after his removal: %s %q", st, d)
	}
	for name, id := range map[string]int64{"the manager's team row": anas, "ben's own row": personal, "ben's row on another team": elsewhere} {
		if st, _ := queueState(t, pool, id); st != "queued" {
			t.Errorf("CONTROL: %s after ben's removal: %s; want queued", name, st)
		}
	}

	// Dropped to viewer (on the other team).
	if err := s.SetTeamMember(ctx, admin, other.ID, rt.ben, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	if st, d := queueState(t, pool, elsewhere); st != "cancelled" || d != "Cancelled: the person who queued it is no longer an operator of the team." {
		t.Fatalf("ben's row on the other team after his drop to viewer: %s %q", st, d)
	}
	if st, _ := queueState(t, pool, personal); st != "queued" {
		t.Errorf("CONTROL: ben's own row after his drop to viewer: %s; want queued", st)
	}
}
