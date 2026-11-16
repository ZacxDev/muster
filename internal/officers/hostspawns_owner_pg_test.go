package officers_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestTheRuntimesSpawnReadsAreTheActingOwnersOwn (O-B round 0, finding 1): a
// person who is a member of a team READS the team's spawn rows through
// row-level security (team_owner_read, migration 0133; team_lent_read, 0130)
// on any door that carries their memberships. A runtime read that trusted RLS
// alone would then hand the person's sweep the TEAM'S open unclaimed notice,
// which the settle withdraws as stale (its officer is not the person's).
// Every runtime read now names the acting principal as the owner (ownRowSQL).
//
// ⚠ AN INVARIANT GUARD, NOT A REPRODUCTION: today the runtime's own doors
// (ViaOfficer, ViaMirrorHost) carry no memberships into the database
// (authz.TeamScope, teamVias), so the sweep as shipped never met a team row.
// The filter is the second layer that holds if a door starts carrying them;
// the test reaches it through ViaSession, the door that does.
//
// The fixture is built so each assertion can only pass on the owner filter:
// the team's row and the person's share ONE officer id, instance, host and
// window, the team's row is the NEWER one (so "latest" would pick it), and the
// control proves the person really reads the team's row.
func TestTheRuntimesSpawnReadsAreTheActingOwnersOwn(t *testing.T) {
	dsn := dbtest.DSN(t)
	cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Connect(cctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(context.Background())
	sfx := time.Now().Format("150405.000000")
	var person, team int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "spawn-owner-person "+sfx).Scan(&person); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(sys, `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', $1) RETURNING id`, "spawn-owner-team "+sfx).Scan(&team); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_host_spawns WHERE owner_id IN ($1, $2)`, person, team)
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id IN ($1, $2)`, person, team)
	})
	teams, err := authz.NewMemberships([]authz.Membership{{Team: team, Role: authz.TeamManager}})
	if err != nil {
		t.Fatal(err)
	}
	// The person on a door that carries their memberships (see above), and
	// the team as itself (internal/api's teamActingPrincipal).
	asPerson := authz.WithPrincipal(context.Background(), authz.Principal{UserID: person, Role: authz.RoleMember, Via: authz.ViaSession, Teams: teams})
	asTeam := authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaOfficer})
	st := officers.NewPGHostSpawnStore(pool)
	const officer, instance, host, window = "marketing:example", "officer-marketing-example-1", "host-alpha", "@3"
	spawn := func(ctx context.Context, session string, notice int64) officers.HostSpawn {
		t.Helper()
		h, err := st.InsertHostSpawn(ctx, officer, instance, host)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := st.RecordHostSpawnWindow(ctx, h.ID, host, window, session); !ok || err != nil {
			t.Fatalf("record the window of %d: %v %v", h.ID, ok, err)
		}
		if ok, err := st.SetUnclaimedNotice(ctx, h.ID, notice); !ok || err != nil {
			t.Fatalf("set the notice of %d: %v %v", h.ID, ok, err)
		}
		return h
	}
	mine := spawn(asPerson, "s-person", 7101)
	theirs := spawn(asTeam, "s-team", 7202)
	if theirs.ID <= mine.ID || theirs.OwnerID != team || mine.OwnerID != person {
		t.Fatalf("fixture: rows %+v / %+v", mine, theirs)
	}

	// CONTROL: the person reads the team's row (team_owner_read). Without
	// this the assertions below would pass on RLS alone.
	visible, err := st.VisibleHostSpawns(asPerson, 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, h := range visible {
		seen = seen || h.ID == theirs.ID
	}
	if !seen {
		t.Fatalf("control: the person does not read the team's spawn %d (%+v); the fixture does not build the hazard", theirs.ID, visible)
	}

	// The unclaimed-notice settle's input: the person's sweep sees its own
	// notice only; the team's survives it and is still the team's to settle.
	open, err := st.OpenUnclaimedNotices(asPerson)
	if err != nil || len(open) != 1 || open[0].ID != mine.ID {
		t.Fatalf("the person's open notices = %+v, %v; want only spawn %d (not the team's %d)", open, err, mine.ID, theirs.ID)
	}
	if ok, _ := st.SettleUnclaimedNotice(asPerson, theirs.ID); ok {
		t.Fatal("the person settled the team's notice")
	}
	open, err = st.OpenUnclaimedNotices(asTeam)
	if err != nil || len(open) != 1 || open[0].ID != theirs.ID || open[0].NoticeSettled {
		t.Fatalf("the team's open notices after the person's sweep = %+v, %v; want spawn %d still open", open, err, theirs.ID)
	}

	// The spawn budget: the same officer id under two owners counts each
	// owner's own spawns.
	since := time.Now().Add(-time.Hour)
	if n, err := st.CountHostSpawnsSince(asPerson, officer, since); err != nil || n != 1 {
		t.Fatalf("the person's spawns of %s = %d, %v; want 1 (the team's counted?)", officer, n, err)
	}
	if n, err := st.CountHostSpawnsSince(asTeam, officer, since); err != nil || n != 1 {
		t.Fatalf("the team's spawns of %s = %d, %v; want 1", officer, n, err)
	}

	// The siblings: hosts are named per owner, so each host-keyed read, and
	// the latest-per-instance read, answers the person's rows only.
	latest, err := st.LatestHostSpawns(asPerson, officer)
	if err != nil || latest[instance].ID != mine.ID {
		t.Fatalf("the person's latest spawn of %s = %+v, %v; want %d (the team's newer %d is not theirs)", instance, latest[instance], err, mine.ID, theirs.ID)
	}
	if ws, err := st.HostSpawnsInWindows(asPerson, host, []string{window}); err != nil || len(ws) != 1 || ws[0].ID != mine.ID {
		t.Fatalf("the person's spawns in %s on %s = %+v, %v; want only %d", window, host, ws, err, mine.ID)
	}
	if h, err := st.HostSpawnForSession(asPerson, host, "s-team"); err == nil {
		t.Fatalf("the person's host resolved the team's session to spawn %d", h.ID)
	}
	if h, err := st.HostSpawnForSession(asPerson, host, "s-person"); err != nil || h.ID != mine.ID {
		t.Fatalf("control: the person's own session = %+v, %v", h, err)
	}
	if ids, err := st.HostSpawnIDs(asPerson, host, []int64{mine.ID, theirs.ID}); err != nil || len(ids) != 1 || !ids[mine.ID] {
		t.Fatalf("the person's spawn ids on %s = %v, %v; want only %d", host, ids, err, mine.ID)
	}
}
