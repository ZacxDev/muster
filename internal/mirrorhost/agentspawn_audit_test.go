package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// The host's controls after #516's delta audit: Claude Code's lock around the
// trust write, the rate limit under concurrency, bounded audit lines, an
// absolute git, worktrees counted as their clone, and "<owner>/*".

// claudeStyleWriter is a writer of the state file following Claude Code's
// protocol (proper-lockfile): mkdir "<file>.lock", read, modify, write a temp
// file and rename it over, rmdir the lock. Each of its n updates adds the key
// "writer-<i>" at the top level.
func claudeStyleWriter(t *testing.T, path string, n int, hold time.Duration) error {
	lock := path + ".lock"
	for i := 0; i < n; i++ {
		for os.Mkdir(lock, 0o755) != nil {
			time.Sleep(time.Millisecond)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			_ = os.Remove(lock)
			return err
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(b, &top); err != nil {
			_ = os.Remove(lock)
			return err
		}
		top["writer-"+strconv.Itoa(i)] = json.RawMessage("true")
		out, _ := json.Marshal(top)
		time.Sleep(hold) // Claude Code's own work between its read and its write
		tmp := path + ".writer.tmp"
		if err := os.WriteFile(tmp, out, 0o644); err != nil {
			_ = os.Remove(lock)
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(lock)
			return err
		}
		_ = os.Remove(lock)
	}
	return nil
}

// trustWorktrees adds n worktrees of the rig's clone, dispatches 100.., under
// the agents directory.
func (r *trustRig) trustWorktrees(t *testing.T, n int) []string {
	t.Helper()
	var dirs []string
	for i := 0; i < n; i++ {
		id := 100 + i
		dir := filepath.Join(realOrClean(r.u.dir), "acme__widgets", strconv.Itoa(id))
		gitIn(t, r.clone, "worktree", "add", "-q", "-b", "muster/"+strconv.Itoa(id)+"-x", dir)
		dirs = append(dirs, dir)
	}
	return dirs
}

// TestTheTrustWriteLosesNoConcurrentClaudeCodeUpdate: a writer following Claude
// Code's lock protocol makes 60 updates while the host writes 12 trust entries.
// Every one of both survives, over several rounds. (Before the lock, 2-3 of
// the 60 were lost per round.)
func TestTheTrustWriteLosesNoConcurrentClaudeCodeUpdate(t *testing.T) {
	for round := 0; round < 3; round++ {
		r := newTrustRig(t)
		dirs := r.trustWorktrees(t, 12)
		var wg sync.WaitGroup
		var werr error
		wg.Add(1)
		go func() { defer wg.Done(); werr = claudeStyleWriter(t, r.state, 60, 2*time.Millisecond) }()
		var trustErrs []error
		for i, d := range dirs {
			if err := r.trust(d, int64(100+i), true); err != nil {
				trustErrs = append(trustErrs, err)
			}
			time.Sleep(3 * time.Millisecond)
		}
		wg.Wait()
		if werr != nil {
			t.Fatal(werr)
		}
		// A read-back that saw the writer's later rename is refused (and not
		// restored); its entry may then be gone too, so count the written ones.
		b, _ := os.ReadFile(r.state)
		var top map[string]json.RawMessage
		if err := json.Unmarshal(b, &top); err != nil {
			t.Fatal(err)
		}
		lostWriter := 0
		for i := 0; i < 60; i++ {
			if _, ok := top["writer-"+strconv.Itoa(i)]; !ok {
				lostWriter++
			}
		}
		var projects map[string]json.RawMessage
		_ = json.Unmarshal(top["projects"], &projects)
		lostTrust := 0
		for _, d := range dirs {
			if _, ok := projects[d]; !ok {
				lostTrust++
			}
		}
		if lostWriter != 0 || lostTrust != 0 || len(trustErrs) != 0 {
			t.Fatalf("round %d: lost %d of the writer's 60 updates and %d of 12 trust entries (%d trust writes refused: %v)",
				round, lostWriter, lostTrust, len(trustErrs), trustErrs)
		}
	}
}

// TestTheTrustWriteKeepsTheModeRefusesASymlinkAndFailsClosedOnAHeldLock: the
// file's mode survives (0644 stays 0644); a symlinked state file is refused
// and left as it is; a lock held past the bound refuses the entry and changes
// nothing; a STALE lock (older than proper-lockfile's 10 s) is taken over.
func TestTheTrustWriteKeepsTheModeRefusesASymlinkAndFailsClosedOnAHeldLock(t *testing.T) {
	r := newTrustRig(t)
	if err := os.Chmod(r.state, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.trust(r.dir, 17, true); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(r.state); fi.Mode().Perm() != 0o644 {
		t.Errorf("the state file's mode became %v; want 0644 kept", fi.Mode().Perm())
	}

	want := func(t *testing.T, err error, why string) {
		t.Helper()
		e, ok := err.(*errSpawn)
		if !ok || e.detail != "trust entry not written: "+why {
			t.Fatalf("refusal %v; want %q", err, why)
		}
	}

	t.Run("a held lock", func(t *testing.T) {
		r := newTrustRig(t)
		r.u.c.cfg.AgentSpawn.TrustLockWait = 100 * time.Millisecond
		if err := os.Mkdir(r.state+".lock", 0o755); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(r.state)
		want(t, r.trust(r.dir, 17, true), "Claude Code's state file is locked by another writer")
		if after, _ := os.ReadFile(r.state); !bytes.Equal(before, after) {
			t.Fatal("a refused write changed the file")
		}
		if _, err := os.Stat(r.state + ".lock"); err != nil {
			t.Fatal("the refused write removed another writer's lock")
		}
	})
	t.Run("a stale lock", func(t *testing.T) {
		r := newTrustRig(t)
		r.u.c.cfg.AgentSpawn.TrustLockWait = 100 * time.Millisecond
		if err := os.Mkdir(r.state+".lock", 0o755); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Minute)
		_ = os.Chtimes(r.state+".lock", old, old)
		if err := r.trust(r.dir, 17, true); err != nil {
			t.Fatalf("a stale lock was not taken over: %v", err)
		}
		if _, err := os.Stat(r.state + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the lock was not released: %v", err)
		}
	})
	t.Run("a symlinked state file", func(t *testing.T) {
		r := newTrustRig(t)
		real := r.state + ".real"
		if err := os.Rename(r.state, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, r.state); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(real)
		want(t, r.trust(r.dir, 17, true), "Claude Code's state file is a symlink or not a regular file")
		if after, _ := os.ReadFile(real); !bytes.Equal(before, after) {
			t.Fatal("a write went through the symlink")
		}
	})
}

// TestConcurrentSpawnsNeverPassTheRateLimit: 10 requests at once under
// 3/10m admit exactly 3 — the admission is recorded under the spawner's lock,
// before the next request counts. (The spawns themselves then stop at the
// hermetic unit's absent tmux server.)
func TestConcurrentSpawnsNeverPassTheRateLimit(t *testing.T) {
	for round := 0; round < 3; round++ {
		u := newSpawnUnit(t)
		u.c.cfg.AgentSpawn.Rate = "3/10m"
		var wg sync.WaitGroup
		var mu sync.Mutex
		limited := 0
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				_, err := u.c.spawnAgent(context.Background(), nil, mirror.Msg{Type: mirror.TypeAgentSpawn, Dispatch: id, Spawn: reqPtr(widgets(testPrompt))}, time.Now())
				var e *errSpawn
				if errors.As(err, &e) && strings.HasPrefix(e.detail, "rate limited") {
					mu.Lock()
					limited++
					mu.Unlock()
				}
			}(int64(200 + i))
		}
		wg.Wait()
		n, err := admittedSince(u.log, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 || limited != 7 {
			t.Fatalf("round %d: %d admitted, %d rate limited; want 3 and 7", round, n, limited)
		}
	}
}

// TestAnOversizeFieldCannotDisableTheLimit: a request with a 200 KB name is
// logged with every field capped (no line over auditLineMax), and the next
// requests' rate checks still read the log: the limit still counts and still
// refuses. A hand-written oversize line and a corrupt one are skipped. A log
// past auditLogMax refuses every spawn, saying why.
func TestAnOversizeFieldCannotDisableTheLimit(t *testing.T) {
	u := newSpawnUnit(t)
	u.c.cfg.AgentSpawn.Rate = "2/10m"
	// Lines that must not stop the count.
	f, err := os.OpenFile(u.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		if err := os.MkdirAll(filepath.Dir(u.log), 0o700); err != nil {
			t.Fatal(err)
		}
		if f, err = os.OpenFile(u.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Fprintf(f, `{"kind":"request","name":"%s"}`+"\n", strings.Repeat("x", 3*auditLineMax))
	fmt.Fprintf(f, "{not json\n")
	_ = f.Close()

	big := widgets(testPrompt)
	big.Name = strings.Repeat("n", 200<<10)
	big.Base = strings.Repeat("b", 200<<10)
	_, _ = u.spawn(t, 301, big)
	b, _ := os.ReadFile(u.log)
	for _, line := range bytes.Split(b, []byte("\n"))[2:] {
		if len(line) > auditLineMax {
			t.Fatalf("a %d-byte audit line was written", len(line))
		}
	}
	// Two admissions fit; the third is refused by the limit — read past the
	// oversize and corrupt lines.
	for i, want := range []bool{false, false, true} {
		_, e := u.spawn(t, int64(310+i), widgets(testPrompt))
		got := e != nil && strings.HasPrefix(e.detail, "rate limited")
		if got != want {
			t.Fatalf("spawn %d: rate limited %v (%+v); want %v", i, got, e, want)
		}
	}
	if n, err := admittedSince(u.log, time.Now().Add(-time.Hour)); err != nil || n != 2 {
		t.Fatalf("admittedSince = %d, %v; want 2", n, err)
	}
	// The big request was refused before the rate check (its base), so it
	// admitted nothing: its own line did not count.

	if err := os.Truncate(u.log, auditLogMax+1); err != nil {
		t.Fatal(err)
	}
	_, e := u.spawn(t, 320, widgets(testPrompt))
	if e == nil || e.code != mirror.SpawnUnavailable || !strings.Contains(e.detail, "audit log full") {
		t.Fatalf("a full log: %+v", e)
	}
}

// TestNoGitOnPathOffersNothing: with no git on PATH, New leaves the spawner's
// git empty and the host is unavailable ("git not found"), so its hello offers
// no runtime; with git, it is an absolute path.
func TestNoGitOnPathOffersNothing(t *testing.T) {
	cfg := func() Config {
		return Config{Host: "host-bravo", Token: mirror.TokenPrefix + "x", URL: "https://muster.invalid", Backend: Tmux{}, Interactive: true,
			AgentSpawn: AgentSpawnConfig{Enabled: true, Dir: "/srv/agents", Log: "/srv/agents.log", RuntimeDir: "/run/user/1"}}
	}
	gitDir := filepath.Dir(gitPath(t))
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	t.Setenv("PATH", t.TempDir())
	c, err := New(cfg())
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.AgentSpawn.Git != "" || agentSpawnUnavailable(c.cfg) != "git not found" {
		t.Fatalf("no git: git %q, unavailable %q", c.cfg.AgentSpawn.Git, agentSpawnUnavailable(c.cfg))
	}
	h := &hostSide{c: c, version: 2, ctx: context.Background()}
	if rt := h.agentSpawnRuntimes(); rt != nil {
		t.Fatalf("with no git the hello offers %v", rt)
	}
	t.Setenv("PATH", gitDir)
	if c, err = New(cfg()); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.cfg.AgentSpawn.Git) || agentSpawnUnavailable(c.cfg) == "git not found" {
		t.Fatalf("with git: %q", c.cfg.AgentSpawn.Git)
	}
}

// TestAWorktreeOfTheCloneIsTheClone: under a root, the clone and a worktree of
// it are ONE repository (resolved to the clone, which new worktrees are cut
// from); a prunable worktree (its repository gone) is skipped; two separate
// clones are refused; a worktree under a root of a clone OUTSIDE the roots
// is no clone at all.
func TestAWorktreeOfTheCloneIsTheClone(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "src")
	clone := func(dir string) string {
		gitIn(t, "", "init", "-q", "-b", "main", dir)
		gitIn(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
		gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
		return realOrClean(dir)
	}
	main := clone(filepath.Join(root, "widgets"))
	gitIn(t, main, "worktree", "add", "-q", "-b", "side", filepath.Join(root, "widgets-side"))
	git := gitPath(t)
	got, err := resolveClone(context.Background(), git, []string{root}, "acme/widgets")
	if err != nil || got != main {
		t.Fatalf("the clone and its worktree: %q, %v; want the clone %q", got, err, main)
	}
	// A prunable worktree: one whose repository's record of it is gone.
	other := clone(filepath.Join(tmp, "elsewhere", "widgets"))
	wt := filepath.Join(root, "widgets-orphan")
	gitIn(t, other, "worktree", "add", "-q", "-b", "orphan", wt)
	if err := os.RemoveAll(filepath.Join(other, ".git", "worktrees", "widgets-orphan")); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveClone(context.Background(), git, []string{root}, "acme/widgets"); err != nil || got != main {
		t.Fatalf("with a prunable worktree: %q, %v", got, err)
	}
	// A live worktree under the root of a clone outside every root: not one.
	gitIn(t, other, "worktree", "prune")
	gitIn(t, other, "worktree", "add", "-q", "-b", "outside", filepath.Join(root, "widgets-outside"))
	if got, err := resolveClone(context.Background(), git, []string{root}, "acme/widgets"); err != nil || got != main {
		t.Fatalf("with an outside clone's worktree: %q, %v", got, err)
	}
	// Two separate clones under the root: refused.
	clone(filepath.Join(root, "widgets-2"))
	_, err = resolveClone(context.Background(), git, []string{root}, "acme/widgets")
	var e *errSpawn
	if !errors.As(err, &e) || e.code != mirror.SpawnNoRepo || e.detail != "repo acme/widgets has 2 clones under this host's roots" {
		t.Fatalf("two clones: %v", err)
	}
}

// TestTheAllowListTakesAnOwnerWildcard: "<owner>/*" admits that owner's
// repositories, case-insensitively, by equality on the owner — never a
// prefix — and mixes with exact entries; "<owner>/x*" and dot names are not
// entries; a repository name starting with '.' is not a name.
func TestTheAllowListTakesAnOwnerWildcard(t *testing.T) {
	list, err := ParseAgentSpawnRepos("Globex/*, Acme/Widgets")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(list, ",") != "globex/*,acme/widgets" {
		t.Fatalf("parsed %q", list)
	}
	for repo, want := range map[string]bool{
		"Globex/muster": true, "globex/anything": true, "GLOBEX/x": true,
		"acme/widgets": true, "ACME/WIDGETS": true,
		"Globex-evil/x": false, "globe/x": false, "acme/widgets2": false, "acme/other": false, "evil/globex": false,
	} {
		if got := repoAllowed(list, repo); got != want {
			t.Errorf("repoAllowed(%q) = %v; want %v", repo, got, want)
		}
	}
	for _, bad := range []string{"Globex/x*", "Globex/**", "*/*", "Globex/.", "Globex/..", "-x/*"} {
		if _, err := ParseAgentSpawnRepos(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	for _, name := range []string{"acme/.", "acme/..", "acme/..x"} {
		if validRepoName(name) {
			t.Errorf("%q is a repo name", name)
		}
	}
	// A spawn: the wildcard admits it past the allow-list (the refusal that
	// follows is the hermetic unit's, not the list's).
	u := newSpawnUnit(t)
	u.c.cfg.AgentSpawn.Repos = []string{"acme/*"}
	if _, e := u.spawn(t, 330, widgets(testPrompt)); e != nil && strings.Contains(e.detail, "allow-list") {
		t.Fatalf("acme/* refused acme/widgets: %+v", e)
	}
	u.c.cfg.AgentSpawn.Repos = []string{"acm/*"}
	if _, e := u.spawn(t, 331, widgets(testPrompt)); e == nil || !strings.Contains(e.detail, "allow-list") {
		t.Fatalf("acm/* admitted acme/widgets: %+v", e)
	}
}

func reqPtr(r mirror.AgentSpawnReq) *mirror.AgentSpawnReq { return &r }

func gitPath(t *testing.T) string {
	t.Helper()
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if p := filepath.Join(d, "git"); fileExecutable(p) {
			return p
		}
	}
	t.Fatal("git is not installed")
	return ""
}

func fileExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// TestAStaleLockThatCannotBeRemovedFailsClosedWithinTheWait: a stale lock
// directory holding a file cannot be removed with rmdir. The lock is not had,
// and the call returns within about its wait instead of spinning. Measured
// on a 200 ms wait.
func TestAStaleLockThatCannotBeRemovedFailsClosedWithinTheWait(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".lock", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".lock", "pid"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	_ = os.Chtimes(path+".lock", old, old)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		unlock, err := lockClaudeState(path, 200*time.Millisecond)
		if unlock != nil {
			unlock()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stale lock that cannot be removed was taken")
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("returned after %v; want about the 200 ms wait", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("lockClaudeState did not return within 3 s (it spins on a stale lock it cannot remove)")
	}
	if _, err := os.Stat(filepath.Join(path+".lock", "pid")); err != nil {
		t.Fatalf("another writer's lock was disturbed: %v", err)
	}
}

// TestDotRepositoryNamesAreNames: GitHub allows a repository name that starts
// with '.' (".github", ".dotfiles"). "." and "..", and any "..", are still
// refused.
func TestDotRepositoryNamesAreNames(t *testing.T) {
	for name, want := range map[string]bool{
		"acme/.github": true, "acme/.dotfiles": true, "acme/widgets": true, "acme/a.b": true,
		"acme/.": false, "acme/..": false, "acme/..x": false, "acme/x..y": false, "acme/a/../b": false,
	} {
		_, err := ParseAgentSpawnRepos(name)
		if got := err == nil; got != want {
			t.Errorf("ParseAgentSpawnRepos(%q): accepted %v; want %v", name, got, want)
		}
	}
	u := newSpawnUnit(t)
	u.c.cfg.AgentSpawn.Repos = []string{"acme/.github"}
	r := widgets(testPrompt)
	r.Repo = "acme/.github"
	if _, e := u.spawn(t, 340, r); e != nil && (strings.Contains(e.detail, "is not <owner>/<repo>") || strings.Contains(e.detail, "allow-list")) {
		t.Fatalf("acme/.github refused as a name: %+v", e)
	}
	r.Repo = "acme/.."
	if _, e := u.spawn(t, 341, r); e == nil || e.detail != `"acme/.." is not <owner>/<repo>` {
		t.Fatalf("acme/..: %+v", e)
	}
}
