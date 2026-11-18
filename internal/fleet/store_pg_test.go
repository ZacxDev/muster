package fleet

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// The digest's marks on a real Postgres, AS muster_app (db.Connect's pool sets
// that role on every connection): the look and the owner scope. Every id is
// invented.

func pgFleet(t *testing.T) *PG {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM fleet_owner_marks`)
	return NewPG(pool)
}

func asUser(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

// TestTheLookMovesTheBaselineOnlyAfterAGap: no look → no baseline; a look
// LookGap or more after the previous one makes that previous one the
// baseline; a re-open inside the gap keeps it. Per owner.
func TestTheLookMovesTheBaselineOnlyAfterAGap(t *testing.T) {
	p := pgFleet(t)
	ctx := asUser(2)
	at := time.Date(2000, 1, 2, 9, 0, 0, 0, time.UTC)
	if b, err := p.Look(ctx, at); err != nil || !b.IsZero() {
		t.Fatalf("first look: %v, %v; want no baseline", b, err)
	}
	if b, err := p.Look(ctx, at.Add(3*time.Minute)); err != nil || !b.IsZero() {
		t.Fatalf("a re-open inside the gap: %v, %v; want still none", b, err)
	}
	// 3m + 10m: the previous look (at+3m) is LookGap before → the baseline.
	if b, err := p.Look(ctx, at.Add(13*time.Minute)); err != nil || !b.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("after the gap: %v, %v; want %v", b, err, at.Add(3*time.Minute))
	}
	if b, err := p.Baseline(ctx); err != nil || !b.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("baseline read: %v, %v", b, err)
	}
	if b, err := p.Look(ctx, at.Add(14*time.Minute)); err != nil || !b.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("a re-open a minute later: %v, %v; want the baseline kept", b, err)
	}
	if b, err := p.Baseline(asUser(3)); err != nil || !b.IsZero() {
		t.Fatalf("the other owner's baseline: %v, %v; want none", b, err)
	}
	if id, err := p.FleetChat(asUser(3)); err != nil || id != 0 {
		t.Fatalf("no fleet chat yet: %d, %v", id, err)
	}
}
