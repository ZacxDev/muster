package mirrorhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// WHICH OF THIS HOST'S CLONES (clones.go, resolveClone). Real git, no tmux:
// the spawn's own tmux half is agentspawn_tmux_integration_test.go's. The
// clone directories are named pairwise distinctly ("ledger-live" sorts before
// "ledger-standalone", so the host's order is known).

// widgetsClone makes a clone of acme/widgets at dir (its origin GitHub's URL)
// and returns its realpath.
func widgetsClone(t *testing.T, dir string) string {
	t.Helper()
	gitIn(t, "", "init", "-q", "-b", "main", dir)
	gitIn(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	return realOrClean(dir)
}

// TestAHintedCloneMustBeOneOfTheHostsOwn: with two clones under the roots and
// no hint the host refuses as it always did (no_repo, naming the count); a
// hint naming either clone EXACTLY as the host lists it is that clone; and a
// hint that is not one of them — a real clone of the same repository OUTSIDE
// the roots, a symlink to a listed clone, a subdirectory of one, a made-up
// path — is refused with its OWN code and sentence (bad_clone), never a
// fallback. With ONE clone, a hint naming another is refused too, and no hint
// is the clone.
func TestAHintedCloneMustBeOneOfTheHostsOwn(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "src")
	live := widgetsClone(t, filepath.Join(root, "ledger-live"))
	old := widgetsClone(t, filepath.Join(root, "ledger-standalone"))
	outside := widgetsClone(t, filepath.Join(tmp, "elsewhere", "ledger-outside"))
	link := filepath.Join(tmp, "link-to-live")
	if err := os.Symlink(live, link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(live, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	git := gitPath(t)
	ctx := context.Background()
	refused := func(hint, code, detail string, roots ...string) {
		t.Helper()
		got, err := resolveClone(ctx, git, roots, "acme/widgets", hint)
		var e *errSpawn
		if !errors.As(err, &e) || e.code != code || e.detail != detail {
			t.Errorf("hint %q: %q, %v; want %s: %s", hint, got, err, code, detail)
		}
	}
	refused("", mirror.SpawnNoRepo, "repo acme/widgets has 2 clones under this host's roots", root)
	for _, hint := range []string{live, old} {
		if got, err := resolveClone(ctx, git, []string{root}, "acme/widgets", hint); err != nil || got != hint {
			t.Errorf("hint %q: %q, %v; want that clone", hint, got, err)
		}
	}
	notOurs := "the clone muster named is not one of this host's 2 clone(s) of repo acme/widgets under its roots"
	for _, hint := range []string{outside, link, filepath.Join(live, "sub"), live + "/", "/srv/fixture-z/never-cloned"} {
		refused(hint, mirror.SpawnBadClone, notOurs, root)
	}
	single := filepath.Join(tmp, "single")
	only := widgetsClone(t, filepath.Join(single, "ledger-only"))
	refused(live, mirror.SpawnBadClone, "the clone muster named is not one of this host's 1 clone(s) of repo acme/widgets under its roots", single)
	if got, err := resolveClone(ctx, git, []string{single}, "acme/widgets", ""); err != nil || got != only {
		t.Fatalf("one clone, no hint: %q, %v", got, err)
	}
}

// TestTheCloneListMarksTheLiveSessionsClone: the host lists every clone of the
// repository under its roots, in its order, and marks LIVE exactly the one a
// listed window works in — here through a worktree of it OUTSIDE the roots, a
// subdirectory deep (the worktree resolves to its main tree by its
// commondir). A window muster started (its dispatch tag) in the other clone
// does not make that one live; a window outside every clone marks nothing.
// The list is refused, with the spawn's own codes, for a repository off the
// allow-list or a host whose spawning is off.
func TestTheCloneListMarksTheLiveSessionsClone(t *testing.T) {
	u := newSpawnUnit(t)
	root := u.c.cfg.AgentSpawn.Roots[0]
	live := widgetsClone(t, filepath.Join(root, "ledger-live"))
	old := widgetsClone(t, filepath.Join(root, "ledger-standalone"))
	wt := filepath.Join(u.tmp, "wt-live")
	gitIn(t, live, "worktree", "add", "-q", "-b", "side", wt)
	deep := filepath.Join(wt, "pkg", "deep")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &windowsBackend{windows: []WindowInfo{
		{Session: "work", ID: "@1", Cwd: deep},
		{Session: "agents", ID: "@2", Cwd: old, Dispatch: 41},
		{Session: "work", ID: "@3", Cwd: u.tmp},
	}}
	got, err := u.c.listClones(context.Background(), b, "acme/widgets")
	want := []mirror.AgentClone{{Path: live, Live: true}, {Path: old}}
	if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the list: %+v %v; want %+v", got, err, want)
	}
	// CONTROL: the same window list with the owner's window moved into the
	// standalone clone marks THAT one live, and only it.
	b.windows[0].Cwd = old
	u.c.repos = &repoNamer{}
	if got, _ := u.c.listClones(context.Background(), b, "acme/widgets"); len(got) != 2 || got[0].Live || !got[1].Live {
		t.Fatalf("with the window in the standalone clone: %+v", got)
	}
	var e *errSpawn
	if _, err := u.c.listClones(context.Background(), b, "acme/other"); !errors.As(err, &e) || e.code != mirror.SpawnNoRepo ||
		e.detail != "repo acme/other is not on this host's allow-list" {
		t.Errorf("a repository off the allow-list: %v", err)
	}
	u.c.cfg.AgentSpawn.Enabled = false
	if _, err := u.c.listClones(context.Background(), b, "acme/widgets"); !errors.As(err, &e) || e.code != mirror.SpawnUnavailable {
		t.Errorf("spawning off: %v", err)
	}
}
