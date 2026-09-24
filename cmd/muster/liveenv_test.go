//go:build liveenv

// THE TWO TESTS HERE ONLY MEAN ANYTHING INSIDE A REAL CODING-AGENT SESSION, AND
// THAT IS WHY THEY SIT BEHIND A BUILD TAG RATHER THAN A t.Skip.
//
// 🔴 WHAT THEY BUY, so nobody deletes them as dead weight: both read the session
// id out of the REAL process environment via sessionIDFrom(os.Getenv). A
// fake-based test CANNOT catch the bug they catch — a fake supplies the same
// constant the production code reads, so if that variable NAME is wrong the fake
// agrees with the code and both are wrong together. The seam test's own failure
// text reads "the variable name is wrong again", a record that this broke once.
//
// 🔴 WHY NOT t.Skip — MEASURED, not reasoned. They used to skip on
// `CLAUDECODE == ""`. Honest in isolation, wrong in a suite: `go test` EXITS 0
// WITH SKIPPED TESTS, so on CI (no session) both skipped silently and the run
// still looked green. muster's gate (tests/verdict.py) refuses ANY skip in a run
// told to require a database and caught exactly this on PR #3 — CI reported
// "1201 verdicts / 2 SKIPPED" where the dev box reported 1203 and none, because
// the dev box HAD a session. A build tag states the environment-dependence at
// COMPILE time instead of hiding it inside a green run.
//
// 🔴 THE GATE WAS NOT WEAKENED TO LAND THIS. verdict.py has no allowlist and
// still has none; adding one would make "nobody ran this and nobody was told"
// a supported configuration.
//
// RUN THEM: `make test-liveenv`, from inside a coding-agent session.

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// TestSeamWithTheREALProcessEnvironment is the control the whole feature turned
// out to need.
//
// 🔴 EVERY OTHER SEAM TEST SUPPLIES ITS OWN ENVIRONMENT, so none of them — not
// the header ledger, not the 0 -> 1 test above — can tell you the CLI reads the
// right variable NAME. That is not hypothetical: the upstream version of this
// feature read a variable that does not exist and shipped completely inert
// behind a green suite AND a mutation table that claimed to have killed exactly
// this mutant.
//
// So this one passes os.Getenv itself. The environment is whatever the calling
// session actually exported, nothing is keyed on a constant, and the assertion
// is that a task's thread moves 0 -> 1 with a session id this test never chose.
func TestSeamWithTheREALProcessEnvironment(t *testing.T) {
	if os.Getenv("CLAUDECODE") == "" {
		t.Skip("not running inside a coding-agent session; the live seam control needs a real session")
	}
	ts, ns := seamServer(t)
	ctx := context.Background()

	// What the running session's id actually is, read the same way production
	// reads it. Captured for the assertion — NOT injected anywhere.
	liveSessionID := sessionIDFrom(os.Getenv)
	if liveSessionID == "" {
		t.Fatalf("no session id in a real session environment; the variable name is wrong again")
	}

	task, err := ns.Create(ctx, notes.Note{Title: "live seam task", Body: "prove the REAL env var reaches the server"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = ns.Delete(context.Background(), task.ID) })
	id := strconv.FormatInt(task.ID, 10)

	before, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil || len(before) != 0 {
		t.Fatalf("thread starts at %d links (err=%v), want 0", len(before), err)
	}

	// The real command tree, with the REAL process environment.
	var out, errOut strings.Builder
	full := []string{
		"--api-url", ts.URL,
		"--token", seamHookToken,
		"--env-file", filepath.Join(t.TempDir(), "absent.env"),
		"task", "get", id,
	}
	if code := run(context.Background(), testProgName, full, strings.NewReader(""), &out, &errOut, os.Getenv, t.TempDir()); code != 0 {
		t.Fatalf("task get exit %d: %s", code, errOut.String())
	}

	after, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions after: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("🔴 with the REAL environment the thread went 0 -> %d, want 0 -> 1. This client is "+
			"not reading the environment variable the session actually exports — the feature is "+
			"inert.", len(after))
	}
	if after[0].SessionID != liveSessionID {
		t.Fatalf("the recorded session id %q is not this process's %q", after[0].SessionID, liveSessionID)
	}
	if after[0].Role != notes.RoleRead {
		t.Fatalf("`task get` recorded role %q, want %q", after[0].Role, notes.RoleRead)
	}
}

// TestSessionIDIsReadableFromThisProcessEnvironment is the LIVE control: when
// the test itself is run by a coding-agent session, the production reader must
// find a session id in the REAL process environment.
//
// 🔴 IT USES os.Getenv, NOT A FAKE. Every other test in this file supplies its
// own map, so all of them together still cannot tell you the name is right —
// that is precisely how the wrong name shipped. This one asks the actual
// environment.
//
// It SKIPS outside such a session, where there is genuinely nothing to assert.
// A skip is honest; asserting against an environment that cannot have the
// variable would be a test that fails for the wrong reason.
func TestSessionIDIsReadableFromThisProcessEnvironment(t *testing.T) {
	if os.Getenv("CLAUDECODE") == "" {
		t.Skip("not running inside a coding-agent session; the live env control needs a real session")
	}
	got := sessionIDFrom(os.Getenv)
	if got == "" {
		var present []string
		for _, n := range sessionIDEnvNames {
			if os.Getenv(n) != "" {
				present = append(present, n)
			}
		}
		t.Fatalf("sessionIDFrom found NO session id in a real session environment (names tried: %v, non-empty: %v).\n"+
			"This is the control that catches a renamed variable — the exact defect that shipped the task-thread feature inert.",
			sessionIDEnvNames, present)
	}
}
