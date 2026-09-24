package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

const roster = `[
  {"id":11,"name":"clever-fox","namespace":"agent-clever-fox","displayName":"Clever Fox","status":"running","kickedOff":true},
  {"id":12,"name":"bold-moth","namespace":"agent-bold-moth","displayName":"Bold Moth","status":"stopped","kickedOff":false},
  {"id":13,"name":"dup","displayName":"Twin","status":"running"},
  {"id":14,"name":"other","displayName":"Twin","status":"error"}
]`

// TestAgentResolveExactMatch is the motivating capability: a name becomes an id
// over the API, with no SELECT against Postgres.
func TestAgentResolveExactMatch(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK, roster)

	got := h.runCLI("agent", "resolve", "clever-fox")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	var a map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &a); err != nil {
		t.Fatalf("stdout not JSON: %v (%q)", err, got.stdout)
	}
	if a["id"].(float64) != 11 {
		t.Fatalf("resolved id = %v, want 11", a["id"])
	}
	if a["namespace"] != "agent-clever-fox" {
		t.Fatalf("namespace = %v", a["namespace"])
	}
	// The raw roster element is emitted, so fields this CLI does not model
	// survive.
	if a["kickedOff"] != true {
		t.Fatalf("kickedOff = %v, want the raw element passed through", a["kickedOff"])
	}
}

func TestAgentResolveIDOnly(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK, roster)

	got := h.runCLI("agent", "resolve", "bold-moth", "--id")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	if strings.TrimSpace(got.stdout) != "12" {
		t.Fatalf("stdout = %q, want 12", got.stdout)
	}
	// A bare integer is still a valid JSON document.
	var n int64
	if err := json.Unmarshal([]byte(got.stdout), &n); err != nil || n != 12 {
		t.Fatalf("--id output is not valid JSON: %v (%q)", err, got.stdout)
	}
}

// TestAgentResolveNoMatch: 0 matches is exit 4, with the roster listed on
// stderr so the caller can see what WAS available.
func TestAgentResolveNoMatch(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK, roster)

	got := h.runCLI("agent", "resolve", "no-such-agent")
	if got.code != exitNotFound {
		t.Fatalf("exit = %d, want %d", got.code, exitNotFound)
	}
	if got.stdout != "" {
		t.Fatalf("stdout = %q, want empty", got.stdout)
	}
	for _, want := range []string{"clever-fox", "bold-moth"} {
		if !strings.Contains(got.stderr, want) {
			t.Fatalf("stderr = %q, want the candidate list to mention %q", got.stderr, want)
		}
	}
}

// TestAgentResolveAmbiguous: >1 match is exit 2 (usage) and every candidate is
// named. Resolution is never allowed to silently pick one.
func TestAgentResolveAmbiguous(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK, roster)

	got := h.runCLI("agent", "resolve", "Twin")
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d", got.code, exitUsage)
	}
	if got.stdout != "" {
		t.Fatalf("stdout = %q, want empty on an ambiguous resolution", got.stdout)
	}
	for _, want := range []string{"dup", "other", "ambiguous"} {
		if !strings.Contains(got.stderr, want) {
			t.Fatalf("stderr = %q, want it to mention %q", got.stderr, want)
		}
	}
}

// TestAgentResolvePrefersExactSlug: an exact slug match wins outright, so a
// display name colliding with another agent's slug is not an ambiguity.
func TestAgentResolvePrefersExactSlug(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK,
		`[{"id":1,"name":"dup","displayName":"x"},{"id":2,"name":"other","displayName":"dup"}]`)

	got := h.runCLI("agent", "resolve", "dup", "--id")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	if strings.TrimSpace(got.stdout) != "1" {
		t.Fatalf("stdout = %q, want 1 (the exact slug match)", got.stdout)
	}
}

// TestAgentResolveEmptyName: the empty-name case is a local usage error, never
// a request — the same class as the empty path parameter.
func TestAgentResolveEmptyName(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK, roster)

	got := h.runCLI("agent", "resolve", "")
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d", got.code, exitUsage)
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

// TestAgentResolveSkipsAMalformedRosterElement: one bad row must not make every
// lookup fail.
func TestAgentResolveSkipsAMalformedRosterElement(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK,
		`["not-an-object",{"id":2,"name":"good-one"},{"id":"not-a-number","name":3}]`)

	got := h.runCLI("agent", "resolve", "good-one", "--id")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q — a malformed sibling row broke the whole lookup", got.code, got.stderr)
	}
	if strings.TrimSpace(got.stdout) != "2" {
		t.Fatalf("stdout = %q, want 2", got.stdout)
	}
}

func TestAgentLsPassesTokenAndPath(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents", http.StatusOK, roster)

	got := h.runCLI("agent", "ls")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].uri != "/api/agents" {
		t.Fatalf("requests = %+v", reqs)
	}
	if reqs[0].auth != "Bearer "+testToken {
		t.Fatalf("auth header = %q", reqs[0].auth)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &out); err != nil || len(out) != 4 {
		t.Fatalf("stdout = %q (err=%v)", got.stdout, err)
	}
}

// ---------------------------------------------------------------------------
// agent messages
// ---------------------------------------------------------------------------

func TestAgentMessagesPagingFlagsGoOnTheQueryString(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents/clever-fox/messages", http.StatusOK, `{"messages":[],"count":0}`)

	got := h.runCLI("agent", "messages", "clever-fox", "--limit", "7", "--session", "3", "--before", "99")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	uri := h.nonHealthRequests()[0].uri
	for _, want := range []string{"limit=7", "session=3", "before=99"} {
		if !strings.Contains(uri, want) {
			t.Fatalf("request URI %q missing %q", uri, want)
		}
	}
}

// TestAgentMessagesDefaultsSendTheDefaultLimitAndNothingElse: a zero session or
// before must be OMITTED, not sent as 0 — the server reads 0 as a real id.
func TestAgentMessagesDefaultsSendTheDefaultLimitAndNothingElse(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/agents/clever-fox/messages", http.StatusOK, `{"messages":[]}`)

	if got := h.runCLI("agent", "messages", "clever-fox"); got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	uri := h.nonHealthRequests()[0].uri
	if !strings.Contains(uri, "limit=50") {
		t.Fatalf("request URI %q does not carry the default limit", uri)
	}
	for _, unwanted := range []string{"session=", "before="} {
		if strings.Contains(uri, unwanted) {
			t.Fatalf("request URI %q sent %q for an unset flag", uri, unwanted)
		}
	}
}

// TestAgentMessagesRejectsNonsensePagingLocally asserts each guard fires with
// ITS OWN message, so a test cannot be satisfied by a different check firing.
func TestAgentMessagesRejectsNonsensePagingLocally(t *testing.T) {
	for _, tc := range []struct{ name, flag, value, want string }{
		{"zero limit", "--limit", "0", "--limit must be a positive integer"},
		{"negative limit", "--limit", "-1", "--limit must be a positive integer"},
		{"negative before", "--before", "-1", "--before must be a positive message id"},
		{"negative session", "--session", "-1", "--session must be a positive chat session id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.json("GET /api/agents/clever-fox/messages", http.StatusOK, `{"messages":[]}`)

			got := h.runCLI("agent", "messages", "clever-fox", tc.flag, tc.value)
			if got.code != exitUsage {
				t.Fatalf("exit = %d, want %d (stderr=%q)", got.code, exitUsage, got.stderr)
			}
			if !strings.Contains(got.stderr, tc.want) {
				t.Fatalf("stderr = %q, want this guard's own message %q", got.stderr, tc.want)
			}
			if n := len(h.requests()); n != 0 {
				t.Fatalf("server saw %d requests, want 0", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// agent task
// ---------------------------------------------------------------------------

// TestAgentTaskCommentIsAuthoredByTheTokenNotBySourceHeader is the attribution
// guard. It fails if the command is ever re-pointed at /api/tasks/{id}/comments,
// because that route needs a provenance header to attribute at all — and an
// agent sending one would be recorded as the generic coding-agent source,
// indistinguishable from a human-driven local pickup.
func TestAgentTaskCommentIsAuthoredByTheTokenNotBySourceHeader(t *testing.T) {
	h := newHarness(t)
	h.json("POST /agent/task/comment", http.StatusOK,
		`{"id":1,"noteId":42,"author":"spry-newt","body":"reproduced it"}`)

	res := h.runCLI("agent", "task", "comment", "--body", "reproduced it")
	if res.code != 0 {
		t.Fatalf("exit %d, want 0 (stderr: %s)", res.code, res.stderr)
	}

	reqs := h.nonHealthRequests()
	if len(reqs) != 1 {
		t.Fatalf("sent %d requests, want exactly 1: %+v", len(reqs), reqs)
	}
	got := reqs[0]
	if got.method != http.MethodPost || got.uri != "/agent/task/comment" {
		t.Errorf("sent %s %s, want POST /agent/task/comment — the /api/tasks route would lose agent attribution",
			got.method, got.uri)
	}
	if got.source != "" {
		t.Errorf("sent %s %q; the agent route derives the author from the token, so sending a source means this command is attributing the wrong way",
			headerSource, got.source)
	}
	if got.auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want the bearer token — it is what identifies the agent", got.auth)
	}
	if !strings.Contains(got.body, "reproduced it") {
		t.Errorf("body %q does not carry the comment text", got.body)
	}
}

// TestAgentTaskStatusRefusesComplete pins the half of the complete gate this
// binary owns. The server enforces it too (taskstatus.AllowedForAgent); this is
// the better error, and it must not cost a round trip.
func TestAgentTaskStatusRefusesComplete(t *testing.T) {
	h := newHarness(t)
	h.json("PATCH /agent/task/status", http.StatusOK, `{"id":42,"status":"complete"}`)

	res := h.runCLI("agent", "task", "status", "complete")
	if res.code != exitUsage {
		t.Fatalf("exit %d, want exitUsage (%d) — an agent must not be able to declare its own task complete",
			res.code, exitUsage)
	}
	if reqs := h.nonHealthRequests(); len(reqs) != 0 {
		t.Errorf("sent %d requests; a refused status must cost no round trip: %+v", len(reqs), reqs)
	}
	// 🔴 Pin the WHOLE normalised message, not a substring of it.
	//
	// A `strings.Contains(stderr, "ready_for_review")` assertion passes for the
	// WRONG REASON: with the complete-specific branch gutted, the fallback
	// "unknown status" message is emitted instead, and that message ENUMERATES
	// the allowed statuses — so it still contains "ready_for_review" while
	// telling the agent something false (`complete` is a valid status) and
	// dropping the close-out guidance entirely. A substring check cannot tell the
	// two messages apart; the whole string can.
	//
	// A cosmetic reword will now fail here. That is the trade, and it is worth it
	// for a message an agent acts on.
	wantErr := testProgName + `: refusing to send "complete": an agent may not declare its own task complete. ` +
		`The server enforces this too (taskstatus.AllowedForAgent); set ready_for_review and let a human or the operator close it.`
	if got := strings.TrimSpace(res.stderr); got != wantErr {
		t.Errorf("stderr mismatch — the complete-specific refusal is not what the agent saw.\n got: %q\nwant: %q", got, wantErr)
	}
	// Belt and braces: the fallback path must NOT be what fired. This is the
	// assertion that would have caught a gutted branch on its own.
	if strings.Contains(res.stderr, invalidStatusMsg) {
		t.Errorf("the generic unknown-status message fired instead of the complete-specific one; got %q", res.stderr)
	}
}

// TestAgentTaskCommandsSendTheTokenAndNoProvenanceHeaders is the identity
// ledger for the whole family, in BOTH directions.
//
//   - EVERY command must send the bearer token. It is the entire identity model
//     — the server resolves which agent is calling, and which task is theirs,
//     from it. Dropping it on `get` or `status` is otherwise invisible.
//   - NO command may send a provenance header. The sibling `task` family sends
//     three; these routes read none of them, and attribution here comes from the
//     token. Adding one silently ships an unreviewed value to a server that may
//     store it.
func TestAgentTaskCommandsSendTheTokenAndNoProvenanceHeaders(t *testing.T) {
	cases := []struct {
		name  string
		route string
		args  []string
	}{
		{"get", "GET /agent/task", []string{"agent", "task", "get"}},
		{"comment", "POST /agent/task/comment", []string{"agent", "task", "comment", "--body", "x"}},
		{"status", "PATCH /agent/task/status", []string{"agent", "task", "status", "in_progress"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.json(tc.route, http.StatusOK, `{"ok":true}`)
			// A session id IS in the environment, so "sent no session header" is a
			// real observation rather than a measurement of the absent case.
			res := h.runCLIEnv(map[string]string{liveSessionEnvName: "cli-session-abc123"},
				strings.NewReader(""), tc.args...)
			if res.code != exitOK {
				t.Fatalf("exit %d, stderr=%q", res.code, res.stderr)
			}
			reqs := h.nonHealthRequests()
			if len(reqs) != 1 {
				t.Fatalf("sent %d requests, want 1: %+v", len(reqs), reqs)
			}
			if reqs[0].auth != "Bearer "+testToken {
				t.Errorf("`%s` sent Authorization %q, want the bearer token — it is what identifies the agent AND selects its task",
					strings.Join(tc.args, " "), reqs[0].auth)
			}
			if names := headerNames(reqs[0].headers); len(names) != 0 {
				t.Errorf("`%s` sent provenance headers %v, want NONE.\nThis ledger fails when the set grows: these routes attribute from the token, so any extra header here is an unreviewed value reaching the server.",
					strings.Join(tc.args, " "), names)
			}
		})
	}
}

// TestAgentTaskStatusAcceptsTheWorkingStates is the positive control for the
// refusal above: without it, a command that refused EVERY status would pass the
// refusal test while being completely broken.
func TestAgentTaskStatusAcceptsTheWorkingStates(t *testing.T) {
	for _, s := range agentTaskStatuses() {
		t.Run(s, func(t *testing.T) {
			h := newHarness(t)
			h.json("PATCH /agent/task/status", http.StatusOK, `{"id":42,"status":"`+s+`"}`)

			res := h.runCLI("agent", "task", "status", s)
			if res.code != 0 {
				t.Fatalf("exit %d, want 0 (stderr: %s)", res.code, res.stderr)
			}
			reqs := h.nonHealthRequests()
			if len(reqs) != 1 {
				t.Fatalf("sent %d requests, want 1: %+v", len(reqs), reqs)
			}
			if reqs[0].method != http.MethodPatch || reqs[0].uri != "/agent/task/status" {
				t.Errorf("sent %s %s, want PATCH /agent/task/status", reqs[0].method, reqs[0].uri)
			}
			if !strings.Contains(reqs[0].body, s) {
				t.Errorf("body %q does not carry status %q", reqs[0].body, s)
			}
		})
	}
}

// TestAgentTaskStatusVocabularyIsDerivedNotRestated checks that the help text's
// status list comes from the server's own predicate. A hand-maintained copy
// would silently deny agents a status added to taskstatus.All() later.
func TestAgentTaskStatusVocabularyIsDerivedNotRestated(t *testing.T) {
	got := agentTaskStatuses()
	if len(got) == 0 {
		t.Fatal("agentTaskStatuses() is empty — the command would advertise no statuses at all")
	}
	var want []string
	for _, s := range taskstatusAll() {
		if s != "complete" {
			want = append(want, s)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("agentTaskStatuses() = %v, want %v (every valid status except complete)", got, want)
	}
	for _, s := range got {
		if s == "complete" {
			t.Fatalf("complete is offered to agents")
		}
	}
}

// TestAgentTaskRouteLedger asserts the exact SET of endpoints the `agent task`
// family reaches. It fails when the set GROWS or SHRINKS, which a per-command
// route assertion cannot do: the failure being guarded against is a command
// nobody remembered to write a test for, quietly pointed at /api/tasks/*.
func TestAgentTaskRouteLedger(t *testing.T) {
	ledger := map[string]string{
		"get":     "GET /agent/task",
		"comment": "POST /agent/task/comment",
		"status":  "PATCH /agent/task/status",
	}
	args := map[string][]string{
		"get":     {"agent", "task", "get"},
		"comment": {"agent", "task", "comment", "--body", "x"},
		"status":  {"agent", "task", "status", "in_progress"},
	}

	// The tree is the authority on which subcommands EXIST, so a new one that is
	// not in the ledger fails here rather than going unnoticed.
	root := newRootCmd(inspectApp())
	var names []string
	for _, c := range root.Commands() {
		if c.Name() != "agent" {
			continue
		}
		for _, sub := range c.Commands() {
			if sub.Name() != "task" {
				continue
			}
			for _, leaf := range sub.Commands() {
				names = append(names, leaf.Name())
			}
		}
	}
	if len(names) != len(ledger) {
		t.Fatalf("`agent task` has %d subcommands %v but the ledger lists %d — update the ledger and prove the new one hits /agent/*",
			len(names), names, len(ledger))
	}

	for _, name := range names {
		want, ok := ledger[name]
		if !ok {
			t.Fatalf("`agent task %s` is not in the route ledger", name)
		}
		h := newHarness(t)
		h.json(want, http.StatusOK, `{"ok":true}`)
		if res := h.runCLI(args[name]...); res.code != 0 {
			t.Fatalf("`agent task %s` exited %d (stderr: %s)", name, res.code, res.stderr)
		}
		reqs := h.nonHealthRequests()
		if len(reqs) != 1 {
			t.Fatalf("`agent task %s` sent %d requests, want 1: %+v", name, len(reqs), reqs)
		}
		if got := reqs[0].method + " " + reqs[0].uri; got != want {
			t.Errorf("`agent task %s` reached %q, want %q", name, got, want)
		}
	}
}

// TestAgentTaskHelpDoesNotRecommendTokenInArgv pins a NEGATIVE: the help must
// not tell an agent to pass its bearer token as a flag, because argv is
// world-readable through /proc while the environment and the token file are not.
//
// Asserted on the rendered help rather than on a source constant, because that
// is what an agent actually reads.
func TestAgentTaskHelpDoesNotRecommendTokenInArgv(t *testing.T) {
	root := newRootCmd(inspectApp())
	var help string
	for _, c := range root.Commands() {
		if c.Name() != "agent" {
			continue
		}
		for _, sub := range c.Commands() {
			if sub.Name() == "task" {
				help = sub.Long
			}
		}
	}
	if help == "" {
		t.Fatal("no `agent task` long help found — this guard has lost its subject")
	}
	// Positive control: prove we are reading the real help, so an empty or wrong
	// string cannot satisfy the negative assertion below vacuously.
	if !strings.Contains(help, "bound to the calling agent") {
		t.Fatalf("this does not look like the agent-task help; the guard would be vacuous:\n%s", help)
	}
	for _, bad := range []string{`--token "$`, "--token $", "pass --token"} {
		if strings.Contains(help, bad) {
			t.Errorf("the help recommends putting the token in argv (%q) — /proc makes that world-readable", bad)
		}
	}
}
