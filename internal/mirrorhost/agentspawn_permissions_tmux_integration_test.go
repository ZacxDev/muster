//go:build tmuxit

package mirrorhost

import (
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// -agent-spawn-skip-permissions=false (AgentSpawnConfig.KeepPermissionPrompts)
// against REAL tmux, through the whole spawn: each runtime's process sees
// EXACTLY the argv it saw before the setting existed — no permission word at
// all. The default-on half is pinned by the two feature tests
// (TestTmuxASpawnedClaudeGetsThePromptVerbatimInATaggedWorktree and
// TestTmuxASpawnedOpenCodeRunsItsModelAndGetsThePromptAfterTheBind), whose
// rigs leave the setting at its default.

func TestTmuxASpawnWithPermissionPromptsKeptRunsClaudeAsBefore(t *testing.T) {
	r := newSpawnRig(t)
	r.c.cfg.AgentSpawn.KeepPermissionPrompts = true
	prompt := mirror.SpawnPreface + " keep the prompts"
	w, e := r.spawn(t, 41, mirror.AgentSpawnReq{Runtime: "claude", Repo: "acme/widgets", Prompt: prompt, Name: "#9 Fix the typo!"})
	if e != nil {
		t.Fatalf("refused %+v", e)
	}
	if got, want := r.argvOf(t, 41), []string{"--session-id", w.AgentSession, "--", prompt}; !slices.Equal(got, want) {
		t.Fatalf("the stub claude saw %q; want exactly %q (no permission flag)", got, want)
	}
}

// TestTmuxTheHelloSaysWhetherSpawnedAgentsSkipPermissionPrompts: a host that
// spawns says so in its hello by default (what muster tells the agent and the
// chief), and does not with the setting off — nor as root (Geteuid 0), where
// its claude keeps its prompts.
func TestTmuxTheHelloSaysWhetherSpawnedAgentsSkipPermissionPrompts(t *testing.T) {
	r := newOCRig(t)
	for _, c := range []struct {
		keep bool
		euid int
		want bool
	}{{false, 1000, true}, {true, 1000, false}, {false, 0, false}, {true, 0, false}} {
		euid := c.euid
		r.c.cfg.AgentSpawn.KeepPermissionPrompts = c.keep
		r.c.cfg.AgentSpawn.Geteuid = func() int { return euid }
		h := r.driven(t, true).of(mirror.TypeHello)[0]
		if len(h.AgentSpawn) == 0 || h.AgentSpawnSkipPermissions != c.want {
			t.Fatalf("keep %v euid %d: the hello offers %q with AgentSpawnSkipPermissions=%v; want %v", c.keep, c.euid, h.AgentSpawn, h.AgentSpawnSkipPermissions, c.want)
		}
	}
}

// TestTmuxARootMirrorRunsClaudeAsWithTheSettingOff: as root (Geteuid 0) with
// the setting at its default, a spawned claude sees EXACTLY the setting-off
// argv (Claude Code refuses bypassPermissions as root), while a spawned
// opencode still gets --auto.
func TestTmuxARootMirrorRunsClaudeAsWithTheSettingOff(t *testing.T) {
	r := newSpawnRig(t)
	r.c.cfg.AgentSpawn.Geteuid = func() int { return 0 }
	prompt := mirror.SpawnPreface + " as root"
	w, e := r.spawn(t, 43, mirror.AgentSpawnReq{Runtime: "claude", Repo: "acme/widgets", Prompt: prompt, Name: "#9 Fix the typo!"})
	if e != nil {
		t.Fatalf("refused %+v", e)
	}
	if got, want := r.argvOf(t, 43), []string{"--session-id", w.AgentSession, "--", prompt}; !slices.Equal(got, want) {
		t.Fatalf("the stub claude saw %q; want exactly %q (no permission flag as root)", got, want)
	}
	o := newOCRig(t)
	o.c.cfg.AgentSpawn.Geteuid = func() int { return 0 }
	ow, oe := o.spawn(t, 44, ocWidgets(mirror.SpawnPreface+" as root"))
	if oe != nil {
		t.Fatalf("opencode refused %+v", oe)
	}
	got := o.recOf(t, "oc-"+strconv.FormatInt(44, 10))
	if want := []string{"opencode", "--opencode-bin", filepath.Join(o.bin, "opencode"), "-s", ow.AgentSession, "--auto"}; !slices.Equal(got, want) {
		t.Fatalf("the launcher saw %q; want exactly %q (--auto as root too)", got, want)
	}
}

func TestTmuxASpawnWithPermissionPromptsKeptRunsOpenCodeAsBefore(t *testing.T) {
	r := newOCRig(t)
	r.c.cfg.AgentSpawn.KeepPermissionPrompts = true
	w, e := r.spawn(t, 42, ocWidgets(mirror.SpawnPreface+" keep the prompts"))
	if e != nil {
		t.Fatalf("refused %+v", e)
	}
	got := r.recOf(t, "oc-"+strconv.FormatInt(42, 10))
	if want := []string{"opencode", "--opencode-bin", filepath.Join(r.bin, "opencode"), "-s", w.AgentSession}; !slices.Equal(got, want) {
		t.Fatalf("the launcher saw %q; want exactly %q (no --auto)", got, want)
	}
}
