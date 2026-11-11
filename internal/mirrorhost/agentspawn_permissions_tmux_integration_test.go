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
// chief), and does not with the setting off.
func TestTmuxTheHelloSaysWhetherSpawnedAgentsSkipPermissionPrompts(t *testing.T) {
	r := newOCRig(t)
	for _, keep := range []bool{false, true} {
		r.c.cfg.AgentSpawn.KeepPermissionPrompts = keep
		h := r.driven(t, true).of(mirror.TypeHello)[0]
		if len(h.AgentSpawn) == 0 || h.AgentSpawnSkipPermissions != !keep {
			t.Fatalf("keep %v: the hello offers %q with AgentSpawnSkipPermissions=%v; want %v", keep, h.AgentSpawn, h.AgentSpawnSkipPermissions, !keep)
		}
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
