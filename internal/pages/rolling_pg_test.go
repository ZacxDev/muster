package pages

import (
	"errors"
	"strconv"
	"testing"
)

// A ROLLING page (Creator.RollingVersions, migration 0166: a scheduled host
// job's own page) keeps its newest MaxVersionsPerPage versions, dropping the
// oldest one nothing references; every other page keeps the cap.

func markdownBlocks(t *testing.T, text string) ([]Block, int) {
	t.Helper()
	b, n, err := Validate([]Block{{Type: BlockMarkdown, Text: text}})
	if err != nil {
		t.Fatal(err)
	}
	return b, n
}

// TestARollingPageTakes105VersionsAndHoldsTheNewest100: 105 versions written
// to a rolling page all succeed; it holds exactly the newest 100 (6..105),
// its id never changes, and the current one is the last written. A plain page
// of the same creator, at the real cap, refuses its 101st.
func TestARollingPageTakes105VersionsAndHoldsTheNewest100(t *testing.T) {
	s, _ := pgStore(t)
	two := asUser(2)
	job := Creator{MirrorHost: "workstation", Label: "job report on workstation", RollingVersions: true}
	b, n := markdownBlocks(t, "run 1")
	p, _, err := s.Create(two, job, "Report", b, n)
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 105; i++ {
		b, n := markdownBlocks(t, "run "+strconv.Itoa(i))
		got, v, err := s.AddVersion(two, job, p.ID, "Report", b, n)
		if err != nil {
			t.Fatalf("version %d of a rolling page: %v", i, err)
		}
		if got.ID != p.ID || v.Version != i || got.CurrentVersion != i {
			t.Fatalf("version %d: page %d, version %d, current %d; want page %d, %d, %d", i, got.ID, v.Version, got.CurrentVersion, p.ID, i, i)
		}
	}
	vs, err := s.Versions(two, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != MaxVersionsPerPage || vs[0].Version != 105 || vs[len(vs)-1].Version != 6 {
		t.Fatalf("a rolling page holds %d versions, %d..%d; want %d, 6..105", len(vs), vs[len(vs)-1].Version, vs[0].Version, MaxVersionsPerPage)
	}
	if cur, _ := s.Version(two, p.ID, 0); cur.Version != 105 || cur.Blocks[0].Text != "run 105" {
		t.Fatalf("current %d %q", cur.Version, cur.Blocks[0].Text)
	}

	plain, _, err := s.Create(two, hostW, "Plain", b, n)
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= MaxVersionsPerPage; i++ {
		if _, _, err := s.AddVersion(two, hostW, plain.ID, "Plain", b, n); err != nil {
			t.Fatalf("version %d of a plain page: %v", i, err)
		}
	}
	if _, _, err := s.AddVersion(two, hostW, plain.ID, "Plain", b, n); !errors.Is(err, ErrTooMany) {
		t.Fatalf("a plain page's version %d: %v; want ErrTooMany", MaxVersionsPerPage+1, err)
	}
	if vs, _ := s.Versions(two, plain.ID); len(vs) != MaxVersionsPerPage {
		t.Fatalf("the plain page holds %d versions; want %d, none dropped", len(vs), MaxVersionsPerPage)
	}
}

// TestARollingPageNeverDropsAReferencedVersion: the current version (what
// every live link serves) and a version a response names are kept, the
// oldest unreferenced one goes instead; and with every older version
// referenced, a new one is refused rather than dropping one of them.
func TestARollingPageNeverDropsAReferencedVersion(t *testing.T) {
	defer func(v int) { MaxVersionsPerPage = v }(MaxVersionsPerPage)
	MaxVersionsPerPage = 3
	s, _ := pgStore(t)
	two := asUser(2)
	job := Creator{MirrorHost: "workstation", Label: "job", RollingVersions: true}
	b, n := blocksOf(t, `[{"type":"markdown","text":"x"},{"type":"question","id":"q1","label":"Ok?"}]`)
	p, _, err := s.Create(two, job, "R", b, n)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.CreateLink(two, p.ID, LinkLive, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResponse(two, l, 1, "q1", KindSubmit, []byte(`{"answer":"yes"}`), ""); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 3; i++ {
		if _, _, err := s.AddVersion(two, job, p.ID, "R", b, n); err != nil {
			t.Fatal(err)
		}
	}
	// Holds 1 (a response names it), 2, 3 (current). Version 4 drops 2.
	if _, _, err := s.AddVersion(two, job, p.ID, "R", b, n); err != nil {
		t.Fatal(err)
	}
	if got := versionNumbers(t, s, p.ID); got != "4,3,1" {
		t.Fatalf("held %s; want 4,3,1 (2 dropped: the oldest unreferenced; 1 has a response)", got)
	}
	// Roll back to 3 (current), then write 5: 4 is the only unreferenced one.
	if _, err := s.SetCurrent(two, p.ID, 3); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddVersion(two, job, p.ID, "R", b, n); err != nil {
		t.Fatal(err)
	}
	if got := versionNumbers(t, s, p.ID); got != "5,3,1" {
		t.Fatalf("held %s; want 5,3,1 (4 dropped; 3 was current when 5 was written; 1 has a response)", got)
	}
	// Responses at 3 and at 5 (current): every held version is referenced.
	for _, v := range []int{3, 5} {
		if _, err := s.AddResponse(two, l, v, "q1", KindSubmit, []byte(`{"answer":"yes"}`), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.AddVersion(two, job, p.ID, "R", b, n); !errors.Is(err, ErrTooMany) {
		t.Fatalf("every held version referenced: %v; want ErrTooMany", err)
	}
	if got := versionNumbers(t, s, p.ID); got != "5,3,1" {
		t.Fatalf("a refused version dropped one: held %s", got)
	}
}

// TestARollingPageDropsAVersionThatUsesAnAsset: a version that uses one of
// the page's assets (0164's page_version_assets) is dropped like any other
// unreferenced version, and its asset references go with it (0164's
// cascade from page_versions), so the drop is not refused by the reference.
// The ASSET stays the page's: it is still listed and its bytes are still
// served from the page's prefix until it is deleted, which, no version
// using it now, is allowed; an asset a version still held uses stays
// served and in use. Deleting the page leaves no asset rows.
func TestARollingPageDropsAVersionThatUsesAnAsset(t *testing.T) {
	defer func(v int) { MaxVersionsPerPage = v }(MaxVersionsPerPage)
	MaxVersionsPerPage = 4
	s, pool := pgStore(t)
	two := asUser(2)
	job := Creator{MirrorHost: "workstation", Label: "job", RollingVersions: true}
	plain, pn := mdBlocks(t, "no media")
	p, _, err := s.Create(two, job, "R", plain, pn)
	if err != nil {
		t.Fatal(err)
	}
	x := addAsset(t, s, 2, job, p.ID, 1, 10, "image/png")
	y := addAsset(t, s, 2, job, p.ID, 2, 20, "video/mp4")
	withX, xn := mdBlocks(t, "![x]("+x.Ref()+")")
	withY, yn := mdBlocks(t, "![y]("+y.Ref()+")")
	for _, w := range []struct {
		b []Block
		n int
	}{{withX, xn}, {withY, yn}, {plain, pn}} {
		if _, _, err := s.AddVersion(two, job, p.ID, "R", w.b, w.n); err != nil {
			t.Fatal(err)
		}
	}
	// Holds 4 (current), 3 (uses y), 2 (uses x), 1. Version 5 drops 1;
	// version 6 drops 2, the version that uses x.
	for i := 5; i <= 6; i++ {
		if _, _, err := s.AddVersion(two, job, p.ID, "R", plain, pn); err != nil {
			t.Fatalf("version %d of a rolling page whose oldest unreferenced version uses an asset: %v", i, err)
		}
	}
	if got := versionNumbers(t, s, p.ID); got != "6,5,4,3" {
		t.Fatalf("held %s; want 6,5,4,3 (1, then 2, dropped)", got)
	}
	refs := func(asset string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(two, `SELECT count(*) FROM page_version_assets WHERE page_id = $1 AND asset_id = $2`, p.ID, asset).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := refs(x.PublicID); n != 0 {
		t.Fatalf("dropped version 2 left %d references to its asset", n)
	}
	if n := refs(y.PublicID); n != 1 {
		t.Fatalf("held version 3 has %d references to its asset; want 1", n)
	}
	if _, err := s.VersionAsset(two, p.ID, 3, y.PublicID); err != nil {
		t.Fatalf("held version 3 no longer serves its asset: %v", err)
	}
	if list, err := s.Assets(two, p.ID); err != nil || len(list) != 2 {
		t.Fatalf("the page lists %d assets (%v); want both: a dropped version takes no asset", len(list), err)
	}
	if _, err := s.DeleteAsset(two, job, p.ID, y.PublicID); !errors.Is(err, ErrAssetInUse) {
		t.Fatalf("the asset held version 3 uses was deleted: %v", err)
	}
	if got, err := s.DeleteAsset(two, job, p.ID, x.PublicID); err != nil || got.PublicID != x.PublicID {
		t.Fatalf("the asset only the dropped version used could not be deleted: %+v %v", got, err)
	}
	if _, err := s.Delete(two, p.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(two, `SELECT (SELECT count(*) FROM page_assets WHERE page_id = $1) + (SELECT count(*) FROM page_version_assets WHERE page_id = $1)`, p.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("deleting the rolling page left %d asset rows", left)
	}
}

func versionNumbers(t *testing.T, s *PGStore, id int64) string {
	t.Helper()
	vs, err := s.Versions(asUser(2), id)
	if err != nil {
		t.Fatal(err)
	}
	out := ""
	for i, v := range vs {
		if i > 0 {
			out += ","
		}
		out += strconv.Itoa(v.Version)
	}
	return out
}
