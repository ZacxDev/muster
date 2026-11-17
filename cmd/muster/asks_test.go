package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestAsksLogSendsItsFiltersAndPrintsTheRows: every filter flag reaches the
// query under the server's own parameter name; stdout is the server's JSON
// (indented), stderr one block per row — with an expired row's text said to
// have expired — and the next page's --before.
func TestAsksLogSendsItsFiltersAndPrintsTheRows(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/asks/log", http.StatusOK, `{"asks":[`+
		`{"id":9,"kind":"plain_text","host":"alpha","session":"s1","repo":"acme/widgets","question":"Tests pass.\nShall I continue?","answer":"yes","answered_by":"owner","outcome":"answered","asked_at":"2000-01-02T03:04:05Z","answer_secs":12,"text_expired":false},`+
		`{"id":8,"kind":"attention_ask","host":"","session":"","repo":"","question":"","answer":"","answered_by":"none","outcome":"expired","asked_at":"2000-01-01T00:00:00Z","answer_secs":null,"text_expired":true}],`+
		`"next":"8"}`)
	got := h.runCLI("asks", "log", "--kind", "plain_text", "--runtime", "claude-code", "--outcome", "answered", "--answered-by", "owner",
		"--host", "alpha", "--repo", "acme/widgets", "--session", "s1", "--since", "7d", "--until", "2000-01-03T00:00:00Z", "--limit", "2", "--before", "11")
	if got.code != exitOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	reqs := asksRequests(h)
	if len(reqs) != 1 {
		t.Fatalf("%d requests to the ask log", len(reqs))
	}
	u, _ := url.Parse(reqs[0].uri)
	want := url.Values{"kind": {"plain_text"}, "runtime": {"claude-code"}, "outcome": {"answered"}, "answered_by": {"owner"}, "host": {"alpha"},
		"repo": {"acme/widgets"}, "session": {"s1"}, "since": {"7d"}, "until": {"2000-01-03T00:00:00Z"}, "limit": {"2"}, "before": {"11"}}
	if u.Path != "/api/asks/log" || u.Query().Encode() != want.Encode() {
		t.Fatalf("sent %s; want /api/asks/log?%s", reqs[0].uri, want.Encode())
	}
	wantErr := "#9 2000-01-02T03:04:05Z plain_text [s1 alpha acme/widgets] answered\n  \"Tests pass.\" → \"yes\" by owner after 12s\n" +
		"#8 2000-01-01T00:00:00Z attention_ask [] expired\n  \"(text expired)\" → - by none\n" +
		"more: --before 8\n"
	if got.stderr != wantErr {
		t.Fatalf("stderr:\n%s\nwant:\n%s", got.stderr, wantErr)
	}
	if !strings.HasPrefix(got.stdout, "{\n  \"asks\": [") {
		t.Fatalf("stdout is not the indented JSON: %q", got.stdout)
	}
}

// TestAsksSummaryPrintsTheCountsAndTheRepeatedQuestions, and both verbs'
// refusals: a hook token's 401 is exit 3, the log off is exit 12 naming
// MUSTER_ASK_LOG.
func TestAsksSummaryPrintsTheCountsAndTheRepeatedQuestions(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/asks/summary", http.StatusOK, `{"total":7,"answered":5,"by_kind":{"plain_text":3,"attention_ask":2},`+
		`"by_outcome":{"answered":4,"open":1},"by_answered_by":{"owner":4,"none":2},"median_answer_secs":40,"p90_answer_secs":128,`+
		`"top_questions":[{"question_norm":"shall i continue","count":3,"answered_by_owner":2,"top_answer":"yes","top_answer_count":2}]}`)
	got := h.runCLI("asks", "summary", "--since", "30d", "--top", "5")
	if got.code != exitOK {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if r := asksRequests(h); len(r) != 1 {
		t.Fatalf("%d requests to the ask log", len(r))
	} else if u, _ := url.Parse(r[0].uri); u.RawQuery != "since=30d&top=5" {
		t.Fatalf("query %q", u.RawQuery)
	}
	want := "7 asks, 5 answered; median 40s, p90 128s to answer\n" +
		"by kind attention_ask=2 plain_text=3\nby outcome answered=4 open=1\nanswered by none=2 owner=4\n" +
		"  3× \"shall i continue\" (you answered 2; usually \"yes\" ×2)\n"
	if got.stderr != want {
		t.Fatalf("stderr:\n%s\nwant:\n%s", got.stderr, want)
	}

	for _, verb := range []string{"log", "summary"} {
		route := "GET /api/asks/" + verb
		h := newHarness(t)
		h.json(route, http.StatusUnauthorized, `{"error":"this route takes a personal API token (mst_…)"}`)
		if got := h.runCLI("asks", verb); got.code != exitAuth {
			t.Errorf("%s, a 401: exit %d (%s); want %d", verb, got.code, got.stderr, exitAuth)
		}
		h = newHarness(t)
		h.json(route, http.StatusNotFound, `{"error":"feature_disabled","feature":"ask-log","state":"off","enable":"set MUSTER_ASK_LOG=1 in .env and restart (docs/features.md#ask-log)"}`)
		if got := h.runCLI("asks", verb); got.code != exitFeatureDisabled || !strings.Contains(got.stderr, "MUSTER_ASK_LOG=1") || got.stdout != "" {
			t.Errorf("%s, the log off: exit %d stderr %q stdout %q; want %d naming MUSTER_ASK_LOG=1", verb, got.code, got.stderr, got.stdout, exitFeatureDisabled)
		}
	}
}

// asksRequests is the requests h saw to the ask log's routes (the client's
// /health skew probe aside).
func asksRequests(h *harness) []recordedRequest {
	var out []recordedRequest
	for _, r := range h.requests() {
		if strings.HasPrefix(r.uri, "/api/asks/") {
			out = append(out, r)
		}
	}
	return out
}
