package morning

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

// The reports' rows on a real Postgres, AS muster_app (db.Connect's pool sets
// that role on every connection): one row per owner per day, the push claimed
// once and never for a quiet day, and one owner's reports invisible to
// another. Every id and value is invented.

func pgMorning(t *testing.T) *PG {
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
	dbtest.OwnerExec(t, dsn, `DELETE FROM morning_reports`)
	return NewPG(pool)
}

func asUser(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

var (
	day1Since = time.Date(2000, 1, 1, 14, 0, 0, 0, time.UTC)
	day1Until = time.Date(2000, 1, 2, 14, 0, 0, 0, time.UTC)
)

func loudRow(date string) Row {
	return Row{Date: date, Since: day1Since, Until: day1Until, Questions: 2,
		Body: Body{Done: []Done{{Kind: DoneTask, Label: "Rotate the example key", Href: "/tasks/41"}}}}
}

// TestOneReportPerOwnerPerDay: the day's second insert writes nothing and
// says so; another day, and the same day for another owner, are their own.
func TestOneReportPerOwnerPerDay(t *testing.T) {
	p := pgMorning(t)
	a, b := asUser(2), asUser(3)
	if ok, err := p.Insert(a, loudRow("2000-01-02")); err != nil || !ok {
		t.Fatalf("first insert: %v, %v", ok, err)
	}
	second := loudRow("2000-01-02")
	second.Questions = 9
	if ok, err := p.Insert(a, second); err != nil || ok {
		t.Fatalf("the day's second insert: %v, %v; want false, nil", ok, err)
	}
	got, ok, err := p.Get(a, "2000-01-02")
	if err != nil || !ok || got.Questions != 2 || len(got.Body.Done) != 1 || got.Body.Done[0].Label != "Rotate the example key" {
		t.Fatalf("the stored report is not the first one: %+v, %v, %v", got, ok, err)
	}
	if ok, err := p.Insert(a, loudRow("2000-01-03")); err != nil || !ok {
		t.Fatalf("the next day: %v, %v", ok, err)
	}
	if ok, err := p.Insert(b, loudRow("2000-01-02")); err != nil || !ok {
		t.Fatalf("another owner's same day: %v, %v", ok, err)
	}
	if l, ok, err := p.Latest(a); err != nil || !ok || l.Date != "2000-01-03" {
		t.Fatalf("latest: %+v, %v, %v", l.Date, ok, err)
	}
}

// TestThePushIsClaimedOnceAndNeverForAQuietDay: a loud day's push is claimed
// by exactly one caller; a quiet day's never is; a day with no report never
// is.
func TestThePushIsClaimedOnceAndNeverForAQuietDay(t *testing.T) {
	p := pgMorning(t)
	ctx := asUser(2)
	at := day1Until.Add(time.Minute)
	if _, err := p.Insert(ctx, loudRow("2000-01-02")); err != nil {
		t.Fatal(err)
	}
	if ok, err := p.ClaimPush(ctx, "2000-01-02", at); err != nil || !ok {
		t.Fatalf("the first claim: %v, %v; want true", ok, err)
	}
	if ok, err := p.ClaimPush(ctx, "2000-01-02", at.Add(time.Minute)); err != nil || ok {
		t.Fatalf("the second claim: %v, %v; want false", ok, err)
	}
	if r, _, _ := p.Get(ctx, "2000-01-02"); !r.PushedAt.Equal(at) {
		t.Fatalf("pushed_at = %v; want the first claim's %v", r.PushedAt, at)
	}
	quiet := loudRow("2000-01-03")
	quiet.Quiet, quiet.Questions, quiet.Body = true, 0, Body{}
	if _, err := p.Insert(ctx, quiet); err != nil {
		t.Fatal(err)
	}
	if ok, err := p.ClaimPush(ctx, "2000-01-03", at); err != nil || ok {
		t.Fatalf("a quiet day's claim: %v, %v; want false", ok, err)
	}
	if ok, err := p.ClaimPush(ctx, "2000-01-04", at); err != nil || ok {
		t.Fatalf("a day with no report: %v, %v; want false", ok, err)
	}
}

// TestAnOwnerSeesOnlyItsOwnReports: B reads none of A's reports and cannot
// claim A's push; A reads its own (the control).
func TestAnOwnerSeesOnlyItsOwnReports(t *testing.T) {
	p := pgMorning(t)
	a, b := asUser(2), asUser(3)
	if _, err := p.Insert(a, loudRow("2000-01-02")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := p.Get(a, "2000-01-02"); err != nil || !ok {
		t.Fatalf("control: A cannot read its own report: %v, %v", ok, err)
	}
	if _, ok, err := p.Get(b, "2000-01-02"); err != nil || ok {
		t.Fatalf("B read A's report: %v, %v", ok, err)
	}
	if _, ok, err := p.Latest(b); err != nil || ok {
		t.Fatalf("B's latest is A's: %v, %v", ok, err)
	}
	if ok, err := p.ClaimPush(b, "2000-01-02", day1Until); err != nil || ok {
		t.Fatalf("B claimed A's push: %v, %v", ok, err)
	}
	if ok, err := p.ClaimPush(a, "2000-01-02", day1Until); err != nil || !ok {
		t.Fatalf("control: A's own claim: %v, %v", ok, err)
	}
}
