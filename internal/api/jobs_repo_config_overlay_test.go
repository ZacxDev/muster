package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/jobs"
)

// A JOB'S `repo_config_overlay` (jobs.go): false reaches the worktree as
// "no repository configuration" (PrepareJobWorktree's skipRepoConfig, whose
// script test is in internal/agentprovision); absent or true is the overlay as
// before; it is refused where it means nothing; and a replay that changes it is
// a 409.

// TestRepoConfigOverlayFalseReachesTheWorktree: a sha job submitted with
// repo_config_overlay false is made WITHOUT the repository's configuration and
// reads back false; one sent true, and one that leaves it out, are made with
// the overlay and read back true.
func TestRepoConfigOverlayFalseReachesTheWorktree(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	base := func(extra map[string]any) map[string]any {
		b := map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	for i, tc := range []struct {
		body map[string]any
		want bool
	}{
		{base(map[string]any{"repo_config_overlay": false}), false},
		{base(map[string]any{"repo_config_overlay": true}), true},
		{base(nil), true},
	} {
		out := f.await(t, f.submit(t, tc.body))
		if out["status"] != jobs.StatusSucceeded || out["repo_config_overlay"] != tc.want {
			t.Fatalf("job %d = %v (repo_config_overlay %v), want succeeded with %t", i+1, out["status"], out["repo_config_overlay"], tc.want)
		}
	}
	made, _ := f.ws.snapshot()
	want := []string{
		"keen-wren#1 acme/widgets#7@" + testFullSHA + " no-repo-config",
		"keen-wren#2 acme/widgets#7@" + testFullSHA,
		"keen-wren#3 acme/widgets#7@" + testFullSHA,
	}
	if strings.Join(made, "|") != strings.Join(want, "|") {
		t.Fatalf("worktrees made = %q, want %q", made, want)
	}
}

// TestRepoConfigOverlaySubmissionsTheAPIRefuses: false without a sha (no
// worktree to skip it in), false with a config_ref (which names a configuration
// to overlay), and a non-boolean are 400s, recording nothing; true without a
// sha is accepted (it is the default).
func TestRepoConfigOverlaySubmissionsTheAPIRefuses(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"false without a sha", map[string]any{"agent": "keen-wren", "prompt": "x", "repo_config_overlay": false},
			"repo_config_overlay false is only for a job with a sha"},
		{"false with a config_ref", map[string]any{"agent": "keen-wren", "prompt": "x", "repo": "acme/widgets", "sha": testFullSHA,
			"pr": 7, "config_ref": testConfigRef, "repo_config_overlay": false}, "config_ref names the agent configuration to overlay"},
	} {
		code, out := f.call(t, http.MethodPost, "/api/jobs", tc.body, true)
		if msg, _ := out["error"].(string); code != http.StatusBadRequest || !strings.HasPrefix(msg, tc.want) {
			t.Errorf("%s: POST = %d %v, want 400 starting %q", tc.name, code, out, tc.want)
		}
	}
	if code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "prompt": "x",
		"repo_config_overlay": "no"}, true); code != http.StatusBadRequest {
		t.Errorf("a string repo_config_overlay: POST = %d %v, want 400", code, out)
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("refused submissions recorded %d job(s)", n)
	}
	f.submit(t, map[string]any{"agent": "keen-wren", "prompt": "x", "repo_config_overlay": true})
}

// TestAReplayWithADifferentRepoConfigOverlayIsRefused: a key first used with
// repo_config_overlay false, replayed without it (true) is a 409 naming the
// difference; replayed with false it is the deduplicated job.
func TestAReplayWithADifferentRepoConfigOverlayIsRefused(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.gw.reply = "done"
	first := map[string]any{"agent": "keen-wren", "prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7,
		"repo_config_overlay": false, "idempotency_key": "review-overlay"}
	id := f.submit(t, first)
	f.await(t, id)
	again := map[string]any{}
	for k, v := range first {
		again[k] = v
	}
	delete(again, "repo_config_overlay")
	code, out := f.call(t, http.MethodPost, "/api/jobs", again, true)
	if msg, _ := out["error"].(string); code != http.StatusConflict || out["idempotency_mismatch"] != true ||
		!strings.Contains(msg, "has repo_config_overlay false, not true") {
		t.Fatalf("replay without it = %d %v, want 409 naming repo_config_overlay", code, out)
	}
	code, out = f.call(t, http.MethodPost, "/api/jobs", first, true)
	if code != http.StatusOK || out["deduplicated"] != true || out["job_id"] != float64(id) {
		t.Fatalf("the same replay = %d %v, want 200 deduplicated job %d", code, out, id)
	}
}
