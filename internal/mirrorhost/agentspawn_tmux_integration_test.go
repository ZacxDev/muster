//go:build tmuxit

package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HOST SPAWN against REAL tmux and REAL git (agentspawn.go), with a stub
// claude that records its argv.
//
// 🔴 THE SERVER IS A DEFAULT SERVER UNDER A PRIVATE TMUX_TMPDIR: the spawn
// refuses any non-default server (design 3.8), so the rig cannot use -L; it
// points TMUX_TMPDIR at its own directory instead, which also proves a custom
// TMUX_TMPDIR is honoured. The operator's own server is never touched.

type spawnRig struct {
	*spawnUnit
	tm                Tmux
	bare, clone, recs string
}

func newSpawnRig(t *testing.T) *spawnRig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux is not installed; this suite fails rather than skips: %v", err)
	}
	u := newSpawnUnit(t)
	r := &spawnRig{spawnUnit: u, recs: filepath.Join(u.tmp, "recs")}
	home := filepath.Join(u.tmp, "home")
	for _, d := range []string{filepath.Join(u.tmp, "x"), home, r.recs, u.c.cfg.AgentSpawn.Roots[0], filepath.Join(u.tmp, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMUX_TMPDIR", filepath.Join(u.tmp, "x"))
	t.Setenv("TMUX", "")
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid"} {
		t.Setenv(k, v)
	}
	r.tm = Tmux{}
	u.c.cfg.Backend = r.tm
	if out, err := exec.Command("tmux", "-f", "/dev/null", "new-session", "-d", "-s", "base").CombinedOutput(); err != nil {
		t.Fatalf("start the rig's tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run() })
	// The remote, with main and dev.
	r.bare = filepath.Join(u.tmp, "remote", "widgets.git")
	work := filepath.Join(u.tmp, "seed")
	gitIn(t, "", "init", "-q", "--bare", "-b", "main", r.bare)
	gitIn(t, "", "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("helo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", "README")
	gitIn(t, work, "commit", "-q", "-m", "first")
	gitIn(t, work, "branch", "dev")
	gitIn(t, work, "push", "-q", r.bare, "main", "dev")
	// The owner's clone, under a root, whose origin is the GitHub name.
	r.clone = filepath.Join(u.c.cfg.AgentSpawn.Roots[0], "widgets")
	gitIn(t, "", "clone", "-q", r.bare, r.clone)
	gitIn(t, r.clone, "remote", "set-url", "origin", "https://github.com/acme/widgets")
	gitIn(t, r.clone, "config", "url."+r.bare+".insteadOf", "https://github.com/acme/widgets")
	// The stub claude: its argv, NUL-separated, into a file named after its
	// working directory's base (the dispatch id); then it stays.
	stub := filepath.Join(u.tmp, "bin", "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\0' \"$@\" > "+r.recs+"/$(basename \"$PWD\").tmp && mv "+r.recs+"/$(basename \"$PWD\").tmp "+r.recs+"/$(basename \"$PWD\")\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	u.c.cfg.AgentSpawn.ClaudeBin = stub
	if err := os.WriteFile(u.c.cfg.AgentSpawn.ClaudeState, []byte(trustStateFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

// argvOf is the stub's argv for dispatch d, once it has run.
func (r *spawnRig) argvOf(t *testing.T, d int64) []string {
	t.Helper()
	p := filepath.Join(r.recs, strconv.FormatInt(d, 10))
	waitFor(t, "the stub claude to record its argv", func() bool { _, err := os.Stat(p); return err == nil })
	b, _ := os.ReadFile(p)
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

func (r *spawnRig) tmux(t *testing.T, args ...string) string {
	t.Helper()
	out, err := r.tm.run(context.Background(), args...)
	if err != nil {
		t.Fatalf("tmux %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// TestTmuxASpawnedClaudeGetsThePromptVerbatimInATaggedWorktree: the feature,
// for every prompt shape the design lists: the stub claude sees EXACTLY
// [--session-id, <uuid>, --permission-mode, bypassPermissions, --, <prompt>]
// (the default: no permission prompts), byte for byte; the prompt file is gone;
// the window is in `agents`, named cc-<d>-<slug>, tagged with its dispatch
// (its OWN option); it sits in a fresh worktree on muster/<d>-<slug> cut from
// origin/main; Claude Code's trust entry names it; the owner's DIRTY clone has
// the same status and HEAD after; the listing reads the window's dispatch; and
// the request wrote its two audit lines, admitted.
func TestTmuxASpawnedClaudeGetsThePromptVerbatimInATaggedWorktree(t *testing.T) {
	r := newSpawnRig(t)
	if err := os.WriteFile(filepath.Join(r.clone, "README"), []byte("the owner's edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusBefore, headBefore := gitIn(t, r.clone, "status", "--porcelain"), gitIn(t, r.clone, "rev-parse", "HEAD")
	r.c.cfg.AgentSpawn.Rate = "100/10m"
	prompts := []string{" a;b", " tail;", ` \;`, " $(id) `id`", ` 'single' "double"`, "\n-leading dash\n", "\nlead and trail\n\n", " " + strings.Repeat("x", 8<<10-len(mirror.SpawnPreface)-1)}
	for i, tail := range prompts {
		d := int64(100 + i)
		prompt := mirror.SpawnPreface + tail
		w, e := r.spawn(t, d, mirror.AgentSpawnReq{Runtime: "claude", Repo: "acme/widgets", Prompt: prompt, Name: "#9 Fix the typo!"})
		if e != nil {
			t.Fatalf("prompt %q: refused %+v", tail, e)
		}
		argv := r.argvOf(t, d)
		// DEFAULT ON (-agent-spawn-skip-permissions): the permission words sit
		// between the session id and `--`, as literals.
		if want := []string{"--session-id", w.AgentSession, "--permission-mode", "bypassPermissions", "--", prompt}; !slices.Equal(argv, want) {
			t.Fatalf("prompt %q: the stub saw %q; want [--session-id %s --permission-mode bypassPermissions -- <prompt>]", tail, argv, w.AgentSession)
		}
		if _, err := os.Stat(filepath.Join(promptDir(r.c.cfg.AgentSpawn.RuntimeDir), strconv.FormatInt(d, 10))); !os.IsNotExist(err) {
			t.Fatalf("prompt %q: the prompt file is still there", tail)
		}
		wantName := "cc-" + strconv.FormatInt(d, 10) + "-9-fix-the-typo"
		if got := r.tmux(t, "display-message", "-p", "-t", w.Window, "#{session_name} #{window_name}"); got != mirror.AgentSpawnSession+" "+wantName {
			t.Fatalf("the window is %q; want agents %s", got, wantName)
		}
		if tag, err := r.tm.dispatchTagOf(context.Background(), w.Window); err != nil || tag != d {
			t.Fatalf("the window's own tag: %d %v", tag, err)
		}
		if w.Branch != "muster/"+strconv.FormatInt(d, 10)+"-9-fix-the-typo" || gitIn(t, w.Worktree, "rev-parse", "--abbrev-ref", "HEAD") != w.Branch+"\n" {
			t.Fatalf("branch %q", w.Branch)
		}
		if got, want := gitIn(t, w.Worktree, "rev-parse", "HEAD"), gitIn(t, r.clone, "rev-parse", "origin/main"); got != want {
			t.Fatalf("the worktree is at %s, not origin/main %s", got, want)
		}
		var st map[string]any
		b, _ := os.ReadFile(r.c.cfg.AgentSpawn.ClaudeState)
		_ = json.Unmarshal(b, &st)
		if e, ok := st["projects"].(map[string]any)[w.Worktree].(map[string]any); !ok || e["hasTrustDialogAccepted"] != true {
			t.Fatalf("no trust entry for %s", w.Worktree)
		}
	}
	if gitIn(t, r.clone, "status", "--porcelain") != statusBefore || gitIn(t, r.clone, "rev-parse", "HEAD") != headBefore {
		t.Fatal("the owner's clone changed")
	}
	ws, err := r.tm.Windows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tagged := 0
	for _, w := range ws {
		if w.Dispatch >= 100 {
			tagged++
		}
	}
	if tagged != len(prompts) {
		t.Fatalf("the listing reads %d tagged windows, want %d", tagged, len(prompts))
	}
	lines := r.auditLines(t)
	// Three lines per spawn: the request, the admission (written under the
	// spawner's lock, what the rate limit counts) and the result.
	if len(lines) != 3*len(prompts) || lines[1]["kind"] != "admit" || lines[2]["result"] != "spawned" || lines[2]["admitted"] != true {
		t.Fatalf("audit: %d lines (%v)", len(lines), lines[:3])
	}
}

// TestTmuxTheAllowListIsTheHostsWhateverTheServerSends: the same request is
// refused with an EMPTY allow-list (no git runs, no window opens) and spawns
// with the repository listed — the positive control that the refusal is the
// list's.
func TestTmuxTheAllowListIsTheHostsWhateverTheServerSends(t *testing.T) {
	r := newSpawnRig(t)
	r.c.cfg.AgentSpawn.Repos = nil
	before := r.tmux(t, "list-windows", "-a", "-F", "#{window_id}")
	if _, e := r.spawn(t, 7, widgets(testPrompt)); e == nil || e.code != mirror.SpawnNoRepo || e.detail != "repo acme/widgets is not on this host's allow-list" {
		t.Fatalf("empty allow-list: %+v", e)
	}
	if n := r.gitCalls(); n != 0 {
		t.Fatalf("git ran %d times", n)
	}
	if after := r.tmux(t, "list-windows", "-a", "-F", "#{window_id}"); after != before {
		t.Fatal("a window opened")
	}
	r.c.cfg.AgentSpawn.Repos = []string{"acme/widgets"}
	if _, e := r.spawn(t, 8, widgets(testPrompt)); e != nil {
		t.Fatalf("control, listed: %+v", e)
	}
	if r.gitCalls() == 0 {
		t.Fatal("control: git never ran for a spawn")
	}
}

// TestTmuxASpawnPastItsDeadlineNeverOpensAWindow: two ways. (1) The deadline
// passes during the work, at the check right before new-window: refused
// "deadline passed", no window, and the worktree, its branch and the prompt
// file are gone. (2) A spawn QUEUED behind another past its deadline — the
// deadline counts from RECEIPT, queue wait included — is refused before any
// git call.
func TestTmuxASpawnPastItsDeadlineNeverOpensAWindow(t *testing.T) {
	r := newSpawnRig(t)
	clock := time.Now()
	var mu sync.Mutex
	r.c.cfg.AgentSpawn.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	r.c.cfg.AgentSpawn.beforeNewWindow = func() { mu.Lock(); clock = clock.Add(2 * AgentSpawnDeadline); mu.Unlock() }
	before := r.tmux(t, "list-windows", "-a", "-F", "#{window_id}")
	w, err := r.c.spawnAgent(context.Background(), nil, mirror.Msg{Dispatch: 21, Spawn: ptr(widgets(testPrompt))}, clock)
	if e, ok := err.(*errSpawn); !ok || e.detail != "deadline passed" {
		t.Fatalf("(1) %v %+v", err, w)
	}
	if after := r.tmux(t, "list-windows", "-a", "-F", "#{window_id}"); after != before {
		t.Fatal("(1) a window opened past the deadline")
	}
	dir := filepath.Join(realOrClean(r.dir), "acme__widgets", "21")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("(1) the worktree was left behind")
	}
	if strings.Contains(gitIn(t, r.clone, "branch", "--list", "muster/21*"), "muster/21") {
		t.Fatal("(1) the branch was left behind")
	}
	if ps, _ := os.ReadDir(promptDir(r.c.cfg.AgentSpawn.RuntimeDir)); len(ps) != 0 {
		t.Fatal("(1) the prompt file was left behind")
	}
	// (2) The queued spawn.
	r.c.cfg.AgentSpawn.beforeNewWindow = nil
	calls := r.gitCalls()
	r.c.spawner.mu.Lock()
	done := make(chan error, 1)
	mu.Lock()
	received := clock // the moment the queued request arrived
	mu.Unlock()
	go func() {
		_, err := r.c.spawnAgent(context.Background(), nil, mirror.Msg{Dispatch: 22, Spawn: ptr(widgets(testPrompt))}, received)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	clock = clock.Add(AgentSpawnDeadline + 10*time.Second) // the first spawn held the host this long
	mu.Unlock()
	r.c.spawner.mu.Unlock()
	if e, ok := (<-done).(*errSpawn); !ok || e.detail != "deadline passed" {
		t.Fatalf("(2) the queued spawn: %v", e)
	}
	if r.gitCalls() != calls {
		t.Fatal("(2) git ran for a spawn already past its deadline")
	}
}

func ptr[T any](v T) *T { return &v }

// TestTmuxRefusalsAfterTheAllowList: each its own code and detail — a base
// that is not on the remote, a leftover branch (left untouched), no clone, two
// clones, a global @muster-dispatch.
func TestTmuxRefusalsAfterTheAllowList(t *testing.T) {
	t.Run("a base not on the remote", func(t *testing.T) {
		r := newSpawnRig(t)
		req := widgets(testPrompt)
		req.Base = "nope"
		if _, e := r.spawn(t, 31, req); e == nil || e.code != mirror.SpawnBadBase || e.detail != `branch "nope" does not exist on acme/widgets's remote` {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("a leftover branch", func(t *testing.T) {
		r := newSpawnRig(t)
		gitIn(t, r.clone, "branch", "muster/32-9-fix-the-typo", "HEAD")
		tip := gitIn(t, r.clone, "rev-parse", "muster/32-9-fix-the-typo")
		if _, e := r.spawn(t, 32, widgets(testPrompt)); e == nil || e.code != mirror.SpawnFailed || e.detail != "a leftover branch muster/32-9-fix-the-typo exists" {
			t.Fatalf("%+v", e)
		}
		if gitIn(t, r.clone, "rev-parse", "muster/32-9-fix-the-typo") != tip {
			t.Fatal("the leftover branch was touched")
		}
	})
	t.Run("no clone", func(t *testing.T) {
		r := newSpawnRig(t)
		r.c.cfg.AgentSpawn.Repos = []string{"acme/widgets", "acme/gadgets"}
		req := widgets(testPrompt)
		req.Repo = "acme/gadgets"
		if _, e := r.spawn(t, 33, req); e == nil || e.code != mirror.SpawnNoRepo || e.detail != "repo acme/gadgets has no clone under this host's roots" {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("two clones", func(t *testing.T) {
		r := newSpawnRig(t)
		second := filepath.Join(r.c.cfg.AgentSpawn.Roots[0], "widgets-2")
		gitIn(t, "", "clone", "-q", r.bare, second)
		gitIn(t, second, "remote", "set-url", "origin", "git@github.com:acme/widgets.git")
		if _, e := r.spawn(t, 34, widgets(testPrompt)); e == nil || e.code != mirror.SpawnNoRepo || e.detail != "repo acme/widgets has 2 clones under this host's roots" {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("a global tag", func(t *testing.T) {
		r := newSpawnRig(t)
		r.tmux(t, "set-option", "-g", mirror.DispatchOption, "5")
		if _, e := r.spawn(t, 35, widgets(testPrompt)); e == nil || e.code != mirror.SpawnUnavailable || !strings.Contains(e.detail, "global") {
			t.Fatalf("%+v", e)
		}
	})
}

// TestTmuxAForwardLandsOnlyInItsOwnTaggedWindow: the host's tag check, the real
// bound on forwarding. A paste naming the dispatch of a window tagged with it
// passes; a mismatched dispatch, and a window with no tag, are refused; the
// 13th in an hour is refused.
func TestTmuxAForwardLandsOnlyInItsOwnTaggedWindow(t *testing.T) {
	r := newSpawnRig(t)
	w, e := r.spawn(t, 41, widgets(testPrompt))
	if e != nil {
		t.Fatal(e)
	}
	untagged := r.tmux(t, "display-message", "-p", "-t", "=base:", "#{window_id}")
	h := &hostSide{c: r.c}
	ctx := context.Background()
	if err := h.checkDispatchPaste(ctx, mirror.Msg{Dispatch: 41, Window: w.Window}); err != nil {
		t.Fatalf("its own window: %v", err)
	}
	for _, m := range []mirror.Msg{{Dispatch: 42, Window: w.Window}, {Dispatch: 41, Window: untagged}, {Dispatch: 41}} {
		if err := h.checkDispatchPaste(ctx, m); err != errNotMusterWindow {
			t.Fatalf("dispatch %d into %q: %v; want refused", m.Dispatch, m.Window, err)
		}
	}
	for i := 2; i <= forwardsPerHour; i++ {
		if err := h.checkDispatchPaste(ctx, mirror.Msg{Dispatch: 41, Window: w.Window}); err != nil {
			t.Fatalf("forward %d: %v", i, err)
		}
	}
	if err := h.checkDispatchPaste(ctx, mirror.Msg{Dispatch: 41, Window: w.Window}); err != errForwardLimited {
		t.Fatalf("forward %d: %v; want limited", forwardsPerHour+1, err)
	}
}

// TestTmuxTheSweepRemovesOnlyWorktreesThatHoldNothing: three spawned
// worktrees whose windows are closed — clean (removed, with its branch), dirty
// (kept), one with an unpushed commit (kept) — and one whose window is still
// open (kept whatever it holds). Every trust entry is still present after (D5).
func TestTmuxTheSweepRemovesOnlyWorktreesThatHoldNothing(t *testing.T) {
	r := newSpawnRig(t)
	r.c.cfg.AgentSpawn.Rate = "100/10m"
	got := map[string]mirror.AgentSpawned{}
	for i, name := range []string{"clean", "dirty", "unpushed", "open"} {
		w, e := r.spawn(t, int64(51+i), mirror.AgentSpawnReq{Runtime: "claude", Repo: "acme/widgets", Prompt: testPrompt, Name: name})
		if e != nil {
			t.Fatal(e)
		}
		got[name] = w
	}
	_ = os.WriteFile(filepath.Join(got["dirty"].Worktree, "README"), []byte("work\n"), 0o644)
	gitIn(t, got["unpushed"].Worktree, "commit", "-q", "--allow-empty", "-m", "work")
	for _, name := range []string{"clean", "dirty", "unpushed"} {
		r.tmux(t, "kill-window", "-t", got[name].Window)
	}
	r.c.sweepOnce(context.Background())
	if _, err := os.Stat(got["clean"].Worktree); !os.IsNotExist(err) {
		t.Fatal("the clean worktree was kept")
	}
	if strings.Contains(gitIn(t, r.clone, "branch", "--list", got["clean"].Branch), "muster/") {
		t.Fatal("the clean worktree's branch was kept")
	}
	for _, name := range []string{"dirty", "unpushed", "open"} {
		if _, err := os.Stat(got[name].Worktree); err != nil {
			t.Fatalf("the %s worktree was removed", name)
		}
	}
	b, _ := os.ReadFile(r.c.cfg.AgentSpawn.ClaudeState)
	for name, w := range got {
		if !bytes.Contains(b, []byte(strconv.Quote(w.Worktree))) {
			t.Fatalf("the %s worktree's trust entry is gone", name)
		}
	}
}
