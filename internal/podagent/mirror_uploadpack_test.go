package podagent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision/worktreetest"
)

// TestEveryGitAMirrorCheckoutRunsCarriesTheMirrorsUploadPack: the CALL SITES —
// the clone and the empty-repository check (ls-remote) — each run git with
// --upload-pack=<MirrorUploadPack>, read from what git was actually handed (a
// recording shim first on PATH). 🔴 This is the guard: on a git newer than the
// image's, both succeed without the flag, so a refactor that dropped it would
// otherwise stay green here and break every deploy-key pool on git 2.39. Each
// call is required to HAPPEN (counted), so "no matching call" cannot pass.
func TestEveryGitAMirrorCheckoutRunsCarriesTheMirrorsUploadPack(t *testing.T) {
	localGitHub(t)
	mirrors := t.TempDir()
	publish(t, mirrors, "m/repo", map[string]string{"README": "from the mirror\n"})
	mirror := filepath.Join(mirrors, "m", "repo.git")
	want, err := MirrorUploadPack(mirror)
	if err != nil {
		t.Fatal(err)
	}
	log := worktreetest.GitShim(t, os.Getenv("PATH"))

	c, err := NewCheckout(CheckoutConfig{Repo: "o/r", Dir: filepath.Join(t.TempDir(), "work"), Mirror: mirror})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Attempt(ctx); err != nil {
		t.Fatalf("clone: %v", err)
	}
	c.remoteHasNoRefs(ctx)

	seen := map[string]int{}
	for _, argv := range worktreetest.GitShimCalls(t, log) {
		sub := worktreetest.GitSubcommand(argv)
		if sub != "clone" && sub != "ls-remote" {
			continue
		}
		seen[sub]++
		if !slices.Contains(argv, "--upload-pack="+want) {
			t.Errorf("MIRROR UPLOAD-PACK MISSING: git %s ran without --upload-pack=%q: %q", sub, want, argv)
		}
	}
	// And the clone carries every other flag the image's smoke pins and runs
	// (CLONE_FLAGS), --no-local above all.
	for _, argv := range worktreetest.GitShimCalls(t, log) {
		if worktreetest.GitSubcommand(argv) != "clone" {
			continue
		}
		for _, f := range strings.Fields(smokePins(t)["CLONE_FLAGS"]) {
			if !slices.Contains(argv, f) {
				t.Errorf("the clone lacks %s, which the image's smoke pins: %q", f, argv)
			}
		}
	}
	if seen["clone"] != 1 || seen["ls-remote"] != 1 {
		t.Fatalf("instrument: the shim saw clone x%d, ls-remote x%d; want one of each", seen["clone"], seen["ls-remote"])
	}
}
