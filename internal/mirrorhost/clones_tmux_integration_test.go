//go:build tmuxit

package mirrorhost

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTmuxALiveWindowsCloneIsTheOneASpawnIsCutFrom is the SEAM, end to end on
// the host with real tmux and real git: two clones of acme/widgets under the
// root (the rig's, and a standalone one); a real tmux window whose shell
// works in a worktree of the rig's clone (outside the roots). The host's
// clone list — the agent-clones-list frame, through JSON — read by muster's
// rule (mirror.ClonesReplyResult, mirror.ChooseClone) names the rig's clone
// as the hint; the spawn carrying that hint is cut from it (its worktree's
// common dir is that clone's .git), never the standalone one. CONTROLS: the
// same spawn with no hint is refused no_repo (the host never guesses), and a
// hint the host did not list is refused bad_clone with no window opened.
func TestTmuxALiveWindowsCloneIsTheOneASpawnIsCutFrom(t *testing.T) {
	r := newSpawnRig(t)
	r.c.cfg.AgentSpawn.Rate = "100/10m"
	liveClone := realOrClean(r.clone)
	standalone := filepath.Join(r.c.cfg.AgentSpawn.Roots[0], "widgets-standalone")
	gitIn(t, "", "clone", "-q", r.bare, standalone)
	gitIn(t, standalone, "remote", "set-url", "origin", "https://github.com/acme/widgets")
	gitIn(t, standalone, "config", "url."+r.bare+".insteadOf", "https://github.com/acme/widgets")
	wt := filepath.Join(r.tmp, "owner-wt")
	gitIn(t, r.clone, "worktree", "add", "-q", "-b", "owner-side", wt)
	// The owner's live session: a window of their own session working in the
	// worktree.
	r.tmux(t, "new-window", "-d", "-t", "base", "-c", wt)
	waitFor(t, "the owner's window in its worktree", func() bool {
		ws, err := r.tm.Windows(context.Background())
		if err != nil {
			return false
		}
		for _, w := range ws {
			if realOrClean(strings.TrimSuffix(w.Cwd, deletedSuffix)) == realOrClean(wt) {
				return true
			}
		}
		return false
	})
	cl, err := r.c.listClones(context.Background(), r.tm, "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	// Across the wire: the frame the host sends, as muster reads it.
	raw, _ := json.Marshal(mirror.Msg{Type: mirror.TypeAgentClonesList, ID: 7, Clones: cl})
	var frame mirror.Msg
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	got, err := mirror.ClonesReplyResult(frame)
	if err != nil || len(got) != 2 {
		t.Fatalf("the clone list as muster reads it: %+v %v", got, err)
	}
	hint, choices := mirror.ChooseClone(got)
	if hint != liveClone || choices != nil {
		t.Fatalf("muster's choice: hint %q choices %+v; want the live sessions' clone %q", hint, choices, liveClone)
	}
	// CONTROL: no hint, two clones: refused, as before.
	if _, e := r.spawn(t, 61, widgets(testPrompt)); e == nil || e.code != mirror.SpawnNoRepo || e.detail != "repo acme/widgets has 2 clones under this host's roots" {
		t.Fatalf("no hint: %+v", e)
	}
	// CONTROL: a hint the host did not list: bad_clone, and no window.
	bad := widgets(testPrompt)
	bad.Clone = realOrClean(r.bare)
	if w, e := r.spawn(t, 62, bad); e == nil || e.code != mirror.SpawnBadClone || w.Window != "" {
		t.Fatalf("a hint not listed: %+v %+v", w, e)
	}
	req := widgets(testPrompt)
	req.Clone = hint
	w, e := r.spawn(t, 63, req)
	if e != nil {
		t.Fatalf("the hinted spawn: %+v", e)
	}
	common := strings.TrimSpace(gitIn(t, w.Worktree, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	if realOrClean(common) != filepath.Join(liveClone, ".git") {
		t.Fatalf("the spawn was cut from %s; want the live sessions' clone %s", common, liveClone)
	}
	if lines := r.auditLines(t); len(lines) == 0 || lines[len(lines)-3]["clone"] != hint {
		t.Fatalf("the audit request line does not record the hint: %v", lines[len(lines)-3])
	}
}
