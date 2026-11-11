package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// GET /v1/usage: the pod's Claude usage per session and UTC hour, counted
// from synthetic transcripts. Every expected number below is written out by
// hand from the fixture, never derived from the code; the token values are
// pairwise distinct within each bucket, so a mutant that swaps two fields
// cannot pass.

const usageBearer = "usage-bearer-for-the-test"

// usageNow is the rig's clock.
var usageNow = time.Date(2000, 1, 3, 12, 20, 0, 0, time.UTC)

// usage line builders: a user record, and an assistant record with usage.
func uLine(at, prompt string, sidechain bool) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "timestamp": at, "promptId": prompt, "isSidechain": sidechain,
		"message": map[string]any{"role": "user", "content": "synthetic prompt"}})
	return string(b) + "\n"
}

func aLine(at, id, model string, in, cacheRead, cacheCreate, out int64, extra map[string]any) string {
	m := map[string]any{"type": "assistant", "timestamp": at, "message": map[string]any{"id": id, "model": model,
		"content": []any{map[string]any{"type": "text", "text": "synthetic reply"}},
		"usage":   map[string]any{"input_tokens": in, "cache_read_input_tokens": cacheRead, "cache_creation_input_tokens": cacheCreate, "output_tokens": out}}}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b) + "\n"
}

func writeUsageFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// usageRig writes the fixture tree and returns a server over it.
//
//	projects/-data-workspace/sess-a.jsonl                 the main transcript
//	projects/-data-workspace/sess-a/subagents/agent-x.jsonl  a subagent's
//	projects/-data-other/sess-b.jsonl                     another project
//	projects/-data-workspace/sess-old.jsonl               mtime before since
func usageRig(t *testing.T) *server {
	t.Helper()
	config := t.TempDir()
	proj := filepath.Join(config, "projects", "-data-workspace")
	writeUsageFile(t, filepath.Join(proj, "sess-a.jsonl"), ""+
		// Before the window (09:xx): neither the turn nor the call counts.
		uLine("2000-01-03T09:50:00.000Z", "p0", false)+
		aLine("2000-01-03T09:55:00.000Z", "msg_0", "claude-test", 1, 2, 3, 4, nil)+
		// 10:05, before since (10:30) but inside its hour: counted.
		uLine("2000-01-03T10:05:00.000Z", "p1", false)+
		// One call, three block records repeating its usage; the largest
		// output (29) is a middle one, written mid-stream.
		aLine("2000-01-03T10:06:00.000Z", "msg_1", "claude-test", 11, 1301, 701, 5, nil)+
		aLine("2000-01-03T10:06:01.000Z", "msg_1", "claude-test", 11, 1301, 701, 29, nil)+
		aLine("2000-01-03T10:07:00.000Z", "msg_1", "claude-test", 11, 1301, 701, 23, nil)+
		// A tool result in the same turn: the same prompt, no new turn.
		uLine("2000-01-03T10:08:00.000Z", "p1", false)+
		// The SAME message id, a different input side: a second call.
		aLine("2000-01-03T11:02:00.000Z", "msg_1", "claude-test", 13, 1409, 37, 41, nil)+
		// An API error and a synthetic record: not calls.
		aLine("2000-01-03T11:03:00.000Z", "msg_err", "claude-test", 9001, 9002, 9003, 9004, map[string]any{"isApiErrorMessage": true})+
		aLine("2000-01-03T11:04:00.000Z", "msg_syn", "<synthetic>", 8001, 8002, 8003, 8004, nil)+
		"{not json\n"+
		uLine("2000-01-03T11:10:00.000Z", "p3", false)+
		// A record still being written: no newline, ignored.
		strings.TrimSuffix(aLine("2000-01-03T11:15:00.000Z", "msg_9", "claude-test", 7001, 7002, 7003, 7004, nil), "\n"))
	writeUsageFile(t, filepath.Join(proj, "sess-a", "subagents", "agent-x.jsonl"), ""+
		// A subagent's prompt is not a turn; its call IS this instance's spend,
		// counted on the parent session, once across its two block records.
		uLine("2000-01-03T11:20:00.000Z", "p2", true)+
		aLine("2000-01-03T11:21:00.000Z", "msg_s1", "claude-test", 3, 211, 97, 53, map[string]any{"isSidechain": true})+
		aLine("2000-01-03T11:21:02.000Z", "msg_s1", "claude-test", 3, 211, 97, 61, map[string]any{"isSidechain": true}))
	writeUsageFile(t, filepath.Join(config, "projects", "-data-other", "sess-b.jsonl"), ""+
		uLine("2000-01-03T12:01:00.000Z", "q1", false)+
		aLine("2000-01-03T12:02:00.000Z", "msg_b1", "claude-test", 2, 503, 101, 71, nil))
	old := filepath.Join(proj, "sess-old.jsonl")
	writeUsageFile(t, old, uLine("2000-01-03T11:30:00.000Z", "o1", false)+
		aLine("2000-01-03T11:31:00.000Z", "msg_o1", "claude-test", 6001, 6002, 6003, 6004, nil))
	// Last written before the window: not read at all (its records claim to be
	// inside it, which only a read would see).
	mt := time.Date(2000, 1, 3, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(old, mt, mt); err != nil {
		t.Fatal(err)
	}
	s := newServer(serverConfig{ConfigDir: config, Bearer: usageBearer}, nil, newAuthTracker())
	s.clock = func() time.Time { return usageNow }
	return s
}

func getUsage(t *testing.T, s *server, query string, auth bool) (int, cctranscript.UsagePage, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage"+query, nil)
	if auth {
		r.Header.Set("Authorization", "Bearer "+usageBearer)
	}
	w := httptest.NewRecorder()
	s.gatewayHandler().ServeHTTP(w, r)
	var p cctranscript.UsagePage
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("body %q: %v", w.Body, err)
		}
	}
	return w.Code, p, w.Body.String()
}

func unixOf(s string) string {
	t, _ := time.Parse(time.RFC3339, s)
	return itoa64(t.Unix())
}

// TestUsageRouteCountsCallsTokensAndTurnsPerSessionHour pins the whole answer
// for since = 10:30: the window starts at 10:00 (truncated), and every rule
// in the fixture's comments shows up as a literal number.
func TestUsageRouteCountsCallsTokensAndTurnsPerSessionHour(t *testing.T) {
	s := usageRig(t)
	code, p, raw := getUsage(t, s, "?since="+unixOf("2000-01-03T10:30:00Z"), true)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	want := `{"since":"2000-01-03T10:00:00Z","buckets":[` +
		`{"session":"sess-a","hour":"2000-01-03T10:00:00Z","turns":1,"api_calls":1,"input_tokens":11,"output_tokens":29,"cache_read_tokens":1301,"cache_creation_tokens":701},` +
		`{"session":"sess-a","hour":"2000-01-03T11:00:00Z","turns":1,"api_calls":2,"input_tokens":16,"output_tokens":102,"cache_read_tokens":1620,"cache_creation_tokens":134},` +
		`{"session":"sess-b","hour":"2000-01-03T12:00:00Z","turns":1,"api_calls":1,"input_tokens":2,"output_tokens":71,"cache_read_tokens":503,"cache_creation_tokens":101}]}` + "\n"
	if raw != want {
		t.Fatalf("answer\n got %s\nwant %s", raw, want)
	}
	if strings.Contains(raw, "synthetic") {
		t.Fatal("transcript text in the answer")
	}
	_ = p
}

// TestUsageRouteCountsOnlyTheWindowsHours: a since inside the 11:00 hour
// leaves 10:00 out entirely — and the call first seen at 10:06 is NOT
// re-counted at 11:00, nor is the turn p1 its tool result repeats.
func TestUsageRouteCountsOnlyTheWindowsHours(t *testing.T) {
	s := usageRig(t)
	_, _, raw := getUsage(t, s, "?since="+unixOf("2000-01-03T11:59:59Z"), true)
	want := `{"since":"2000-01-03T11:00:00Z","buckets":[` +
		`{"session":"sess-a","hour":"2000-01-03T11:00:00Z","turns":1,"api_calls":2,"input_tokens":16,"output_tokens":102,"cache_read_tokens":1620,"cache_creation_tokens":134},` +
		`{"session":"sess-b","hour":"2000-01-03T12:00:00Z","turns":1,"api_calls":1,"input_tokens":2,"output_tokens":71,"cache_read_tokens":503,"cache_creation_tokens":101}]}` + "\n"
	if raw != want {
		t.Fatalf("answer\n got %s\nwant %s", raw, want)
	}
	// A window after every record: [], never null.
	_, _, raw = getUsage(t, s, "?since="+unixOf("2000-01-03T13:00:00Z"), true)
	if raw != `{"since":"2000-01-03T13:00:00Z","buckets":[]}`+"\n" {
		t.Fatalf("empty window: %s", raw)
	}
}

// TestUsageRouteClampsSinceToTheLookback: a since older than 48 h is the
// first whole hour inside it — measured at a clock off the hour (12:20 →
// 13:00 two days back) and on it (12:00 → 12:00).
func TestUsageRouteClampsSinceToTheLookback(t *testing.T) {
	s := usageRig(t)
	if _, p, _ := getUsage(t, s, "?since=0", true); !p.Since.Equal(time.Date(2000, 1, 1, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("since 0 at 12:20: window starts %s, want 2000-01-01T13:00:00Z", p.Since)
	}
	s.clock = func() time.Time { return time.Date(2000, 1, 3, 12, 0, 0, 0, time.UTC) }
	if _, p, _ := getUsage(t, s, "?since=0", true); !p.Since.Equal(time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("since 0 at 12:00: window starts %s, want 2000-01-01T12:00:00Z", p.Since)
	}
}

func TestUsageRouteRefusesNoBearerAndABadSince(t *testing.T) {
	s := usageRig(t)
	if code, _, _ := getUsage(t, s, "?since=0", false); code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d, want 401", code)
	}
	for _, q := range []string{"", "?since=", "?since=abc", "?since=-5", "?since=1.5"} {
		if code, _, _ := getUsage(t, s, q, true); code != http.StatusBadRequest {
			t.Fatalf("%q: %d, want 400", q, code)
		}
	}
}
