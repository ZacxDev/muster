package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// testCreateTok is the agent-create token the harness hands the verb.
// It differs from testToken (the CLI's ordinary token, passed as --token) so a
// test can tell which one reached the route.
const testCreateTok = "mst_agent-create-token-never-printed"

// TestAgentCreatePresentsOnlyTheAgentCreateToken: `agent create` sends
// MUSTER_AGENT_CREATE_TOKEN as its bearer — never the ordinary token, which
// the harness also sets (--token) — and the form's fields as JSON, start
// included even when false.
func TestAgentCreatePresentsOnlyTheAgentCreateToken(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/agents", http.StatusCreated, `{"agent":{"id":13,"name":"spry-newt"},"started":false}`)
	got := h.runCLIEnv(map[string]string{envAgentCreateToken: testCreateTok}, strings.NewReader(""),
		"agent", "create", "--start=false", "--kind", "claude-code", "--repo", "acme/widgets",
		"--repo-branch", "dev", "--prompt", "fix the build", "--worker-pool", "reviewers", "--cc-account", "acct-b", "--cc-failover")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.requests()
	var post *recordedRequest
	for i := range reqs {
		if reqs[i].method == http.MethodPost && reqs[i].uri == "/api/agents" {
			post = &reqs[i]
		}
	}
	if post == nil {
		t.Fatalf("no POST /api/agents; saw %+v", reqs)
	}
	if post.auth != "Bearer "+testCreateTok {
		t.Fatalf("Authorization = %q; want the agent-create token, never the ordinary one", post.auth)
	}
	if strings.Contains(post.auth, testToken) {
		t.Fatalf("the ordinary token reached POST /api/agents")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(post.body), &body); err != nil {
		t.Fatalf("body %q: %v", post.body, err)
	}
	want := map[string]any{"kind": "claude-code", "repo": "acme/widgets", "repo_branch": "dev", "prompt": "fix the build",
		"worker_pool": "reviewers", "cc_account": "acct-b", "cc_failover": true, "start": false}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%q] = %#v; want %#v (body %s)", k, body[k], v, post.body)
		}
	}
	if _, ok := body["note_id"]; ok {
		t.Errorf("note_id sent without --note-id: %s", post.body)
	}
}

// TestAgentCreateWithOnlyTheOrdinaryTokenSendsNothing: with the CLI's ordinary
// full token set and MUSTER_AGENT_CREATE_TOKEN unset, the verb refuses with the
// scope sentence and sends NO request — so the full token never reaches the
// route that refuses it.
func TestAgentCreateWithOnlyTheOrdinaryTokenSendsNothing(t *testing.T) {
	h := newHarness(t)
	got := h.runCLIEnv(map[string]string{envToken: "mst_full-token"}, strings.NewReader(""), "agent", "create", "--start=true", "--prompt", "x")
	if got.code != exitUsage {
		t.Fatalf("exit = %d; want %d (stderr %q)", got.code, exitUsage, got.stderr)
	}
	if !strings.Contains(got.stderr, "needs a token of scope agent-create") || !strings.Contains(got.stderr, envAgentCreateToken) {
		t.Fatalf("stderr %q does not carry the scope refusal", got.stderr)
	}
	for _, r := range h.requests() {
		if r.uri == "/api/agents" {
			t.Fatalf("a request reached %s %s without an agent-create token", r.method, r.uri)
		}
	}
}

// TestAgentCreateRequiresStart: the two outcomes differ too much to default,
// so a call that does not say --start is a usage error and sends nothing.
func TestAgentCreateRequiresStart(t *testing.T) {
	h := newHarness(t)
	got := h.runCLIEnv(map[string]string{envAgentCreateToken: testCreateTok}, strings.NewReader(""), "agent", "create", "--prompt", "x")
	if got.code == exitOK {
		t.Fatalf("exit 0 without --start; stdout %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "start") {
		t.Fatalf("stderr %q does not name --start", got.stderr)
	}
	for _, r := range h.requests() {
		if r.uri == "/api/agents" {
			t.Fatalf("a request reached POST /api/agents without --start")
		}
	}
}

// TestAgentCreateSendsNoteIDAndStartTrue: --note-id travels as a number and
// --start (bare) as true.
func TestAgentCreateSendsNoteIDAndStartTrue(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/agents", http.StatusCreated, `{"agent":{"id":14,"name":"calm-owl"},"started":true}`)
	got := h.runCLIEnv(map[string]string{envAgentCreateToken: testCreateTok}, strings.NewReader(""), "agent", "create", "--start", "--note-id", "42")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	for _, r := range h.requests() {
		if r.uri == "/api/agents" {
			if !strings.Contains(r.body, `"note_id":42`) || !strings.Contains(r.body, `"start":true`) {
				t.Fatalf("body %s; want note_id 42 and start true", r.body)
			}
			return
		}
	}
	t.Fatal("no POST /api/agents")
}
