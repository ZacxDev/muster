package mirrorhost

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// branchTree is a directory tree for the branch tests: a repository on
// "feat/x-1" with a subdirectory; a worktree whose `.git` FILE names its
// gitdir by an ABSOLUTE path (on "feat/wt-2"), one naming it RELATIVELY (on
// "feat/rel-3"); a repository on a DETACHED head; one whose HEAD holds a name
// outside the grammar; and a directory outside any repository. Every branch
// is distinct, so a lookup that read the wrong HEAD cannot pass.
func branchTree(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("repo/.git/worktrees/wt")
	mk("repo/.git/worktrees/rel")
	mk("repo/cmd/tool")
	write("repo/.git/HEAD", "ref: refs/heads/feat/x-1\n")
	write("repo/.git/worktrees/wt/HEAD", "ref: refs/heads/feat/wt-2\n")
	write("repo/.git/worktrees/rel/HEAD", "ref: refs/heads/feat/rel-3\n")
	mk("wt/sub")
	write("wt/.git", "gitdir: "+filepath.Join(root, "repo/.git/worktrees/wt")+"\n")
	mk("rel")
	write("rel/.git", "gitdir: ../repo/.git/worktrees/rel\n")
	mk("detached/.git")
	write("detached/.git/HEAD", "0123456789abcdef0123456789abcdef01234567\n")
	mk("odd/.git")
	write("odd/.git/HEAD", "ref: refs/heads/feat/é-1\n")
	mk("plain/inner")
	return root
}

// TestABranchIsReadFromTheRepositorysHead: a repository, a subdirectory of
// one, a worktree by an absolute and by a relative gitdir — each its OWN
// HEAD's branch — and no branch for a detached HEAD, a name outside the
// grammar, outside git, or the inputs that name nothing.
func TestABranchIsReadFromTheRepositorysHead(t *testing.T) {
	root := branchTree(t)
	r := &branchReader{}
	for _, c := range []struct{ cwd, want string }{
		{filepath.Join(root, "repo"), "feat/x-1"},
		{filepath.Join(root, "repo/cmd/tool"), "feat/x-1"},
		{filepath.Join(root, "repo/cmd/gone") + deletedSuffix, "feat/x-1"},
		{filepath.Join(root, "wt/sub"), "feat/wt-2"},
		{filepath.Join(root, "rel"), "feat/rel-3"},
		{filepath.Join(root, "detached"), ""},
		{filepath.Join(root, "odd"), ""},
		{filepath.Join(root, "plain/inner"), ""},
		{"", ""},
		{"relative/dir", ""},
	} {
		if got := r.branch(c.cwd); got != c.want {
			t.Errorf("branch(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}

// TestAnOverlongHeadIsNoBranchNeverACutOne: a HEAD past maxHeadBytes is no
// answer — not its first maxHeadBytes, which would be ANOTHER (valid) name —
// and a branch over mirror.MaxBranch is none either.
func TestAnOverlongHeadIsNoBranchNeverACutOne(t *testing.T) {
	root := t.TempDir()
	for name, n := range map[string]int{"huge": maxHeadBytes, "long": mirror.MaxBranch + 1, "edge": mirror.MaxBranch} {
		dir := filepath.Join(root, name, ".git")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte(headPrefix+strings.Repeat("b", n)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A valid first line followed by more than maxHeadBytes: no answer either
	// (a reader that kept its first maxHeadBytes would answer feat/x-1).
	if err := os.MkdirAll(filepath.Join(root, "padded", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "padded", ".git", "HEAD"), []byte(headPrefix+"feat/x-1\n"+strings.Repeat("#", maxHeadBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &branchReader{}
	if got := r.branch(filepath.Join(root, "padded")); got != "" {
		t.Errorf("a HEAD over maxHeadBytes answered %q, want no branch", got)
	}
	if got := r.branch(filepath.Join(root, "huge")); got != "" {
		t.Errorf("a %d-byte HEAD answered %d bytes, want no branch", len(headPrefix)+maxHeadBytes+1, len(got))
	}
	if got := r.branch(filepath.Join(root, "long")); got != "" {
		t.Errorf("a %d-byte branch answered %d bytes, want no branch", mirror.MaxBranch+1, len(got))
	}
	if got := r.branch(filepath.Join(root, "edge")); len(got) != mirror.MaxBranch {
		t.Errorf("a %d-byte branch answered %q, want it whole", mirror.MaxBranch, got)
	}
}

// TestTheBranchLookupIsCachedPerCwdAndPerHead: ten windows in one
// repository's cwd read its HEAD ONCE, and five more reads inside
// branchCacheTTL read NOTHING and stat NOTHING; past branchCacheTTL the HEAD is
// read again (a `git checkout` shows) while the walk is not repeated until
// repoCacheTTL.
func TestTheBranchLookupIsCachedPerCwdAndPerHead(t *testing.T) {
	root := branchTree(t)
	cwd := filepath.Join(root, "repo/cmd/tool")
	var stats, reads int
	now := time.Unix(1791903000, 0)
	r := &branchReader{now: func() time.Time { return now },
		stat: func(p string) (os.FileInfo, error) { stats++; return os.Stat(p) },
		read: func(p string) ([]byte, error) { reads++; return readHead(p) }}
	var windows []WindowInfo
	for i := 0; i < 10; i++ {
		windows = append(windows, WindowInfo{Session: "s", ID: "@" + strconv.Itoa(i+1), Cwd: cwd})
	}
	first := r.windowBranches(windows)
	if len(first) != 10 || first["@3"] != "feat/x-1" {
		t.Fatalf("windowBranches = %v", first)
	}
	if stats != 3 || reads != 1 {
		t.Fatalf("the first read made %d stats and %d reads, want 3 (tool, cmd, repo) and 1", stats, reads)
	}
	stats, reads = 0, 0
	for i := 0; i < 5; i++ {
		r.windowBranches(windows)
	}
	if stats != 0 || reads != 0 {
		t.Fatalf("five more reads made %d stats and %d reads, want 0 (cached)", stats, reads)
	}
	if err := os.WriteFile(filepath.Join(root, "repo/.git/HEAD"), []byte("ref: refs/heads/feat/y-9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(branchCacheTTL)
	if got := r.windowBranches(windows); got["@1"] != "feat/y-9" {
		t.Fatalf("past branchCacheTTL the checkout reads %q, want feat/y-9", got["@1"])
	}
	if stats != 0 || reads != 1 {
		t.Fatalf("past branchCacheTTL: %d stats and %d reads, want 0 (the walk is still cached) and 1", stats, reads)
	}
}

// TestAHungHeadReadDoesNotFreezeTheListing (#151's rule): a HEAD read that
// never returns costs the listing at most the timeout — the window has no
// branch — no second read starts while it is stuck, and its late answer
// replaces the empty one.
func TestAHungHeadReadDoesNotFreezeTheListing(t *testing.T) {
	root := branchTree(t)
	cwd := filepath.Join(root, "repo")
	release := make(chan struct{})
	var hung atomic.Int32
	r := &branchReader{timeout: 200 * time.Millisecond, read: func(p string) ([]byte, error) {
		if strings.HasSuffix(p, "HEAD") {
			hung.Add(1)
			<-release
		}
		return readHead(p)
	}}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	start := time.Now()
	if got := r.branch(cwd); got != "" {
		t.Fatalf("a hung read answered %q", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the hung read took %s", took)
	}
	r.now = func() time.Time { return time.Now().Add(2 * branchCacheTTL) }
	for i := 0; i < 5; i++ {
		start = time.Now()
		r.branch(cwd)
		if took := time.Since(start); took > 100*time.Millisecond {
			t.Fatalf("read %d while stuck waited %s: a second read was started", i, took)
		}
	}
	if n := hung.Load(); n != 1 {
		t.Fatalf("%d reads reached the hung HEAD, want 1", n)
	}
	r.now = nil
	once.Do(func() { close(release) })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if r.branch(cwd) == "feat/x-1" {
			return
		}
	}
	t.Fatal("the read's late answer never replaced the empty one")
}

// TestAPerWindowHostSendsItsBranchesAfterItsWindows: the host's branches
// message — after the hello that listed the windows, one entry per window on
// a branch (none for one outside git), sent again only when a branch CHANGES.
func TestAPerWindowHostSendsItsBranchesAfterItsWindows(t *testing.T) {
	root := branchTree(t)
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: []WindowInfo{
		{Session: "scratch2", ID: "@7", Index: 3, Active: true, Pane: "%21", PanePID: 1, Command: "claude",
			Claude: &ClaudePane{Pane: "%21", PID: 9, Gen: "sess-a"}, Cwd: filepath.Join(root, "repo/cmd")},
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 2, Command: "zsh", Cwd: filepath.Join(root, "plain")},
	}}
	d.run(t, b, nil, false)
	waitUntil(t, "the first branches message", func() bool { return len(d.of(mirror.TypeBranches)) == 1 })
	if got := d.of(mirror.TypeBranches)[0].Branches; !maps.Equal(got, map[string]string{"@7": "feat/x-1"}) {
		t.Fatalf("branches = %v, want only the repository window's", got)
	}
	d.mu.Lock()
	if d.msgs[0].Type != mirror.TypeHello || d.msgs[0].Windows == nil {
		d.mu.Unlock()
		t.Fatalf("the first message was %q, want the hello listing the windows", d.msgs[0].Type)
	}
	d.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	if n := len(d.of(mirror.TypeBranches)); n != 1 {
		t.Fatalf("%d branches messages with nothing changed, want 1", n)
	}
	b.mu.Lock()
	w := slices.Clone(b.windows)
	b.mu.Unlock()
	w[0].Cwd = filepath.Join(root, "wt/sub")
	b.set(w...)
	waitUntil(t, "the second branches message", func() bool { return len(d.of(mirror.TypeBranches)) == 2 })
	if got := d.of(mirror.TypeBranches)[1].Branches; !maps.Equal(got, map[string]string{"@7": "feat/wt-2"}) {
		t.Fatalf("branches after the move = %v", got)
	}
}

// TestTheLargestBranchesMessageFitsOneFrame is the size behind sending the
// branches on a message of their own: MaxWindows entries, the longest window
// id, every branch at MaxBranch bytes (no byte of the grammar is one JSON
// escapes). It must fit one frame. The control: the window list's measured
// worst case (TestTheLargestWindowListFitsOneFrame) could not have carried
// even the field's NAME per window — the separate message is needed.
func TestTheLargestBranchesMessageFitsOneFrame(t *testing.T) {
	var windows []mirror.Window
	branches := map[string]string{}
	for i := 0; i < mirror.MaxWindows; i++ {
		id := "@" + strconv.Itoa(9_999_999_999-i)
		windows = append(windows, mirror.Window{Session: "s", ID: id, Index: i})
		branches[id] = strings.Repeat("+", mirror.MaxBranch-len(itoa(i))) + itoa(i)
	}
	kept := mirror.NormalizeBranches(branches, mirror.NormalizeWindows(windows, 0))
	if len(kept) != mirror.MaxWindows {
		t.Fatalf("instrument check: %d branches kept, want %d", len(kept), mirror.MaxWindows)
	}
	for _, v := range kept {
		if len(v) != mirror.MaxBranch {
			t.Fatalf("instrument check: a branch of %d bytes, so this is not the worst case", len(v))
		}
	}
	b, err := json.Marshal(mirror.Msg{Type: mirror.TypeBranches, Branches: kept})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > mirror.MaxFrame {
		t.Fatalf("the largest branches message is %d bytes, over MaxFrame (%d)", len(b), mirror.MaxFrame)
	}
	// The window list's measured worst case (TestTheLargestWindowListFitsOneFrame's log).
	const windowListWorst = 1_046_461
	if windowListWorst+mirror.MaxWindows*len(`,"branch":""`) <= mirror.MaxFrame {
		t.Fatalf("instrument check: an empty branch field per window would have fitted the window list — the separate message is not needed")
	}
	t.Logf("largest branches message: %d branches in %d bytes (%.0f%% of MaxFrame)", len(kept), len(b), 100*float64(len(b))/float64(mirror.MaxFrame))
}
