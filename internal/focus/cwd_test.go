package focus

import (
	"context"
	"strings"
	"testing"
)

// THE WORKING DIRECTORY (cwd.go). Fixture paths are invented placeholders.

func TestCleanCwdBelievesOnlyAnAbsoluteBoundedPath(t *testing.T) {
	for in, want := range map[string]string{
		"/srv/example-work":           "/srv/example-work",
		"/srv/example-work/":          "/srv/example-work",
		"/srv/example-work/a/../b/./": "/srv/example-work/b",
		"//srv//example-work":         "/srv/example-work",
	} {
		if got, ok := CleanCwd(in); !ok || got != want {
			t.Errorf("%q: %q %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "srv/example-work", "./x", "~/x", "/srv/ex\nample", "/srv/ex\x00ample", "/srv/ex​ample",
		"/" + strings.Repeat("d", MaxCwdBytes)} {
		if got, ok := CleanCwd(bad); ok || got != "" {
			t.Errorf("%q: %q %v; want unknown", bad, got, ok)
		}
	}
}

// 🔴 The newest entry naming a directory decides; a malformed newest one is
// unknown, never an older valid one.
func TestTheLatestDirectoryWins(t *testing.T) {
	entries := []Entry{{Role: "user", Text: "a", Cwd: "/srv/example-old"}, {Role: "assistant", Text: "b"},
		{Role: "user", Text: "c", Cwd: "/srv/example-new/"}, {Role: "assistant", Text: "d"}}
	if got, named := LatestCwd(entries); got != "/srv/example-new" || !named {
		t.Fatalf("latest: %q %v", got, named)
	}
	if got, named := LatestCwd(append(entries, Entry{Role: "user", Text: "e", Cwd: "relative/dir"})); got != "" || !named {
		t.Fatalf("a malformed newest directory gave %q %v; want named and unknown", got, named)
	}
	if got, named := LatestCwd([]Entry{{Role: "user", Text: "x"}}); got != "" || named {
		t.Fatalf("no directory gave %q %v", got, named)
	}
}

// The refresh stores the tail's latest directory with the focus; a later tail
// naming none keeps it; a malformed newest one makes it unknown.
func TestARefreshRecordsTheTailsDirectory(t *testing.T) {
	r, st, src, _, sub := fixture()
	ctx := context.Background()
	src.tails[sub] = []Entry{{Role: "user", Text: "fix the build", Cwd: "/srv/example-old"}, {Role: "assistant", Text: "on it"},
		{Role: "user", Text: "and the tests", Cwd: "/srv/example-work/app"}, {Role: "assistant", Text: "patching"}}
	if o, err := r.Refresh(ctx, 1, sub); err != nil || o != OutcomeUpdated || st.rows[sub].Cwd != "/srv/example-work/app" {
		t.Fatalf("%v %v %+v; want the newest directory stored", o, err, st.rows[sub])
	}
	src.tails[sub] = []Entry{{Role: "assistant", Text: "a long autonomous stretch with no prompt in the tail"}}
	if o, _ := r.Refresh(ctx, 1, sub); o != OutcomeUpdated || st.rows[sub].Cwd != "/srv/example-work/app" {
		t.Fatalf("a tail naming no directory lost the stored one: %+v", st.rows[sub])
	}
	src.tails[sub] = []Entry{{Role: "user", Text: "elsewhere now", Cwd: "not/absolute"}}
	if o, _ := r.Refresh(ctx, 1, sub); o != OutcomeUpdated || st.rows[sub].Cwd != "" {
		t.Fatalf("a malformed newest directory left %q; want unknown", st.rows[sub].Cwd)
	}
}
