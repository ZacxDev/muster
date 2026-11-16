package officers_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// A draft officer's public branches (public_branches.go): one contract,
// asserted on the memory store and on officer_public_branches (migration
// 0138) with a database — where the record also survives a new store on its
// own pool (a restarted server, or the other replica).

const (
	pbOfficerA = "draft:example-a"
	pbOfficerB = "draft:example-b"
	pbRepo     = "example-org/example-infra"
	pbFirst    = "officer/fix/raise-limit-abcdefghijkl"
	pbSecond   = "officer/fix/other-words-mnopqrstuvwx"
	pbTheirs   = "officer/fix/their-words-qzvrwmtkpjhd"
)

func publicBranchContract(t *testing.T, ctx context.Context, st officers.PublicBranchStore, reopen func() officers.PublicBranchStore) {
	t.Helper()
	item := func(st officers.PublicBranchStore, officer, repo, branch string) (int, bool) {
		t.Helper()
		n, ok, err := st.PublicBranchItem(ctx, officer, repo, branch)
		if err != nil {
			t.Fatal(err)
		}
		return n, ok
	}
	if _, ok := item(st, pbOfficerA, pbRepo, pbFirst); ok {
		t.Fatal("never minted, yet recorded")
	}
	got, err := st.PublicBranchMint(ctx, pbOfficerA, "Example-Org/Example-Infra", 7, pbFirst)
	if err != nil || got != pbFirst {
		t.Fatalf("first mint: %q, %v; want %q", got, err, pbFirst)
	}
	// The same (officer, repository, item) again — the repository in another
	// case — keeps the first row and answers it (the database store: ON
	// CONFLICT DO NOTHING, then the re-select).
	got, err = st.PublicBranchMint(ctx, pbOfficerA, pbRepo, 7, pbSecond)
	if err != nil || got != pbFirst {
		t.Fatalf("a second mint for item 7: %q, %v; want the first branch %q", got, err, pbFirst)
	}
	if _, ok := item(st, pbOfficerA, pbRepo, pbSecond); ok {
		t.Fatalf("the losing name %q was recorded", pbSecond)
	}
	// Read back without case on the repository.
	if n, ok := item(st, pbOfficerA, "EXAMPLE-ORG/example-infra", pbFirst); !ok || n != 7 {
		t.Fatalf("item of %q (repository upper-cased): %d %v, want 7", pbFirst, n, ok)
	}
	// One name, one item: the recorded name for another item is refused.
	if _, err := st.PublicBranchMint(ctx, pbOfficerA, pbRepo, 8, pbFirst); err == nil {
		t.Fatalf("%q recorded for item 8 as well as item 7", pbFirst)
	}
	// Officer isolation: B neither reads A's branch nor gets it from a mint
	// for the same repository and item — B's mint records B's own.
	if _, ok := item(st, pbOfficerB, pbRepo, pbFirst); ok {
		t.Fatalf("officer B reads officer A's branch %q", pbFirst)
	}
	if l, err := st.PublicBranches(ctx, pbOfficerB, pbRepo); err != nil || len(l) != 0 {
		t.Fatalf("officer B's branches before any mint: %v %v, want none", l, err)
	}
	got, err = st.PublicBranchMint(ctx, pbOfficerB, pbRepo, 7, pbTheirs)
	if err != nil || got != pbTheirs {
		t.Fatalf("officer B's mint for item 7: %q, %v; want its own %q (not A's)", got, err, pbTheirs)
	}
	if n, ok := item(st, pbOfficerA, pbRepo, pbTheirs); ok {
		t.Fatalf("officer A reads officer B's branch as item %d", n)
	}
	if l, err := st.PublicBranches(ctx, pbOfficerA, "EXAMPLE-ORG/EXAMPLE-INFRA"); err != nil || len(l) != 1 || l[0] != pbFirst {
		t.Fatalf("officer A's branches: %v %v, want only %q", l, err, pbFirst)
	}
	if l, _ := st.PublicBranches(ctx, pbOfficerA, "example-org/other"); len(l) != 0 {
		t.Fatalf("another repository's branches: %v, want none", l)
	}
	if reopen != nil {
		again := reopen()
		if n, ok := item(again, pbOfficerA, pbRepo, pbFirst); !ok || n != 7 {
			t.Fatalf("a new store reads %q as %d %v, want item 7 (the record survives a restart)", pbFirst, n, ok)
		}
		if got, err := again.PublicBranchMint(ctx, pbOfficerA, pbRepo, 7, pbSecond); err != nil || got != pbFirst {
			t.Fatalf("a new store's mint for item 7: %q, %v; want %q", got, err, pbFirst)
		}
	}
}

func TestTheMemoryPublicBranchStoreKeepsTheContract(t *testing.T) {
	publicBranchContract(t, context.Background(), officers.NewMemPublicBranchStore(), nil)
}

func TestTheDatabasePublicBranchStoreKeepsTheContract(t *testing.T) {
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE officer_public_branches RESTART IDENTITY`)
	open := func() officers.PublicBranchStore {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		pool, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return officers.NewPGPublicBranchStore(pool)
	}
	publicBranchContract(t, dbtest.AsOperator(context.Background()), open(), open)
}
