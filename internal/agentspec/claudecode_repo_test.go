package agentspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ccNoRepoGoldenPath pins a claude-code agent WITHOUT a repository, byte for byte.
//
// 🔴 IT WAS WRITTEN FROM THE CODE BEFORE REPOSITORY SUPPORT EXISTED (the base of
// feat/claude-code-agent-repo, ab08b60), so it is the "today" a repo-less
// claude-code agent — the live chief among them — must stay identical to. Two
// cases: a plain worker, and a chief-shaped one (instructions + the gateway
// sidecar). Like spec.golden.json it is evidence about CHANGE only; secrets are
// redacted to their names (goldenView).
const ccNoRepoGoldenPath = "testdata/claudecode-norepo.golden.json"

func TestAClaudeCodeSpecWithoutARepoIsUnchanged(t *testing.T) {
	plain := mustBuild(t, ccAgent(), ccConfig(), Options{})
	chief := mustBuild(t, ccAgent(), sidecarConfig(), Options{Instructions: "# chief\n\nprose\n", GatewaySidecar: &GatewaySidecar{}})
	got, err := json.MarshalIndent(map[string]any{"plain": goldenView(plain), "chief": goldenView(chief)}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *goldenWrite {
		if err := os.WriteFile(filepath.FromSlash(ccNoRepoGoldenPath), got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("wrote %s; this run asserted nothing — re-run without -golden-write", ccNoRepoGoldenPath)
	}
	want, err := os.ReadFile(filepath.FromSlash(ccNoRepoGoldenPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("a claude-code spec with NO repository changed (it must be byte-identical to the pre-repo build).\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
