package officers_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// restartStore is a PG restart store on its own pool over the package's test
// database. Each call is a fresh "server".
func restartStore(t *testing.T, dsn string) *officers.PGRestartStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return officers.NewPGRestartStore(pool)
}

// TestTheAutoRestartRecordSurvivesANewServer: restarts and a held notice
// recorded through one store are read back by a NEW store on its own pool (a
// restarted server, or another replica), per instance and only for the day
// asked; clearing the held notice keeps the episode's last error. Then a
// fresh wake loop over that store honours the day's cap.
func TestTheAutoRestartRecordSurvivesANewServer(t *testing.T) {
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE officer_auto_restarts`)
	ctx := dbtest.AsOperator(context.Background())
	const off, day, other = "release:example-web", "2000-01-01", "2000-01-02"
	a, b := officers.InstanceName(off, 1), officers.InstanceName(off, 2)
	first := restartStore(t, dsn)
	at := time.Date(2000, 1, 1, 9, 41, 0, 0, time.UTC)
	for i, e := range []string{"crash one (fixture 5113)", "crash two (fixture 5129)"} {
		if err := first.RecordRestart(ctx, off, day, a, at.Add(time.Duration(i)*time.Minute), e); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.SetRestartHeld(ctx, off, day, b, true, "build spec: no token (fixture 5137)"); err != nil {
		t.Fatal(err)
	}
	if err := first.RecordRestart(ctx, off, other, a, at, "next day"); err != nil {
		t.Fatal(err)
	}
	second := restartStore(t, dsn)
	rows, err := second.RestartDay(ctx, off, day)
	if err != nil {
		t.Fatal(err)
	}
	ra, rb := rows[a], rows[b]
	if len(rows) != 2 || ra.Restarts != 2 || ra.LastError != "crash two (fixture 5129)" || !ra.LastRestartAt.Equal(at.Add(time.Minute)) || ra.HeldTold {
		t.Fatalf("instance 1's day: %+v (rows %d); want 2 restarts, the last error and time", ra, len(rows))
	}
	if rb.Restarts != 0 || !rb.HeldTold || rb.LastError != "build spec: no token (fixture 5137)" {
		t.Fatalf("instance 2's day: %+v; want held, its error, no restarts", rb)
	}
	if err := second.SetRestartHeld(ctx, off, day, b, false, ""); err != nil {
		t.Fatal(err)
	}
	rows, _ = first.RestartDay(ctx, off, day)
	if rows[b].HeldTold || rows[b].LastError != "build spec: no token (fixture 5137)" {
		t.Fatalf("cleared: %+v; want not held, the episode's error kept", rows[b])
	}

	// A fresh loop over the store honours today's 2 restarts at a cap of 2.
	r, _ := newRestartRigWith(t, 1, restartAfter, 2, second)
	r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	if relID != off {
		t.Fatalf("fixture: the rig's officer is %s", relID)
	}
	r.errorOut(1, officers.ErrorKickoffStuck)
	r.check(t)
	r.clk.Advance(time.Hour)
	if res := r.check(t); len(res.Decision.Restart) != 0 {
		t.Fatalf("a fresh loop restarted past the stored cap: %+v", res.Decision.Restart)
	}
}
