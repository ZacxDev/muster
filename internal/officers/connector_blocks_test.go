package officers_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// The connector-block record (connector_blocks.go): one contract, asserted on
// the memory store and on officer_connector_blocks (migration 0148) with a
// database — where the record also survives a new store on its own pool (a
// restarted server, or the other replica). Ids are made up.

const (
	blkOfficer = "shepherd:example-infra"
	blkGrant   = int64(4471)
)

func blockContract(t *testing.T, ctx context.Context, st officers.ConnectorBlockStore, reopen func() officers.ConnectorBlockStore) {
	t.Helper()
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	rec := func(st officers.ConnectorBlockStore, grant int64, item string, at time.Time) {
		t.Helper()
		if err := st.RecordBlock(ctx, officers.ConnectorBlock{Officer: blkOfficer, GrantID: grant, Item: officers.ItemID(item), BlockedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	items := func(st officers.ConnectorBlockStore, grant int64, since time.Time, limit int) []string {
		t.Helper()
		bs, err := st.BlocksForGrant(ctx, blkOfficer, grant, since, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range bs {
			out = append(out, string(b.Item))
		}
		return out
	}
	rec(st, blkGrant, "3702", t0)
	rec(st, blkGrant, "4030", t0.Add(2*time.Hour))
	rec(st, blkGrant, "4156", t0.Add(5*time.Hour))
	rec(st, blkGrant+1, "3980", t0.Add(3*time.Hour)) // another grant
	// a repeat refusal keeps the first time: 3702 stays the oldest
	rec(st, blkGrant, "3702", t0.Add(9*time.Hour))
	if got := items(st, blkGrant, t0, 10); len(got) != 3 || got[0] != "4156" || got[1] != "4030" || got[2] != "3702" {
		t.Fatalf("grant's blocks %v; want [4156 4030 3702] (newest first, the repeat keeping its first time, never another grant's)", got)
	}
	if got := items(st, blkGrant, t0.Add(time.Hour), 10); len(got) != 2 {
		t.Fatalf("since +1h: %v; want the two recorded after it", got)
	}
	if got := items(st, blkGrant, t0, 1); len(got) != 1 || got[0] != "4156" {
		t.Fatalf("limit 1: %v; want the newest", got)
	}
	if err := st.SettleBlock(ctx, blkOfficer, blkGrant, "4030", t0.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := items(st, blkGrant, t0, 10); len(got) != 2 || got[0] != "4156" || got[1] != "3702" {
		t.Fatalf("after settling 4030: %v; want it gone (a settled block is never read again)", got)
	}
	if got := items(st, blkGrant+1, t0, 10); len(got) != 1 || got[0] != "3980" {
		t.Fatalf("the other grant: %v", got)
	}
	if reopen != nil {
		if got := items(reopen(), blkGrant, t0, 10); len(got) != 2 {
			t.Fatalf("a new store on its own pool sees %v; want the same two", got)
		}
	}

	// UnsettledGrants: every grant with an unsettled block, ascending; a
	// grant whose blocks are all settled is not named; the limit holds.
	grants := func(limit int) []int64 {
		t.Helper()
		g, err := st.UnsettledGrants(ctx, blkOfficer, limit)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	rec(st, blkGrant+7, "5120", t0)
	if got := grants(10); len(got) != 3 || got[0] != blkGrant || got[1] != blkGrant+1 || got[2] != blkGrant+7 {
		t.Fatalf("unsettled grants %v; want [%d %d %d]", got, blkGrant, blkGrant+1, blkGrant+7)
	}
	if got := grants(1); len(got) != 1 || got[0] != blkGrant {
		t.Fatalf("limit 1: %v; want [%d]", got, blkGrant)
	}
	// SettleBlocks with a bound settles only those recorded before it (3702
	// at t0; 4156 at t0+5h stays), and says how many.
	n, err := st.SettleBlocks(ctx, blkOfficer, blkGrant, t0.Add(time.Hour), t0.Add(10*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("SettleBlocks before t0+1h = %d, %v; want 1", n, err)
	}
	if got := items(st, blkGrant, time.Time{}, 10); len(got) != 1 || got[0] != "4156" {
		t.Fatalf("after the bounded settle: %v; want [4156]", got)
	}
	// With no bound it settles every unsettled one of THAT grant only.
	if n, err := st.SettleBlocks(ctx, blkOfficer, blkGrant+1, time.Time{}, t0.Add(10*time.Hour)); err != nil || n != 1 {
		t.Fatalf("SettleBlocks of grant+1 = %d, %v; want 1", n, err)
	}
	if got := grants(10); len(got) != 2 || got[0] != blkGrant || got[1] != blkGrant+7 {
		t.Fatalf("unsettled grants after settling grant+1: %v; want [%d %d]", got, blkGrant, blkGrant+7)
	}
	// A repeat settles nothing more.
	if n, err := st.SettleBlocks(ctx, blkOfficer, blkGrant+1, time.Time{}, t0.Add(11*time.Hour)); err != nil || n != 0 {
		t.Fatalf("repeat SettleBlocks = %d, %v; want 0", n, err)
	}
}

func TestTheMemoryBlockStoreKeepsTheContract(t *testing.T) {
	blockContract(t, context.Background(), officers.NewMemConnectorBlockStore(), nil)
}

func TestTheDatabaseBlockStoreKeepsTheContract(t *testing.T) {
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE officer_connector_blocks RESTART IDENTITY`)
	open := func() officers.ConnectorBlockStore {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		pool, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return officers.NewPGConnectorBlockStore(pool)
	}
	blockContract(t, dbtest.AsOperator(context.Background()), open(), open)
}

// TestRerunItemSpeaksHQsRoute: the client posts {key, reason} to the item's
// rerun route with the officer token and reads HQ's answer; an HQ without
// the route is a 404 *HQError the caller can tell.
func TestRerunItemSpeaksHQsRoute(t *testing.T) {
	const tok = "fixture-officer-token-blk-91ad"
	hq := hqtest.New(map[string]string{tok: blkOfficer})
	t.Cleanup(hq.Close)
	src := hq.AddItem(blkOfficer, "shepherd.opened", "example-org/example-infra#3615")
	hq.Complete(src)
	c := officers.NewClient(officers.Officer{ID: blkOfficer, HQURL: hq.BaseURL()}.WithToken(tok), nil)
	rb, err := officers.Serves[officers.RerunBackend](c, officers.CapRerun)
	if err != nil {
		t.Fatalf("HQ's client does not serve rerun: %v", err)
	}
	req := officers.RerunRequest{Key: officers.GrantRerunKey(blkGrant), Reason: "grant approved"}
	a, err := rb.RerunItem(context.Background(), officers.ItemID(src), req)
	if err != nil || a.Rerun != "created" || a.Item == "" || a.Item == officers.ItemID(src) {
		t.Fatalf("first rerun = %+v, %v; want created, a new item", a, err)
	}
	if got := hq.Reruns(); len(got) != 1 || got[0].Key != "connector-grant:4471" || got[0].Reason != "grant approved" || got[0].Item != src {
		t.Fatalf("HQ got %+v", got)
	}
	again, err := rb.RerunItem(context.Background(), officers.ItemID(src), req)
	if err != nil || again.Rerun != "already" || again.Item != a.Item {
		t.Fatalf("second rerun = %+v, %v; want already, item %s", again, err, a.Item)
	}
	hq.RerunAbsent = true
	_, err = rb.RerunItem(context.Background(), officers.ItemID(src), req)
	var he *officers.HQError
	if !errors.As(err, &he) || he.Status != http.StatusNotFound {
		t.Fatalf("an HQ without the route: %v; want a 404 *HQError", err)
	}
}
