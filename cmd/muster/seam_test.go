package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/sse"
)

// 🔴 THE BEHAVIOURAL HALF OF THE SEAM, AND THE ONLY THING THAT PINS THE CLIENT
// AND THE SERVER TO EACH OTHER.
//
// The header ledger next door proves this CLI SENDS the provenance headers. The
// server's own tests prove muster RECORDS them. Neither proves the two AGREE: a
// header-name typo, a case difference, a value the server trims to nothing —
// each leaves both suites green and the feature dead. "Verified in isolation" is
// the vacuous green, and the defect lives in the seam nobody owns.
//
// So this test builds NOTHING itself. It runs the real command tree against the
// real api.Handler backed by real Postgres, and watches one task's thread move
// 0 -> 1.
//
// The 0 is not decoration. A test that only asserts "1 afterwards" cannot tell a
// working seam from a fixture that was already linked, and a zero measured by an
// instrument nobody has seen produce a non-zero is indistinguishable from an
// instrument wired to nothing — so the pair is asserted, and reported as a pair.
//
// DB-gated like every other Postgres-backed test here: without a database it
// SKIPS, and a skipped test is not a passing one. `make test` exports
// MUSTER_TEST_REQUIRE_DB=1, which turns that skip into a FAILURE — so the
// project's own gate cannot produce a green that never exercised this.

const seamHookToken = "seam-hook-token"

// seamLiveness is a SessionLivenessProbe that reports every session as having no
// transcript.
//
// ⚠ IT IS NOT A SHORTCUT. The transcript records belong to another service;
// api.UseExtensions treats a Notes store with NO liveness probe as a readiness
// DEFECT precisely because the alternative is every link rendering "no
// transcript recorded" over live transcripts. Supplying an honest always-false
// probe here keeps the wiring in its supported shape while asserting nothing
// about transcripts, which this test does not measure.
type seamLiveness struct{}

func (seamLiveness) SessionsExisting(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// seamServer stands up the real muster HTTP surface over a real database.
func seamServer(t *testing.T) (*httptest.Server, notes.Store) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ns := notes.NewPG(pool)
	srv := api.New(sse.New(8), api.AuthConfig{HookToken: seamHookToken}, log.New(io.Discard, "", 0))
	srv.UseExtensions(api.Extensions{Notes: ns, SessionLiveness: seamLiveness{}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, ns
}

// runSeamCLI drives the real command tree at the real server with a controlled
// environment.
func runSeamCLI(t *testing.T, ts *httptest.Server, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	full := append([]string{
		"--api-url", ts.URL,
		"--token", seamHookToken,
		"--env-file", filepath.Join(t.TempDir(), "absent.env"),
	}, args...)
	code := run(context.Background(), testProgName, full, strings.NewReader(""), &out, &errOut,
		func(k string) string { return env[k] }, t.TempDir())
	return code, out.String(), errOut.String()
}

func TestSeamClientToServerMovesTheThreadCount(t *testing.T) {
	ts, ns := seamServer(t)
	ctx := context.Background()
	const sessionID = "seam-session-7f3a"

	task, err := ns.Create(ctx, notes.Note{Title: "seam task", Body: "prove the header crosses the wire"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = ns.Delete(context.Background(), task.ID) })
	id := strconv.FormatInt(task.ID, 10)

	// --- NEGATIVE CONTROL: the same command with NO session in the environment
	// must move nothing. Without this, the 0 -> 1 below could be produced by any
	// request at all rather than by the header specifically.
	if code, _, stderr := runSeamCLI(t, ts, nil, "task", "get", id); code != 0 {
		t.Fatalf("task get (no session) exit %d: %s", code, stderr)
	}
	if links, err := ns.SessionsForTask(ctx, task.ID); err != nil || len(links) != 0 {
		t.Fatalf("a session-less CLI call created %d links (err=%v), want 0", len(links), err)
	}

	// --- THE PAIR. 0 before…
	before, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions before: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("thread starts at %d, want 0", len(before))
	}

	// …one real `task comment` with the session variable set…
	code, _, stderr := runSeamCLI(t, ts, map[string]string{liveSessionEnvName: sessionID},
		"task", "comment", id, "--body", "picked this up")
	if code != 0 {
		t.Fatalf("task comment exit %d: %s", code, stderr)
	}

	// …and 1 after, read back from the STORE (not from the response the CLI
	// printed), so the assertion cannot be satisfied by the client echoing itself.
	after, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions after: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("🔴 the thread went 0 -> %d, want 0 -> 1. The CLI sent a request the server did "+
			"not record as a session touch — the two halves of the seam disagree, and both suites "+
			"are green.", len(after))
	}
	if after[0].SessionID != sessionID {
		t.Fatalf("linked session = %q, want %q (the header value did not survive the wire)", after[0].SessionID, sessionID)
	}
	if after[0].Role != notes.RoleWorked {
		t.Fatalf("`task comment` recorded role %q, want %q — commenting is work", after[0].Role, notes.RoleWorked)
	}
	if after[0].Host == "" {
		t.Fatalf("the host header did not reach the stored link: %+v", after[0])
	}

	// A `task get` from the SAME session must not add a second row, and must not
	// downgrade the role — the whole monotonic contract, exercised over the wire.
	if code, _, stderr := runSeamCLI(t, ts, map[string]string{liveSessionEnvName: sessionID}, "task", "get", id); code != 0 {
		t.Fatalf("task get exit %d: %s", code, stderr)
	}
	again, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions after get: %v", err)
	}
	if len(again) != 1 || again[0].Role != notes.RoleWorked {
		t.Fatalf("a later `task get` produced %+v; want one link still at role worked", again)
	}

	// And the CLI's own output carries the thread, so a caller reading stdout
	// sees what the store holds.
	code, stdout, stderr := runSeamCLI(t, ts, nil, "task", "get", id)
	if code != 0 {
		t.Fatalf("task get exit %d: %s", code, stderr)
	}
	var got struct {
		Sessions []notes.SessionLink `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode CLI stdout: %v", err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != sessionID {
		t.Fatalf("the CLI's own JSON does not carry the thread: %+v", got.Sessions)
	}

	reverse, err := ns.TasksForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if len(reverse) != 1 || reverse[0].NoteID != task.ID {
		t.Fatalf("reverse lookup = %+v, want task %d", reverse, task.ID)
	}
}

// TestSeamTaskGetRecordsAReadNotAWrite is the role half of the seam. Without it,
// "a link exists" could be satisfied by a client that labelled every touch the
// same way, and the whole point of the role is that reading and working are
// different facts about a task.
func TestSeamTaskGetRecordsAReadNotAWrite(t *testing.T) {
	ts, ns := seamServer(t)
	ctx := context.Background()
	const sessionID = "seam-read-only-session"

	task, err := ns.Create(ctx, notes.Note{Title: "read-only seam task", Body: "b"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = ns.Delete(context.Background(), task.ID) })

	code, _, stderr := runSeamCLI(t, ts, map[string]string{liveSessionEnvName: sessionID},
		"task", "get", strconv.FormatInt(task.ID, 10))
	if code != 0 {
		t.Fatalf("task get exit %d: %s", code, stderr)
	}
	links, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("thread has %d links, want 1", len(links))
	}
	if links[0].Role != notes.RoleRead {
		t.Fatalf("`task get` recorded role %q, want %q", links[0].Role, notes.RoleRead)
	}
}

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
