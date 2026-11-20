package officers_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// A draft officer's pull-request queue (pr_queue.go): one contract, asserted
// on the memory store and on officer_pr_queue (migration 0168) with a
// database — where the queue also survives a new store on its own pool (a
// restarted server, or the other replica).
//
// The branches are queued in the order Z, A, M, which is neither the order of
// their names nor of their items, so a list sorted by either is caught.

const (
	pqOfficerA = "draft:example-a"
	pqOfficerB = "draft:example-b"
	pqRepo     = "example-org/example-infra"
	pqOther    = "example-org/example-web"
	pqZ        = "bot/example-a/31-zeta"
	pqA        = "bot/example-a/52-alpha"
	pqM        = "bot/example-a/17-mu"
	pqSha1     = "1111111111111111111111111111111111111111"
	pqSha2     = "2222222222222222222222222222222222222222"
	pqSha3     = "3333333333333333333333333333333333333333"
	pqSha4     = "4444444444444444444444444444444444444444"
)

func branchesOf(l []officers.QueuedPR) string {
	var out []string
	for _, q := range l {
		out = append(out, q.Branch)
	}
	return strings.Join(out, ",")
}

func prQueueContract(t *testing.T, ctx context.Context, st officers.PRQueueStore, pause func(), reopen func() officers.PRQueueStore) {
	t.Helper()
	list := func(st officers.PRQueueStore, officer, repo string) []officers.QueuedPR {
		t.Helper()
		l, err := st.PRQueueList(ctx, officer, repo)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	add := func(branch, sha string, item int, title string) officers.QueuedPR {
		t.Helper()
		q, err := st.PRQueueAdd(ctx, pqOfficerA, officers.QueuedPR{Repo: "Example-Org/Example-Infra", Branch: branch, HeadSHA: sha, Title: title,
			Body: "body of " + branch, Item: item, Instance: "officer-example-a-1"})
		if err != nil {
			t.Fatal(err)
		}
		pause()
		return q
	}
	if l := list(st, pqOfficerA, pqRepo); len(l) != 0 {
		t.Fatalf("an empty queue lists %v", l)
	}
	z := add(pqZ, pqSha1, 31, "Zeta")
	add(pqA, pqSha2, 52, "Alpha")
	add(pqM, pqSha3, 17, "Mu")
	if z.State != officers.PRQueueQueued || z.Repo != pqRepo || z.HeadSHA != pqSha1 || z.Item != 31 || z.Title != "Zeta" || z.Instance != "officer-example-a-1" {
		t.Fatalf("the first row = %+v", z)
	}
	if got := branchesOf(list(st, pqOfficerA, "EXAMPLE-ORG/example-infra")); got != pqZ+","+pqA+","+pqM {
		t.Fatalf("the queue lists %s, want first-queued first: %s,%s,%s", got, pqZ, pqA, pqM)
	}
	// A second create from a queued branch refreshes it and keeps its place.
	again := add(pqZ, pqSha4, 31, "Zeta again")
	l := list(st, pqOfficerA, pqRepo)
	if branchesOf(l) != pqZ+","+pqA+","+pqM || again.ID != z.ID || l[0].HeadSHA != pqSha4 || l[0].Title != "Zeta again" {
		t.Fatalf("after refreshing %s: %s, first %+v; want it first, at the new head", pqZ, branchesOf(l), l[0])
	}
	// The settle is conditional on the head it was decided on.
	if ok, err := st.PRQueueSettle(ctx, pqOfficerA, z.ID, pqSha1, officers.PRQueueOpened, 7, ""); err != nil || ok {
		t.Fatalf("a settle at the OLD head: %v %v, want nothing settled", ok, err)
	}
	if ok, err := st.PRQueueSettle(ctx, pqOfficerA, z.ID, pqSha4, officers.PRQueueOpened, 7, ""); err != nil || !ok {
		t.Fatalf("a settle at the current head: %v %v", ok, err)
	}
	if ok, _ := st.PRQueueSettle(ctx, pqOfficerA, z.ID, pqSha4, officers.PRQueueSkipped, 0, "twice"); ok {
		t.Fatal("a settled row settled again")
	}
	if got := branchesOf(list(st, pqOfficerA, pqRepo)); got != pqA+","+pqM {
		t.Fatalf("after opening %s the queue lists %s", pqZ, got)
	}
	// A create from a settled branch queues it again, at the BACK.
	add(pqZ, pqSha1, 31, "Zeta, third")
	if got := branchesOf(list(st, pqOfficerA, pqRepo)); got != pqA+","+pqM+","+pqZ {
		t.Fatalf("a settled branch queued again: %s, want it last", got)
	}
	// Officer and repository isolation.
	if l := list(st, pqOfficerB, pqRepo); len(l) != 0 {
		t.Fatalf("officer B reads officer A's queue: %v", branchesOf(l))
	}
	if l := list(st, pqOfficerA, pqOther); len(l) != 0 {
		t.Fatalf("another repository's queue: %v", branchesOf(l))
	}
	if got := branchesOf(list(st, pqOfficerA, "")); got != pqA+","+pqM+","+pqZ {
		t.Fatalf("every repository's queue: %s", got)
	}
	cur := list(st, pqOfficerA, pqRepo)
	if ok, _ := st.PRQueueSettle(ctx, pqOfficerB, cur[0].ID, cur[0].HeadSHA, officers.PRQueueSkipped, 0, "not mine"); ok {
		t.Fatal("officer B settled officer A's row")
	}
	if _, err := st.PRQueueSettle(ctx, pqOfficerA, cur[0].ID, cur[0].HeadSHA, "queued", 0, ""); err == nil {
		t.Fatal("settled to a state that is not opened or skipped")
	}
	if reopen != nil {
		// 🔴 A RESTART: a new store on its own pool reads the same queue, in
		// the same order, and settles it.
		st2 := reopen()
		l := list(st2, pqOfficerA, pqRepo)
		if branchesOf(l) != pqA+","+pqM+","+pqZ || l[0].Body != "body of "+pqA || l[0].Item != 52 || l[0].HeadSHA != pqSha2 {
			t.Fatalf("after a restart the queue lists %s (first %+v), want %s,%s,%s", branchesOf(l), l[0], pqA, pqM, pqZ)
		}
		if ok, err := st2.PRQueueSettle(ctx, pqOfficerA, l[0].ID, pqSha2, officers.PRQueueSkipped, 0, strings.Repeat("é", 900)); err != nil || !ok {
			t.Fatalf("a restarted store's settle (an over-long note): %v %v", ok, err)
		}
		if got := branchesOf(list(st2, pqOfficerA, pqRepo)); got != pqM+","+pqZ {
			t.Fatalf("after the restarted store skipped %s: %s", pqA, got)
		}
	}
}

func TestTheMemoryPRQueueStoreKeepsTheContract(t *testing.T) {
	prQueueContract(t, context.Background(), officers.NewMemPRQueueStore(), func() {}, nil)
}

func TestTheDatabasePRQueueStoreKeepsTheContract(t *testing.T) {
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE officer_pr_queue RESTART IDENTITY`)
	open := func() officers.PRQueueStore {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		pool, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return officers.NewPGPRQueueStore(pool)
	}
	// queued_at is now(): a pause keeps each add's time distinct, so the order
	// asserted is queued_at's and not only the id's tie-break.
	prQueueContract(t, dbtest.AsOperator(context.Background()), open(), func() { time.Sleep(5 * time.Millisecond) }, open)
}
