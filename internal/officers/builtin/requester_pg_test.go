package builtin_test

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/officers/backendtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// TestAnInheritedRequesterNeverReplacesOne: inheritRequesterTx copies an
// item's requester onto an item naming none (the positive control), and
// leaves an item that already names a session as it is (FIRST REQUESTER
// WINS), whatever the source names.
func TestAnInheritedRequesterNeverReplacesOne(t *testing.T) {
	w := newPGWorld(t, false)
	ctx := ctxOp()
	const sidA, sidB = "5e55a1d0-0000-4000-8000-0000000000a1", "5e55a1d0-0000-4000-8000-0000000000b2"
	item := func(subject, host, sid string) int64 {
		t.Helper()
		id, _, err := w.store.Enqueue(ctx, 1, builtin.NewItem{Officer: backendtest.OfficerA, Kind: "owner.plan", SubjectKey: subject}, w.clock())
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			if ok, err := w.store.SetItemRequester(ctx, 1, id, host, sid); err != nil || !ok {
				t.Fatalf("set requester: %v %v", ok, err)
			}
		}
		return id
	}
	from, named, empty := item("req-from", "tower", sidA), item("req-named", "laptop", sidB), item("req-empty", "", "")
	inherit := func(to int64) {
		t.Helper()
		tx, err := w.store.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if err := builtin.InheritRequesterTx(ctx, tx, 1, backendtest.OfficerA, from, to); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	inherit(empty)
	inherit(named)
	if h, s, err := w.store.ItemRequester(ctx, 1, empty); err != nil || h != "tower" || s != sidA {
		t.Fatalf("an item naming no session: %q/%q (%v); want tower/%s inherited", h, s, err, sidA)
	}
	if h, s, err := w.store.ItemRequester(ctx, 1, named); err != nil || h != "laptop" || s != sidB {
		t.Fatalf("an item already naming a session: %q/%q (%v); want laptop/%s kept", h, s, err, sidB)
	}
}
