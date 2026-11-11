package askattempt

import (
	"context"
	"io"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// The answer-attempt log on a real Postgres: what a row holds as written, its
// numbering per card, that another owner sees none of it, the retention
// sweep, and the snapshot's redaction and cap.

func pgStore(t *testing.T) (*PG, *pgxpool.Pool) {
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
	dbtest.OwnerExec(t, dsn, `TRUNCATE ask_attempts`)
	return NewPG(pool), pool
}

func asUser(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

// t0 is a fixed instant (year 2000, the leak gate's allowed year).
var t0 = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)

func failedAttempt(card string) Attempt {
	return Attempt{At: t0, Runtime: RuntimeClaudeCode, Door: DoorCard, Action: ActionSubmit, Method: MethodTyped,
		Host: "ws", Session: "main", Window: "@1", CardID: card,
		Shape:  Shape{Options: []int{3, 2}, Multi: []bool{false, true}, Other: true},
		Reason: "lost", Keys: 4, Duration: 1500 * time.Millisecond, Snapshot: "the pane"}
}

// TestARowHoldsWhatWasRecordedAndIsNumberedPerCard: every column as
// literals; the outcome is the reason's (whatever the caller set); a second
// attempt on the same card is attempt 2, another card's is 1, another owner's
// on the same card id is 1.
func TestARowHoldsWhatWasRecordedAndIsNumberedPerCard(t *testing.T) {
	p, _ := pgStore(t)
	in := failedAttempt("toolu_card1")
	in.Outcome = OutcomeOK // the store's, never the caller's
	a, err := p.Record(asUser(2), in)
	if err != nil {
		t.Fatal(err)
	}
	want := Attempt{ID: a.ID, Owner: 2, At: t0, Runtime: "claude-code", Door: "card", Action: "submit", Method: "typed",
		Host: "ws", Session: "main", Window: "@1", CardID: "toolu_card1",
		Shape:   Shape{Options: []int{3, 2}, Multi: []bool{false, true}, Other: true},
		Outcome: "failed", Reason: "lost", Keys: 4, Duration: 1500 * time.Millisecond, Number: 1, Snapshot: "the pane"}
	a.At = a.At.UTC()
	if a.ID <= 0 || !equalAttempt(a, want) {
		t.Fatalf("recorded\n got %+v\nwant %+v", a, want)
	}
	b, err := p.Record(asUser(2), failedAttempt("toolu_card1"))
	if err != nil || b.Number != 2 {
		t.Fatalf("the second attempt on the card: %+v %v", b, err)
	}
	if c, err := p.Record(asUser(2), failedAttempt("toolu_card2")); err != nil || c.Number != 1 {
		t.Fatalf("another card: %+v %v", c, err)
	}
	if d, err := p.Record(asUser(3), failedAttempt("toolu_card1")); err != nil || d.Number != 1 {
		t.Fatalf("another owner's attempt on the same card id: %+v %v", d, err)
	}
	tl, err := p.ForCard(asUser(2), "toolu_card1")
	if err != nil || len(tl) != 2 || tl[0].Number != 1 || tl[1].Number != 2 {
		t.Fatalf("the card's timeline: %+v %v", tl, err)
	}
}

func equalAttempt(a, b Attempt) bool {
	if len(a.Shape.Options) != len(b.Shape.Options) || len(a.Shape.Multi) != len(b.Shape.Multi) {
		return false
	}
	for i := range a.Shape.Options {
		if a.Shape.Options[i] != b.Shape.Options[i] || a.Shape.Multi[i] != b.Shape.Multi[i] {
			return false
		}
	}
	if a.Shape.Other != b.Shape.Other {
		return false
	}
	a.Shape, b.Shape = Shape{}, Shape{}
	return reflect.DeepEqual(a, b)
}

// TestAnotherOwnerSeesNoneOfIt: owner 3 lists, reads the timeline of, and
// numbers past, none of owner 2's rows — the row-level security, not a WHERE.
func TestAnotherOwnerSeesNoneOfIt(t *testing.T) {
	p, pool := pgStore(t)
	for i := 0; i < 3; i++ {
		if _, err := p.Record(asUser(2), failedAttempt("toolu_shared")); err != nil {
			t.Fatal(err)
		}
	}
	// Control: owner 2 sees its three.
	if got, _ := p.List(asUser(2), t0.Add(-time.Hour), 0); len(got) != 3 {
		t.Fatalf("control: owner 2 lists %d", len(got))
	}
	if got, err := p.List(asUser(3), t0.Add(-time.Hour), 0); err != nil || len(got) != 0 {
		t.Fatalf("owner 3 lists %d of owner 2's rows (%v)", len(got), err)
	}
	if got, err := p.ForCard(asUser(3), "toolu_shared"); err != nil || len(got) != 0 {
		t.Fatalf("owner 3 reads owner 2's timeline: %d (%v)", len(got), err)
	}
	// A direct read under owner 3's principal, no WHERE at all.
	var n int
	if err := pool.QueryRow(asUser(3), `SELECT count(*) FROM ask_attempts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("owner 3 counts %d rows (%v)", n, err)
	}
	// And cannot write a row as owner 2.
	if _, err := pool.Exec(asUser(3), `INSERT INTO ask_attempts (owner_id, runtime, door, action, method, outcome, reason)
		VALUES (2, 'claude-code', 'card', 'submit', 'typed', 'ok', 'typed')`); err == nil {
		t.Fatal("owner 3 wrote a row as owner 2")
	}
}

// TestTheSnapshotIsRedactedAndCapped: a secret in the pane is stored
// redacted; a pane over MaxSnapshot keeps its LAST MaxSnapshot characters (the
// picker is at the bottom), redacted before the cut.
func TestTheSnapshotIsRedactedAndCapped(t *testing.T) {
	p, pool := pgStore(t)
	secret := "sk_live_" + "FAKE0000example"
	in := failedAttempt("toolu_snap")
	in.Snapshot = "top line\nstripe key " + secret + "\n❯ 1. Red"
	a, err := p.Record(asUser(2), in)
	if err != nil {
		t.Fatal(err)
	}
	if want := "top line\nstripe key «redacted»\n❯ 1. Red"; a.Snapshot != want {
		t.Fatalf("snapshot %q, want %q", a.Snapshot, want)
	}
	var stored string
	if err := pool.QueryRow(asUser(2), `SELECT snapshot FROM ask_attempts WHERE id = $1`, a.ID).Scan(&stored); err != nil || strings.Contains(stored, secret) {
		t.Fatalf("stored %q (%v)", stored, err)
	}
	big := strings.Repeat("x", MaxSnapshot) + "\nstripe key " + secret + "\nEND"
	in.Snapshot = big
	b, err := p.Record(asUser(2), in)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(b.Snapshot)); n != MaxSnapshot || !strings.HasSuffix(b.Snapshot, "\nstripe key «redacted»\nEND") || strings.Contains(b.Snapshot, secret) {
		t.Fatalf("capped snapshot: %d runes, tail %q", n, b.Snapshot[len(b.Snapshot)-40:])
	}
}

// TestPruneDeletesOnlyRowsPastTheTTL, for every owner, by age.
func TestPruneDeletesOnlyRowsPastTheTTL(t *testing.T) {
	p, _ := pgStore(t)
	old, fresh := failedAttempt("toolu_old"), failedAttempt("toolu_fresh")
	old.At, fresh.At = t0.Add(-RowTTL-time.Minute), t0.Add(-RowTTL+time.Minute)
	for _, uid := range []int64{2, 3} {
		for _, a := range []Attempt{old, fresh} {
			if _, err := p.Record(asUser(uid), a); err != nil {
				t.Fatal(err)
			}
		}
	}
	n, err := p.Prune(context.Background(), t0.Add(-RowTTL))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d (%v), want 2", n, err)
	}
	for _, uid := range []int64{2, 3} {
		got, _ := p.List(asUser(uid), t0.Add(-2*RowTTL), 0)
		if len(got) != 1 || got[0].CardID != "toolu_fresh" {
			t.Fatalf("owner %d kept %+v", uid, got)
		}
	}
}

// TestTheStoreRefusesAnUnknownReason: the one enumerated set.
func TestTheStoreRefusesAnUnknownReason(t *testing.T) {
	p, _ := pgStore(t)
	in := failedAttempt("toolu_x")
	in.Reason = "made-up"
	if _, err := p.Record(asUser(2), in); err == nil {
		t.Fatal("an unknown reason was recorded")
	}
}
