package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE SUCCESSOR'S PERMISSION WORDING (-agent-spawn-skip-permissions): a
// succession start is a host spawn like agent_dispatch's, so the Claude Code
// successor is told what its host's hello says (hostSkipsPermissions), and the
// kickoff room leaves space for the LONGER sentence. The sentences are pinned
// whole as literals (the same texts as
// TestAHostThatSkipsPermissionsIsDescribedAsSkippingThem), never the constants.

const (
	sxClaudeSkip   = "You run without permission prompts (Claude Code's bypassPermissions mode): you use tools without asking, and there is no owner approval step, so stay inside the task."
	sxClaudeNormal = "You run in normal permission mode: your owner answers your asks on muster."
)

// TestASuccessorOnAHostThatSkipsPermissionsIsToldItSkipsThem: on host-echo
// saying it skips them, the tapped successor (a Claude Code one, by target)
// is told it runs without permission prompts and NOT that it asks; on the same
// host saying nothing, it is told normal permission mode and NOT the skip one.
func TestASuccessorOnAHostThatSkipsPermissionsIsToldItSkipsThem(t *testing.T) {
	for _, c := range []struct {
		skip      bool
		want, not string
	}{{true, sxClaudeSkip, sxClaudeNormal}, {false, sxClaudeNormal, sxClaudeSkip}} {
		f := newSuccFixture(t)
		f.m.mu.Lock()
		for i := range f.m.hosts {
			f.m.hosts[i].AgentSpawnSkipPermissions = c.skip && f.m.hosts[i].Host == "host-echo"
		}
		f.m.mu.Unlock()
		f.viewLine(t, sxOpenCodeKey, "continue it in a Claude Code session")
		text, id := f.propose(t, map[string]any{"successor_of": sxOpenCodeKey, "succession": "spinoff",
			"prompt_from": map[string]any{"ref": sxRefFenc, "block": 0}, "target": "host/host-echo/claude", "repo": "acme/widgets"})
		if id <= 0 {
			t.Fatalf("skip %v: the proposal: %s", c.skip, text)
		}
		if w := f.tapAs(t, f.ownerSession(), id, "@3", "Start it"); w.Code != http.StatusNoContent {
			t.Fatalf("skip %v: the tap: %d %s", c.skip, w.Code, w.Body)
		}
		calls := f.m.spawnCalls()
		if len(calls) != 1 || calls[0].req.Runtime != mirror.SpawnRuntimeClaude || calls[0].host != "host-echo" {
			t.Fatalf("skip %v: the spawns %+v; want one claude on host-echo", c.skip, calls)
		}
		if p := calls[0].req.Prompt; !strings.Contains(p, c.want) || strings.Contains(p, c.not) || !strings.Contains(p, "\n---\n"+sxBlock0+"\n---") {
			t.Fatalf("skip %v: the successor's first message:\n%s\nwant %q and not %q", c.skip, p, c.want, c.not)
		}
	}
}

// TestTheKickoffRoomFitsTheLongerPermissionSentence: a Claude Code kickoff of
// exactly successorPromptRoom bytes makes a first message of EXACTLY
// mirror.MaxSpawnPrompt bytes with the skip sentence (the longer, so it binds),
// and one shorter by exactly the two sentences' difference with the normal-mode
// one — the room is the same whichever the host says when the owner taps.
func TestTheKickoffRoomFitsTheLongerPermissionSentence(t *testing.T) {
	x := plugins.Proposal{SubjectKey: sxClaudeKey, SubjectRuntime: "claude", SubjectHost: "host-echo", SubjectTitle: "the fixture session"}
	room := successorPromptRoom(mirror.SpawnRuntimeClaude, "host-echo", "acme/widgets", x)
	if room <= 0 {
		t.Fatalf("instrument check: room %d", room)
	}
	kickoff := strings.Repeat("k", room)
	skip := successorPrompt(mirror.SpawnRuntimeClaude, "host-echo", "acme/widgets", x, kickoff, true)
	normal := successorPrompt(mirror.SpawnRuntimeClaude, "host-echo", "acme/widgets", x, kickoff, false)
	if !strings.Contains(skip, sxClaudeSkip) || !strings.Contains(normal, sxClaudeNormal) {
		t.Fatal("instrument check: the prompts do not carry the pinned sentences")
	}
	if len(skip) != mirror.MaxSpawnPrompt {
		t.Fatalf("with the skip sentence a room-sized kickoff makes %d bytes; want exactly %d", len(skip), mirror.MaxSpawnPrompt)
	}
	if d := mirror.MaxSpawnPrompt - len(normal); d != len(sxClaudeSkip)-len(sxClaudeNormal) {
		t.Fatalf("with the normal sentence it is %d short of the limit; want %d", d, len(sxClaudeSkip)-len(sxClaudeNormal))
	}
}
