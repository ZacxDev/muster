package main

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

// 🔴 THE SEAM. muster's server reads a session-id header on every task-scoped
// route and joins the session to the task's thread. A server that reads a header
// no client sends is two components each hermetically green and broken TOGETHER:
// the server tests pass (they set the header themselves), the CLI tests pass
// (they never look at headers), and the feature records nothing in production.
//
// So there are two tests, and BOTH are needed:
//
//  1. TestTaskSubcommandHeaderLedger — a LEDGER of the exact header set every
//     task subcommand sends, failing when the set GROWS or SHRINKS. A structural
//     check alone would still type-check past a wrong value, so it asserts
//     values too.
//  2. TestSeamClientToServerMovesTheThreadCount (in the DB-gated file) — the
//     BEHAVIOURAL half: drive the real CLI at the real server and watch a task's
//     thread go 0 -> 1.

// liveSessionEnvName is the LITERAL environment variable a coding-agent session
// exports, written out here rather than referenced from sessionIDEnvNames.
//
// 🔴 THE LITERAL IS THE WHOLE POINT. The upstream version of every test in this
// file keyed its fake environment on the CONSTANT — `map[string]string{sessionIDEnv:
// sid}` against a `getenv` of `func(k){return env[k]}` — so the fake resolved
// whatever the constant said and the constant's VALUE was never under test. The
// constant named a variable that does not exist, and the whole feature shipped
// inert from the CLI with a fully green suite, a passing header ledger, and a
// mutation table that recorded exactly this mutant as KILLED.
//
// Do not replace this with sessionIDEnvNames[0]. The circularity is the bug.
const liveSessionEnvName = "CLAUDE_CODE_SESSION_ID"

// TestSessionIDEnvVarNamesAreLiteral pins the env var names against literal
// strings and against the BEHAVIOUR of a real map, with a negative control.
func TestSessionIDEnvVarNamesAreLiteral(t *testing.T) {
	// Structural: the exact list, in precedence order. Literals on the
	// expectation side — no reference to the production variable.
	want := []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID"}
	if strings.Join(sessionIDEnvNames, ",") != strings.Join(want, ",") {
		t.Fatalf("sessionIDEnvNames = %v, want exactly %v (primary first).\n"+
			"An earlier build read the second name as the primary and the task-thread feature shipped INERT with a green suite.",
			sessionIDEnvNames, want)
	}

	// Behavioural, over literal env maps — including the NEGATIVE CONTROL,
	// without which "a session id was found" could not distinguish "read the
	// right var" from "reads anything at all".
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"primary", map[string]string{"CLAUDE_CODE_SESSION_ID": "primary-id"}, "primary-id"},
		{"fallback", map[string]string{"CLAUDE_SESSION_ID": "fallback-id"}, "fallback-id"},
		{"primary wins over fallback", map[string]string{
			"CLAUDE_CODE_SESSION_ID": "primary-id", "CLAUDE_SESSION_ID": "fallback-id",
		}, "primary-id"},
		{"blank primary falls through", map[string]string{
			"CLAUDE_CODE_SESSION_ID": "   ", "CLAUDE_SESSION_ID": "fallback-id",
		}, "fallback-id"},
		{"negative control: a plausible WRONG name yields nothing", map[string]string{
			"CLAUDE_SESSION": "x", "CLAUDECODE_SESSION_ID": "x", "SESSION_ID": "x",
		}, ""},
		{"empty environment", map[string]string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionIDFrom(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Fatalf("sessionIDFrom(%v) = %q, want %q", tc.env, got, tc.want)
			}
		})
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

// headerNames returns the sorted header names a recorded request carried, minus
// the ones every HTTP client sets. The subtraction is explicit rather than an
// allowlist of what we care about: an allowlist can only ever notice headers
// someone thought of, and "a header nobody thought of" is the failure mode.
func headerNames(h http.Header) []string {
	transport := map[string]bool{
		"Accept": true, "Accept-Encoding": true, "Content-Length": true,
		"Content-Type": true, "User-Agent": true, "Authorization": true,
	}
	var out []string
	for k := range h {
		if !transport[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TestTaskSubcommandHeaderLedger pins the EXACT provenance-header set of every
// task subcommand.
//
// The ledger fails in both directions on purpose: dropping the session id from
// `task comment` breaks the thread silently, and quietly ADDING a header is how
// an unreviewed value starts reaching a server that stores it.
//
// ⚠ THE CASE LIST IS DERIVED FROM THE VERB LEDGER, not written out beside it, so
// a task subcommand added without a row here fails rather than going unobserved.
func TestTaskSubcommandHeaderLedger(t *testing.T) {
	const sid = "cli-session-abc123"
	// Every task subcommand sends the same three: the producer id, the session
	// id (so the task-thread link can be recorded) and the host (denormalised
	// onto the link so "which machine worked this" survives the transcript being
	// reaped).
	//
	// 🔴 LITERALS, NOT THE PRODUCTION CONSTANTS, AND THIS WAS MEASURED RATHER
	// THAN REASONED. Written as `[]string{headerHost, headerSessionID,
	// headerSource}` the ledger is a test of the code against itself: renaming a
	// constant renames the sent header AND the expectation together, so the
	// mutant is invisible. Driven as a mutation — the session-id constant changed
	// to a name Go's header canonicalisation cannot fold back — the constant
	// version SURVIVED with all six subtests green while the behavioural seam
	// test caught it. That is the same circularity this file's header describes
	// for the env-var names, one layer up.
	want := []string{"X-Muster-Host", "X-Muster-Session-Id", "X-Muster-Source"}
	sort.Strings(want)

	cases := []struct {
		name  string
		route string
		reply string
		args  []string
	}{
		{"task ls", "GET /api/tasks", `[]`, []string{"task", "ls"}},
		{"task get", "GET /api/tasks/12", `{"id":12}`, []string{"task", "get", "12"}},
		{"task create", "POST /api/tasks", `{"id":13}`, []string{"task", "create", "--body", "b"}},
		{"task status", "PATCH /api/tasks/12/status", `{"id":12}`, []string{"task", "status", "12", "in_progress"}},
		{"task comment", "POST /api/tasks/12/comments", `{"id":1,"author":"claude-code","body":"c"}`,
			[]string{"task", "comment", "12", "--body", "c"}},
	}

	// Derivation check: every `task` verb in the wiring ledger must have a case
	// here, and vice versa.
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.name] = true
	}
	for _, w := range wiring {
		if !strings.HasPrefix(w.name, "task ") {
			continue
		}
		if !covered[w.name] {
			t.Fatalf("🔴 the verb ledger has %q and this header ledger does not cover it. A task "+
				"subcommand with no header assertion is exactly how a provenance header goes "+
				"missing silently.", w.name)
		}
		delete(covered, w.name)
	}
	if len(covered) != 0 {
		t.Fatalf("this header ledger covers %v, which the verb ledger does not list", covered)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.json(tc.route, http.StatusOK, tc.reply)
			got := h.runCLIEnv(map[string]string{liveSessionEnvName: sid}, strings.NewReader(""), tc.args...)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			reqs := h.nonHealthRequests()
			if len(reqs) != 1 {
				t.Fatalf("requests = %+v", reqs)
			}
			names := headerNames(reqs[0].headers)
			if strings.Join(names, ",") != strings.Join(want, ",") {
				t.Fatalf("`%s` sent headers %v, want EXACTLY %v.\n"+
					"This ledger fails when the set grows OR shrinks: a dropped session-id header silently stops the task thread recording, and an added header is an unreviewed value reaching a server that stores it.",
					strings.Join(tc.args, " "), names, want)
			}
			// Structural agreement is not enough — a ledger of names type-checks
			// past a header carrying the WRONG value.
			if got := reqs[0].headers.Get(headerSessionID); got != sid {
				t.Fatalf("%s = %q, want %q", headerSessionID, got, sid)
			}
			if got := reqs[0].headers.Get(headerSource); got != defaultTaskSource {
				t.Fatalf("%s = %q, want %q", headerSource, got, defaultTaskSource)
			}
			if reqs[0].headers.Get(headerHost) == "" {
				t.Fatalf("%s is empty", headerHost)
			}
		})
	}
}

// TestSessionIDHeaderOmittedWhenUnset: an unset or blank session variable must
// omit the header, never send an empty one. An empty header is a CLAIM ("I am a
// session whose id is nothing"); its absence is the truth.
func TestSessionIDHeaderOmittedWhenUnset(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"unset", ""},
		{"blank", "   "},
		{"tabs", "\t\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.json("GET /api/tasks/12", http.StatusOK, `{"id":12}`)
			env := map[string]string{}
			if tc.value != "" {
				env[liveSessionEnvName] = tc.value
			}
			if got := h.runCLIEnv(env, strings.NewReader(""), "task", "get", "12"); got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			reqs := h.nonHealthRequests()
			if len(reqs) != 1 {
				t.Fatalf("requests = %+v", reqs)
			}
			if _, present := reqs[0].headers[http.CanonicalHeaderKey(headerSessionID)]; present {
				t.Fatalf("a %s session variable still sent %s = %q",
					tc.name, headerSessionID, reqs[0].headers.Get(headerSessionID))
			}
		})
	}
}

// TestTaskHeadersTrimsAndSurvivesAHostnameFailure exercises taskHeaders
// directly (the builder the wrappers share) for the two edge cases the
// command-level tests cannot reach: a padded session id, and a hostname lookup
// that fails.
func TestTaskHeadersTrimsAndSurvivesAHostnameFailure(t *testing.T) {
	getenv := func(k string) string {
		if k == liveSessionEnvName {
			return "  padded-session  "
		}
		return ""
	}
	ok := taskHeaders(getenv, func() (string, error) { return "a-host", nil }, defaultTaskSource)
	if ok[headerSessionID] != "padded-session" {
		t.Fatalf("session id = %q, want it trimmed", ok[headerSessionID])
	}

	// A hostname failure must cost the host header and NOTHING else — the thread
	// link is worth more than the machine name on it.
	degraded := taskHeaders(getenv, func() (string, error) { return "", errHostname }, defaultTaskSource)
	if _, present := degraded[headerHost]; present {
		t.Fatalf("a failed hostname lookup still set %s = %q", headerHost, degraded[headerHost])
	}
	if degraded[headerSessionID] != "padded-session" || degraded[headerSource] != defaultTaskSource {
		t.Fatalf("a failed hostname lookup cost the other headers: %+v", degraded)
	}

	// A hostname that succeeds but returns whitespace is the same case as a
	// failure: an empty header is a claim, not a value.
	blank := taskHeaders(getenv, func() (string, error) { return "   ", nil }, defaultTaskSource)
	if _, present := blank[headerHost]; present {
		t.Fatalf("a blank hostname was still sent: %q", blank[headerHost])
	}

	// An empty source is omitted rather than sent blank, same reasoning as the
	// session id.
	noSource := taskHeaders(getenv, func() (string, error) { return "h", nil }, "  ")
	if _, present := noSource[headerSource]; present {
		t.Fatalf("a blank source was still sent: %q", noSource[headerSource])
	}
}

// errHostname is a stand-in for os.Hostname's failure.
var errHostname = &hostnameError{}

type hostnameError struct{}

func (*hostnameError) Error() string { return "hostname unavailable" }

// TestTaskCommentStillHonoursAnExplicitSource: the ledger must not have
// flattened `--source` into the default. The comment author is derived from it
// server-side, so losing the flag would silently re-attribute reports.
func TestTaskCommentStillHonoursAnExplicitSource(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/12/comments", http.StatusOK, `{"id":1,"author":"repo-cos","body":"c"}`)
	got := h.runCLIEnv(map[string]string{liveSessionEnvName: "sid"}, strings.NewReader(""),
		"task", "comment", "12", "--body", "c", "--source", "repo-cos")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 || reqs[0].source != "repo-cos" {
		t.Fatalf("%s = %q, want repo-cos (the --source flag must still win)", headerSource, reqs[0].source)
	}
	if got.stderr != "" {
		t.Fatalf("unexpected stderr %q — the attribution warning should not fire for an accepted source", got.stderr)
	}
}
