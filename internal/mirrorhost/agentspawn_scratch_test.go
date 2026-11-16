package mirrorhost

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// OFFICERS ON COMPOSE, THE HOST'S HALF (officers redesign O-B): a scratch
// spawn for an officer with no repository, the officers' own rate ceiling,
// and the readiness the hello advertises. The scratch spawn against real
// tmux is agentspawn_tmux_integration_test.go's
// TestTmuxAScratchSpawnRunsInAPrivateDirectoryAndIsSweptAway.

const officerPrompt = mirror.OfficerSpawnPreface + ".\n\nYou are officer sentinel:sessions"

// scratchReq is an officer's scratch spawn.
func scratchReq(prompt string) mirror.AgentSpawnReq {
	return mirror.AgentSpawnReq{Runtime: mirror.SpawnRuntimeClaude, Prompt: prompt, Name: "officer-sentinel-sessions-1", Scratch: true}
}

// TestAScratchSpawnIsAnOfficersOnAPlainAgentSpawnHost: scratch spawns are ON
// with -agent-spawn itself (O-B round 0, the operator's Q1: there is no
// -agent-spawn-scratch). A scratch spawn is still refused, before any git
// call or file, for a CHIEF's prompt (only an officer starts with no
// repository) and for a scratch request that names a repository or base. Each
// refusal is its own detail. The positive control: an officer's scratch
// request on a plain -agent-spawn host — no allow-list at all — passes every
// one of these and is refused only later, at tmux (the hermetic unit has no
// tmux server).
func TestAScratchSpawnIsAnOfficersOnAPlainAgentSpawnHost(t *testing.T) {
	for _, c := range []struct {
		name   string
		req    mirror.AgentSpawnReq
		code   string
		detail string
	}{
		{"a chief's prompt", scratchReq(testPrompt), mirror.SpawnNoRepo, "only an officer instance starts with no repository"},
		{"a scratch spawn naming a repo", func() mirror.AgentSpawnReq { r := scratchReq(officerPrompt); r.Repo = "acme/widgets"; return r }(),
			mirror.SpawnFailed, "a scratch spawn names no repository, base or clone, and runs claude (a muster bug)"},
		{"a scratch spawn naming a base", func() mirror.AgentSpawnReq { r := scratchReq(officerPrompt); r.Base = "main"; return r }(),
			mirror.SpawnFailed, "a scratch spawn names no repository, base or clone, and runs claude (a muster bug)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newSpawnUnit(t)
			_, e := u.spawn(t, 71, c.req)
			if e == nil || e.code != c.code || e.detail != c.detail {
				t.Fatalf("refusal %+v; want %s %q", e, c.code, c.detail)
			}
			if n := u.gitCalls(); n != 0 {
				t.Fatalf("git ran %d times for a refused scratch request", n)
			}
			if _, err := os.Stat(filepath.Join(u.dir, scratchDirName)); !os.IsNotExist(err) {
				t.Fatalf("a refused scratch request made the scratch root: %v", err)
			}
		})
	}
	u := newSpawnUnit(t)
	u.c.cfg.AgentSpawn.Repos = nil
	_, e := u.spawn(t, 72, scratchReq(officerPrompt))
	if e == nil || e.code != mirror.SpawnUnavailable || e.detail != "no tmux server" {
		t.Fatalf("control: an officer's scratch spawn on a plain -agent-spawn host was refused %+v; want it to get as far as tmux (no tmux server)", e)
	}
	if n := u.gitCalls(); n != 0 {
		t.Fatalf("control: a scratch spawn ran git %d times", n)
	}
}

// TestOfficerSpawnsHaveTheirOwnRateCeiling: the chief's -agent-spawn-rate
// counts the chief's spawns only, and -agent-spawn-officer-rate the officers'
// — so a host whose chief spent its 3 per 10 minutes still starts an officer,
// and the officers' ceiling refuses an officer once ITS admissions reach it,
// while a chief spawn still passes it. Both directions, counted from the
// audit log's admit lines. A line with NO officer flag (a build before the
// ceiling; O-B round 0) counts against neither: three of them do not spend the
// chief's 3 per 10 minutes, nor the officers' 2, while three flagged chief
// lines do (the control).
func TestOfficerSpawnsHaveTheirOwnRateCeiling(t *testing.T) {
	now := time.Now()
	officerWidgets := widgets(officerPrompt)
	for _, c := range []struct {
		name        string
		officerRate string
		chief       int // chief admits in the last minutes
		officer     int // officer admits in the last minutes
		legacy      int // admits with no officer field, in the last minutes
		req         mirror.AgentSpawnReq
		want        string // the rate refusal, "" for none
	}{
		{"the chief's limit spent: an officer still passes", "3/10m", 3, 0, 0, officerWidgets, ""},
		{"the chief's limit spent: the chief is refused (control)", "", 3, 0, 0, widgets(testPrompt), "rate limited: 3 spawns in the last 10 minutes"},
		{"the officers' ceiling spent: an officer is refused", "2/10m", 0, 2, 0, officerWidgets, "rate limited: 2 officer spawns in the last 10 minutes (this host's -agent-spawn-officer-rate)"},
		{"the officers' ceiling spent: the chief still passes", "2/10m", 0, 2, 0, widgets(testPrompt), ""},
		{"under the officers' ceiling", "3/10m", 0, 2, 0, officerWidgets, ""},
		{"older unflagged lines do not spend the chief's limit", "", 0, 0, 3, widgets(testPrompt), ""},
		{"older unflagged lines do not spend the officers' ceiling", "2/10m", 0, 0, 3, officerWidgets, ""},
		{"older unflagged lines beside a spent chief limit (control)", "", 3, 0, 3, widgets(testPrompt), "rate limited: 3 spawns in the last 10 minutes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newSpawnUnit(t)
			u.c.cfg.AgentSpawn.OfficerRate = c.officerRate
			u.c.cfg.AgentSpawn.Now = func() time.Time { return now }
			u.c.cfg.AgentSpawn.Roots = nil // past the limit, the next refusal is the clone's
			for i := 0; i < c.chief; i++ {
				if err := appendAudit(u.log, spawnAuditAdmit{At: now.Add(-time.Minute), Kind: "admit", Dispatch: int64(10 + i)}); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < c.officer; i++ {
				if err := appendAudit(u.log, spawnAuditAdmit{At: now.Add(-time.Minute), Kind: "admit", Dispatch: int64(20 + i), Officer: true}); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < c.legacy; i++ {
				// The older build's line, as it wrote it: no officer field.
				legacy := map[string]any{"at": now.Add(-time.Minute), "kind": "admit", "dispatch": 30 + i}
				if err := appendAudit(u.log, legacy); err != nil {
					t.Fatal(err)
				}
			}
			_, e := u.spawn(t, 45, c.req)
			if c.want != "" {
				if e == nil || e.code != mirror.SpawnUnavailable || e.detail != c.want {
					t.Fatalf("refusal %+v; want unavailable %q", e, c.want)
				}
				return
			}
			if e == nil || strings.HasPrefix(e.detail, "rate limited") {
				t.Fatalf("refused by a limit that is not its own: %+v", e)
			}
			// It was admitted: its admit line carries the officer flag.
			var last map[string]any
			for _, l := range u.auditLines(t) {
				if l["kind"] == "admit" && l["dispatch"] == float64(45) {
					last = l
				}
			}
			if flag, ok := last["officer"].(bool); last == nil || !ok || flag != strings.HasPrefix(c.req.Prompt, mirror.OfficerSpawnPreface) {
				t.Fatalf("the admission line %v does not say whose spawn it was (a chief's says officer false, never nothing)", last)
			}
		})
	}
}

// TestTheSweepRemovesAScratchDirectoryOnceItsWindowIsGone: a scratch spawn's
// directory is private to its instance and holds nothing of the owner's, so
// the sweep removes it whole once no live window names its dispatch — and
// keeps one a live window still names. Neither touches a repository's
// worktree directory.
func TestTheSweepRemovesAScratchDirectoryOnceItsWindowIsGone(t *testing.T) {
	u := newSpawnUnit(t)
	root := filepath.Join(u.c.spawnDir(), scratchDirName)
	for _, d := range []string{"81", "82"} {
		if err := os.MkdirAll(filepath.Join(root, d, "notes"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "notes", "draft.md"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	u.c.SweepAgentWorktrees(context.Background(), map[int64]bool{81: true})
	if _, err := os.Stat(filepath.Join(root, "81", "notes", "draft.md")); err != nil {
		t.Fatalf("the live instance's scratch directory was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "82")); !os.IsNotExist(err) {
		t.Fatalf("the gone instance's scratch directory is still there: %v", err)
	}
}

// TestTheReadinessSaysWhatTheHostCanStart: the hello's readiness carries the
// allow-list, scratch spawns, whether claude resolves (with what to do when it
// does not: -claude-path) and Claude Code's own login answer — yes, no, or
// unknown when claude cannot say — and nothing of that answer's account.
func TestTheReadinessSaysWhatTheHostCanStart(t *testing.T) {
	dir := t.TempDir()
	stub := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	in := stub("claude-in", `[ "$1 $2 $3" = "auth status --json" ] && printf '{"loggedIn": true, "email": "someone@example.invalid"}' || exit 3`)
	out := stub("claude-out", `printf '{"loggedIn": false, "authMethod": "none"}'`)
	old := stub("claude-old", `echo "error: unknown command 'auth'" >&2; exit 1`)
	noexec := stub("claude-noexec", `printf '{"loggedIn": true}'`)
	if err := os.Chmod(noexec, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, bin, claude, login string
	}{
		{"logged in", in, mirror.ClaudeFound, mirror.LoginYes},
		{"not logged in", out, mirror.ClaudeFound, mirror.LoginNo},
		{"an older claude", old, mirror.ClaudeFound, mirror.LoginUnknown},
		{"no claude (a relative -claude-path)", "claude", mirror.ClaudeMissing, mirror.LoginUnknown},
		// O-B round 0: a set -claude-path is checked, not trusted.
		{"no claude (a -claude-path naming nothing)", filepath.Join(dir, "claude-absent"), mirror.ClaudeMissing, mirror.LoginUnknown},
		{"no claude (a -claude-path that is not executable)", noexec, mirror.ClaudeMissing, mirror.LoginUnknown},
		{"no claude (a -claude-path that is a directory)", dir, mirror.ClaudeMissing, mirror.LoginUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newSpawnUnit(t)
			u.c.cfg.AgentSpawn.ClaudeBin = c.bin
			u.c.cfg.AgentSpawn.Repos = []string{"acme/widgets", "acme/*"}
			h := &hostSide{c: u.c, ctx: context.Background(), version: 2}
			r := h.spawnReadiness()
			if r == nil || r.Claude != c.claude || r.Login != c.login || !r.Scratch || !slices.Equal(r.Repos, []string{"acme/widgets", "acme/*"}) {
				t.Fatalf("readiness %+v; want claude %s, login %s, scratch spawns offered (every -agent-spawn host), the allow-list", r, c.claude, c.login)
			}
			if c.claude == mirror.ClaudeMissing && !strings.Contains(r.ClaudeDetail, "-claude-path") {
				t.Errorf("a missing claude's detail %q does not say how to point at it", r.ClaudeDetail)
			}
			if c.claude == mirror.ClaudeMissing && filepath.IsAbs(c.bin) && !strings.Contains(r.ClaudeDetail, c.bin) {
				t.Errorf("a set -claude-path that is not a binary: the detail %q does not name %s", r.ClaudeDetail, c.bin)
			}
			if strings.Contains(r.ClaudeDetail+r.Unavailable, "example.invalid") {
				t.Errorf("the readiness carries the login's account: %+v", r)
			}
		})
	}
	// A read-only host advertises none (a spawn is a write).
	u := newSpawnUnit(t)
	u.c.cfg.Interactive = false
	if r := (&hostSide{c: u.c, ctx: context.Background(), version: 2}).spawnReadiness(); r != nil {
		t.Fatalf("a read-only host advertised %+v", r)
	}
	// A host with -agent-spawn off says so, offers no scratch, and never asks
	// claude for its login (O-B round 0): the stub records each run. The
	// control: the same stub on a spawning host is asked once.
	runs := filepath.Join(dir, "runs")
	counting := stub("claude-counting", `echo run >> `+runs+`; printf '{"loggedIn": true}'`)
	ran := func() int {
		b, _ := os.ReadFile(runs)
		return strings.Count(string(b), "run")
	}
	u = newSpawnUnit(t)
	u.c.cfg.AgentSpawn.Enabled, u.c.cfg.AgentSpawn.ClaudeBin = false, counting
	if r := (&hostSide{c: u.c, ctx: context.Background(), version: 2}).spawnReadiness(); r == nil || r.Unavailable != "agent spawn is off" || r.Scratch || r.Login != mirror.LoginUnknown {
		t.Fatalf("a host with spawn off advertised %+v", r)
	}
	if n := ran(); n != 0 {
		t.Fatalf("a host with spawn off ran claude %d time(s) for its login", n)
	}
	u.c.cfg.AgentSpawn.Enabled = true
	if r := (&hostSide{c: u.c, ctx: context.Background(), version: 2}).spawnReadiness(); r == nil || r.Login != mirror.LoginYes || ran() != 1 {
		t.Fatalf("control: a spawning host's readiness %+v after %d run(s); want logged in, from one run", r, ran())
	}
}

// TestAChangedReadinessIsSentAgain: the connection re-reads its readiness on
// its tick and sends a spawn-ready frame only when it CHANGED (a login done
// meanwhile) — never the same one again.
func TestAChangedReadinessIsSentAgain(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "logged")
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nif [ -e "+state+" ]; then printf '{\"loggedIn\":true}'; else printf '{\"loggedIn\":false}'; fi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := newSpawnUnit(t)
	u.c.cfg.AgentSpawn.ClaudeBin = bin
	u.c.cfg.SpawnReadyEvery = 20 * time.Millisecond
	u.c.log = log.New(io.Discard, "", 0)
	sent := make(chan mirror.Msg, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &hostSide{c: u.c, ctx: ctx, version: 2}
	first := h.spawnReadiness()
	if first.Login != mirror.LoginNo {
		t.Fatalf("the first readiness %+v", first)
	}
	go h.watchSpawnReady(first, func(m mirror.Msg) error { sent <- m; return nil })
	select {
	case m := <-sent:
		t.Fatalf("an unchanged readiness was sent again: %+v", m)
	case <-time.After(150 * time.Millisecond):
	}
	if err := os.WriteFile(state, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-sent:
		if m.Type != mirror.TypeSpawnReady || m.SpawnReady == nil || m.SpawnReady.Login != mirror.LoginYes {
			t.Fatalf("sent %+v; want a spawn-ready saying logged in", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the changed readiness was never sent")
	}
}
