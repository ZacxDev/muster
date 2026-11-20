package pages

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// assetID is a 22-character public id, distinct per n.
func assetID(n int) string { return fmt.Sprintf("asset%017d", n) }

func addAsset(t *testing.T, s *PGStore, uid int64, c Creator, page int64, n int, size int64, ct string) Asset {
	t.Helper()
	a, err := s.AddAsset(asUser(uid), c, page, Asset{PublicID: assetID(n), Filename: fmt.Sprintf("f%d", n), ContentType: ct, Size: size})
	if err != nil {
		t.Fatalf("add asset %d: %v", n, err)
	}
	return a
}

func mdBlocks(t *testing.T, text string) ([]Block, int) {
	t.Helper()
	return blocksOf(t, `[{"type":"markdown","text":`+fmt.Sprintf("%q", text)+`}]`)
}

// TestAnAssetIsItsPagesOwnersAndCreators: an asset is readable and addable
// only by its page's owner — another owner reads none and adds none — and
// through the "…For" methods only by the page's creator (another creator of
// the same owner reads it as missing).
func TestAnAssetIsItsPagesOwnersAndCreators(t *testing.T) {
	s, _ := pgStore(t)
	two, three := asUser(2), asUser(3)
	b, n := blocksOf(t, qBlocks)
	p, _, err := s.Create(two, agentA, "Media", b, n)
	if err != nil {
		t.Fatal(err)
	}
	x := addAsset(t, s, 2, agentA, p.ID, 1, 10, "image/png")
	if x.PageID != p.ID || x.Ref() != "asset:"+assetID(1) {
		t.Fatalf("added %+v", x)
	}
	// Control: the owner and creator read it.
	if _, err := s.AssetFor(two, agentA, p.ID, x.PublicID); err != nil {
		t.Fatalf("control: the creator reads its asset: %v", err)
	}
	if list, err := s.Assets(two, p.ID); err != nil || len(list) != 1 {
		t.Fatalf("control: the owner lists %d assets (%v)", len(list), err)
	}
	// Another owner: nothing, and adds nothing.
	if _, err := s.AssetFor(three, agentA, p.ID, x.PublicID); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 read user 2's asset: %v", err)
	}
	if list, _ := s.Assets(three, p.ID); len(list) != 0 {
		t.Errorf("user 3 lists %d of user 2's assets", len(list))
	}
	if _, err := s.AddAsset(three, agentA, p.ID, Asset{PublicID: assetID(2), ContentType: "image/png", Size: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 added an asset to user 2's page: %v", err)
	}
	if _, err := s.AssetRoom(three, agentA, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 read user 2's page's room: %v", err)
	}
	// Another creator of the same owner: missing.
	if _, err := s.AssetFor(two, agentB, p.ID, x.PublicID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another creator read the asset: %v", err)
	}
	if _, err := s.AddAsset(two, agentB, p.ID, Asset{PublicID: assetID(3), ContentType: "image/png", Size: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another creator added an asset: %v", err)
	}
	if _, err := s.DeleteAsset(two, agentB, p.ID, x.PublicID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another creator deleted the asset: %v", err)
	}
	// A type that is not one of the six is refused before the row.
	if _, err := s.AddAsset(two, agentA, p.ID, Asset{PublicID: assetID(4), ContentType: "text/html", Size: 1}); !errors.Is(err, ErrAssetType) {
		t.Errorf("an html asset was recorded: %v", err)
	}
}

// TestAVersionRefersOnlyToItsOwnPagesAssets: a version naming another page's
// asset (the same owner's), or an asset that does not exist, is refused with
// a sentence naming it; one naming its own page's is recorded and serves it,
// while the other page's version does not serve it. A new page has no assets,
// so page_create naming one is refused. And the foreign key holds it too: a
// reference written straight to the table, past the store, to another page's
// asset is refused by Postgres.
func TestAVersionRefersOnlyToItsOwnPagesAssets(t *testing.T) {
	s, pool := pgStore(t)
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, err := s.Create(two, agentA, "P", b, n)
	if err != nil {
		t.Fatal(err)
	}
	q, _, err := s.Create(two, agentA, "Q", b, n)
	if err != nil {
		t.Fatal(err)
	}
	x := addAsset(t, s, 2, agentA, p.ID, 1, 10, "image/png")
	y := addAsset(t, s, 2, agentA, q.ID, 2, 10, "video/mp4")

	bad, bn := mdBlocks(t, "Look: ![shot]("+y.Ref()+")")
	if _, _, err := s.AddVersion(two, agentA, p.ID, "P", bad, bn); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), y.Ref()+" is not an asset of this page") {
		t.Fatalf("a version of P naming Q's asset: %v", err)
	}
	missing, mn := mdBlocks(t, "![gone](asset:"+assetID(99)+")")
	if _, _, err := s.AddVersion(two, agentA, p.ID, "P", missing, mn); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a version naming no asset at all: %v", err)
	}
	if _, _, err := s.Create(two, agentA, "new", missing, mn); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a new page naming an asset: %v", err)
	}
	good, gn := blocksOf(t, `[{"type":"html","html":"<img src=\"`+x.Ref()+`\">"}]`)
	_, v, err := s.AddVersion(two, agentA, p.ID, "P", good, gn)
	if err != nil {
		t.Fatalf("a version naming its own page's asset: %v", err)
	}
	if got, err := s.VersionAsset(two, p.ID, v.Version, x.PublicID); err != nil || got.PublicID != x.PublicID {
		t.Fatalf("P's version does not serve its asset: %+v %v", got, err)
	}
	if list, err := s.VersionAssets(two, p.ID, 0); err != nil || len(list) != 1 || list[0].PublicID != x.PublicID {
		t.Fatalf("P's current version's assets: %+v %v", list, err)
	}
	// The other page, and the other page's asset through this one: missing.
	if _, err := s.VersionAsset(two, q.ID, 0, x.PublicID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Q serves P's asset: %v", err)
	}
	if _, err := s.VersionAsset(two, p.ID, v.Version, y.PublicID); !errors.Is(err, ErrNotFound) {
		t.Errorf("P serves Q's asset: %v", err)
	}
	if _, err := s.VersionAsset(asUser(3), p.ID, v.Version, x.PublicID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another owner reads P's asset through its version: %v", err)
	}
	// The key, past the store's check: a reference from P's version to Q's
	// asset, written as P's owner.
	_, err = pool.Exec(two, `INSERT INTO page_version_assets (page_id, version, asset_id) VALUES ($1, $2, $3)`, p.ID, v.Version, y.PublicID)
	if pe := (*pgconn.PgError)(nil); !errors.As(err, &pe) || pe.Code != "23503" || pe.ConstraintName != "page_version_assets_asset_fkey" {
		t.Fatalf("a reference to another page's asset written past the store: %v", err)
	}
	// Control: the same statement naming P's own asset in a version that does
	// not have it yet is accepted (so the refusal above is the key's).
	if _, err := pool.Exec(two, `INSERT INTO page_version_assets (page_id, version, asset_id) VALUES ($1, 1, $2)`, p.ID, x.PublicID); err != nil {
		t.Fatalf("control: a reference to P's own asset: %v", err)
	}
}

// TestAVersionKeepsItsAssetsAndTheirPageTakesThem: an asset an OLD version
// refers to is still served for that version once a newer version (which does
// not use it) is current, and cannot be deleted; an asset no version uses can
// be; Files lists every asset with the page's viewers' files; deleting the
// page removes every asset row.
func TestAVersionKeepsItsAssetsAndTheirPageTakesThem(t *testing.T) {
	s, pool := pgStore(t)
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, err := s.Create(two, hostW, "Review", b, n)
	if err != nil {
		t.Fatal(err)
	}
	x := addAsset(t, s, 2, hostW, p.ID, 1, 10, "image/jpeg")
	z := addAsset(t, s, 2, hostW, p.ID, 2, 10, "video/webm")
	with, wn := mdBlocks(t, "![old]("+x.Ref()+")")
	_, v2, err := s.AddVersion(two, hostW, p.ID, "Review", with, wn)
	if err != nil {
		t.Fatal(err)
	}
	without, won := mdBlocks(t, "no media now")
	if _, _, err := s.AddVersion(two, hostW, p.ID, "Review", without, won); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VersionAsset(two, p.ID, v2.Version, x.PublicID); err != nil {
		t.Fatalf("version %d's asset is gone once a newer version is current: %v", v2.Version, err)
	}
	if _, err := s.VersionAsset(two, p.ID, 0, x.PublicID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the current version (which does not use it) serves it: %v", err)
	}
	if _, err := s.DeleteAsset(two, hostW, p.ID, x.PublicID); !errors.Is(err, ErrAssetInUse) {
		t.Fatalf("an asset an old version uses was deleted: %v", err)
	}
	if _, err := s.VersionAsset(two, p.ID, v2.Version, x.PublicID); err != nil {
		t.Fatalf("the refused delete took the asset anyway: %v", err)
	}
	if got, err := s.DeleteAsset(two, hostW, p.ID, z.PublicID); err != nil || got.PublicID != z.PublicID {
		t.Fatalf("an asset no version uses was not deleted: %+v %v", got, err)
	}
	files, err := s.Files(two, p.ID)
	if err != nil || len(files) != 1 || files[0] != x.PublicID {
		t.Fatalf("the page's files are %v (%v), want its one asset", files, err)
	}
	if _, err := s.Delete(two, p.ID); err != nil {
		t.Fatalf("deleting a page whose versions use an asset: %v", err)
	}
	var left int
	if err := pool.QueryRow(two, `SELECT (SELECT count(*) FROM page_assets WHERE page_id = $1) + (SELECT count(*) FROM page_version_assets WHERE page_id = $1)`, p.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("deleting the page left %d asset rows", left)
	}
}

// TestAPagesAssetsAreCappedUnderItsLock: AddAsset counts the page's total
// WITH the new asset under the page row's lock: six 15 MiB assets (90 MiB),
// then an 11 MiB one overshoots 100 MiB and is refused; a 10 MiB one lands it
// exactly on 100 MiB and is taken; after that one byte is refused. AssetRoom
// reports what is left.
func TestAPagesAssetsAreCappedUnderItsLock(t *testing.T) {
	s, _ := pgStore(t)
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, err := s.Create(two, agentA, "Big", b, n)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		addAsset(t, s, 2, agentA, p.ID, i, MaxAssetBytes, "video/mp4")
	}
	if room, err := s.AssetRoom(two, agentA, p.ID); err != nil || room != 10<<20 {
		t.Fatalf("room after 90 MiB: %d %v", room, err)
	}
	if _, err := s.AddAsset(two, agentA, p.ID, Asset{PublicID: assetID(7), ContentType: "video/mp4", Size: 11 << 20}); !errors.Is(err, ErrTooMany) || !strings.Contains(err.Error(), "limited to 100 MiB") {
		t.Fatalf("11 MiB onto 90: %v", err)
	}
	addAsset(t, s, 2, agentA, p.ID, 8, 10<<20, "video/mp4")
	if _, err := s.AddAsset(two, agentA, p.ID, Asset{PublicID: assetID(9), ContentType: "image/png", Size: 1}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("one byte onto 100 MiB: %v", err)
	}
	if room, err := s.AssetRoom(two, agentA, p.ID); err != nil || room != 0 {
		t.Fatalf("room at 100 MiB: %d %v", room, err)
	}
}
