package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// WHETHER A JOB'S WORKTREE GETS THE REPOSITORY'S AGENT CONFIGURATION IS THE
// POOL'S, NOT THE JOB'S. A pool with a deploy key never gets it (the adapter
// strips it: internal/agentprovision); the job API has no field for it, and a
// config_ref — which names a configuration to overlay — is refused on such a
// pool rather than silently ignored.

// noOverlayPool is a pool the fixture's workspace strips the configuration for.
const noOverlayPool = "private-review"

func init() {
	jobAgentRows["priv-a"] = agents.Agent{ID: 30, Name: "priv-a", Kind: agents.KindClaudeCode, CCAccount: "alpha",
		Repo: "acme/widgets", Status: agents.StatusRunning, WorkerPool: noOverlayPool}
}

// StripsRepoConfig answers for the fixture's pools.
func (w *jobWorkspace) StripsRepoConfig(a agents.Agent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stripPools[a.WorkerPool]
}

// TestRepoConfigOverlayIsNotAJobField: the field is gone — a body carrying it
// is an unknown field (400), recording nothing, whatever its value.
func TestRepoConfigOverlayIsNotAJobField(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	for _, v := range []any{false, true} {
		code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"agent": "keen-wren", "prompt": "x",
			"repo": "acme/widgets", "sha": testFullSHA, "pr": 7, "repo_config_overlay": v}, true)
		if msg, _ := out["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "repo_config_overlay") {
			t.Errorf("repo_config_overlay %v: POST = %d %v, want 400 naming the unknown field", v, code, out)
		}
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("refused submissions recorded %d job(s)", n)
	}
}

// TestAConfigRefOnANoOverlayPoolIsRefused: a sha job with a config_ref,
// submitted to a pool whose workers never get the repository's configuration —
// by the pool's name, or naming one of its workers — is a 409 that names the
// pool and config_ref, recording nothing. Controls: the same body on a pool
// that overlays is accepted, and so is the no-overlay pool's job without the
// config_ref.
func TestAConfigRefOnANoOverlayPoolIsRefused(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.ext.Kinds = &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}}
	f.ws.stripPools = map[string]bool{noOverlayPool: true}
	f.gw.reply = "done"
	sha := func(target map[string]any, cfg bool) map[string]any {
		b := map[string]any{"prompt": "review", "repo": "acme/widgets", "sha": testFullSHA, "pr": 7}
		if cfg {
			b["config_ref"] = testConfigRef
		}
		for k, v := range target {
			b[k] = v
		}
		return b
	}
	for name, target := range map[string]map[string]any{
		"by pool":  {"worker_pool": noOverlayPool},
		"by agent": {"agent": "priv-a"},
	} {
		code, out := f.call(t, http.MethodPost, "/api/jobs", sha(target, true), true)
		msg, _ := out["error"].(string)
		if code != http.StatusConflict || !strings.Contains(msg, "config_ref") || !strings.Contains(msg, noOverlayPool) {
			t.Errorf("%s: POST = %d %v, want 409 naming config_ref and the pool", name, code, out)
		}
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("refused submissions recorded %d job(s)", n)
	}
	f.await(t, f.submit(t, sha(map[string]any{"worker_pool": "reviewers"}, true)))
	f.await(t, f.submit(t, sha(map[string]any{"worker_pool": noOverlayPool}, false)))
}
