//go:build opencodereal

package main

import (
	"os"
	"os/exec"
	"testing"
)

// TestTheRealOpencodeBinaryRunsALane is `ccd opencode-check` (cc-smoke step 13)
// against the opencode on PATH, as a Go test:
//
//	go test -tags opencodereal -run TestTheRealOpencodeBinaryRunsALane ./cmd/ccd/
//
// ⚠ BEHIND A BUILD TAG, NOT A SKIP: CI's test job has no opencode, and a skip
// there is a failure. CI runs the same check on the image's pinned binary
// (the claude-code-agent image smoke). Without an opencode on PATH this FAILS.
func TestTheRealOpencodeBinaryRunsALane(t *testing.T) {
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal("no opencode on PATH: this tagged test needs one")
	}
	old := os.Getenv(envAgentToken)
	t.Cleanup(func() { os.Setenv(envAgentToken, old) })
	if err := runOpencodeCheck(bin, execLauncher, t.Logf); err != nil {
		t.Fatal(err)
	}
}
