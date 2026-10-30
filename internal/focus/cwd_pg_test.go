package focus

import "testing"

// The stored working directory (migration 0063) against a real Postgres: a
// tail naming one stores it cleaned; a tail naming none keeps it; a named,
// malformed one clears it; another owner never reads it. Fixture paths are
// invented placeholders.
func TestUpsertStoresKeepsAndClearsTheDirectory(t *testing.T) {
	s, _ := pgStore(t)
	sub := MirrorSubject("0c1d2e3f-cwd-store")
	put := func(title, cwd string, named bool) Focus {
		t.Helper()
		f, err := s.Upsert(as(2), sub, Write{Title: title, Source: SourceSummarizer, Cwd: cwd, CwdNamed: named})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if f := put("Porting the gizmo parser", "/srv/example-work/app/", true); f.Cwd != "/srv/example-work/app" {
		t.Fatalf("stored %q; want the cleaned directory", f.Cwd)
	}
	if f := put("Still porting the gizmo parser", "", false); f.Cwd != "/srv/example-work/app" {
		t.Fatalf("a tail naming no directory left %q", f.Cwd)
	}
	if got, _, err := s.Get(as(2), sub); err != nil || got.Cwd != "/srv/example-work/app" {
		t.Fatalf("read back %q %v", got.Cwd, err)
	}
	if f := put("Porting elsewhere", "relative/dir", true); f.Cwd != "" {
		t.Fatalf("a named malformed directory left %q; want unknown", f.Cwd)
	}
	put("Back in the tree", "/srv/example-work", true)
	// ⚠ Invariant guard: row-level security, not this column, keeps it in.
	if got, has, err := s.Get(as(3), sub); err != nil || has || got.Cwd != "" {
		t.Fatalf("another owner read %+v %v %v", got, has, err)
	}
}
