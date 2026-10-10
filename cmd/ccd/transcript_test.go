package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are REAL transcript records with their content replaced (paths,
// ids and timestamps normalised; record types, keys, block types and ordering
// exactly as written by the CLI):
//
//	turn_auth_failed_2.1.296.jsonl   recorded on the pinned CLI, deliberately invalid token
//	turn_success_tools_2.1.289.jsonl a real tool-using turn (thinking, text, tool_use,
//	                                 tool_result, thinking, text, stop_hook_summary,
//	                                 turn_duration), preceded by an EARLIER turn
//	turn_rate_limited.jsonl          the auth record re-shaped to the rate-limit record
//	                                 seen in real transcripts (error=rate_limit, 429)
//	turn_not_logged_in.jsonl         error=authentication_failed with NO apiErrorStatus
const fixturePromptID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func extract(t *testing.T, b []byte, promptID string) turnResult {
	t.Helper()
	res, err := extractTurn(bytes.NewReader(b), promptID)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestExtractTurnReturnsOnlyThisTurnsTextInOrder(t *testing.T) {
	res := extract(t, fixture(t, "turn_success_tools_2.1.289.jsonl"), fixturePromptID)
	want := []string{
		"First segment: I will list the workspace.",
		"Second segment: the workspace holds file-a and file-b.",
	}
	if !res.Found || !res.Complete {
		t.Fatalf("Found=%v Complete=%v, want both", res.Found, res.Complete)
	}
	if len(res.Segments) != len(want) {
		t.Fatalf("segments %q, want %q", res.Segments, want)
	}
	for i := range want {
		if res.Segments[i] != want[i] {
			t.Fatalf("segment %d = %q, want %q", i, res.Segments[i], want[i])
		}
	}
	if res.ToolUses != 1 {
		t.Fatalf("ToolUses = %d, want 1", res.ToolUses)
	}
	if res.Err != nil {
		t.Fatalf("Err = %+v on a successful turn", res.Err)
	}
	if got := res.Text(); got != want[0]+"\n\n"+want[1] {
		t.Fatalf("Text() = %q", got)
	}
}

func TestExtractTurnDropsThinkingAndToolUse(t *testing.T) {
	b := fixture(t, "turn_success_tools_2.1.289.jsonl")
	// Positive control: the fixture really does carry both, so their absence
	// below is the parser's doing.
	for _, s := range []string{"PRIVATE REASONING", `"type":"tool_use"`, `"name":"Bash"`} {
		if !bytes.Contains(b, []byte(s)) {
			t.Fatalf("fixture lost %q; this test would pass vacuously", s)
		}
	}
	text := extract(t, b, fixturePromptID).Text()
	for _, s := range []string{"PRIVATE REASONING", "Bash", "file-a\nfile-b"} {
		if strings.Contains(text, s) {
			t.Fatalf("reply leaks %q: %q", s, text)
		}
	}
}

func TestExtractTurnIgnoresTheEarlierTurn(t *testing.T) {
	b := fixture(t, "turn_success_tools_2.1.289.jsonl")
	if !bytes.Contains(b, []byte("AN EARLIER REPLY")) {
		t.Fatal("fixture lost its earlier turn; this test would pass vacuously")
	}
	if strings.Contains(extract(t, b, fixturePromptID).Text(), "EARLIER") {
		t.Fatal("the reply includes the previous turn's text")
	}
	// And the earlier turn is itself extractable by ITS prompt id.
	if got := extract(t, b, "99999999-8888-4777-8666-555555555555").Text(); got != "AN EARLIER REPLY that is not this turn" {
		t.Fatalf("earlier turn = %q", got)
	}
}

func TestExtractTurnUnknownPromptIsNotFound(t *testing.T) {
	res := extract(t, fixture(t, "turn_success_tools_2.1.289.jsonl"), "not-a-prompt-id")
	if res.Found || len(res.Segments) != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestExtractTurnAPIErrorRecords(t *testing.T) {
	cases := []struct {
		file, code string
		status     int
		msgHas     string
	}{
		{"turn_auth_failed_2.1.296.jsonl", "authentication_failed", 401, "401 OAuth access token is invalid"},
		{"turn_rate_limited.jsonl", "rate_limit", 429, "resets 9:45pm"},
		{"turn_not_logged_in.jsonl", "authentication_failed", 0, "Not logged in"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			res := extract(t, fixture(t, c.file), fixturePromptID)
			if res.Err == nil {
				t.Fatal("no error extracted from an isApiErrorMessage record")
			}
			if res.Err.Code != c.code || res.Err.Status != c.status || !strings.Contains(res.Err.Message, c.msgHas) {
				t.Fatalf("Err = %+v", *res.Err)
			}
			// The error's text is the error, never the reply.
			if len(res.Segments) != 0 {
				t.Fatalf("error text leaked into the reply: %q", res.Segments)
			}
			if !res.Complete {
				t.Fatal("turn_duration not seen")
			}
		})
	}
}

func TestExtractTurnIgnoresAnUnterminatedTrailingRecord(t *testing.T) {
	b := fixture(t, "turn_success_tools_2.1.289.jsonl")
	lines := bytes.SplitAfter(bytes.TrimRight(b, "\n"), []byte("\n"))
	last := lines[len(lines)-1] // turn_duration
	if !bytes.Contains(last, []byte("turn_duration")) {
		t.Fatal("fixture's last record is not turn_duration")
	}
	// The CLI is mid-write: the last record has no newline yet.
	partial := append(bytes.Join(lines[:len(lines)-1], nil), last[:len(last)/2]...)
	res := extract(t, partial, fixturePromptID)
	if res.Complete {
		t.Fatal("a half-written turn_duration was treated as written")
	}
	if len(res.Segments) != 2 {
		t.Fatalf("segments before the partial record: %q", res.Segments)
	}
}

func TestExtractTurnStopsAtTheNextPrompt(t *testing.T) {
	b := fixture(t, "turn_success_tools_2.1.289.jsonl")
	lines := bytes.SplitAfter(bytes.TrimRight(b, "\n"), []byte("\n"))
	// An interrupted turn: no turn_duration, and the next prompt begins.
	body := bytes.Join(lines[:len(lines)-1], nil)
	body = append(body, []byte(`{"type":"user","promptId":"next-prompt","message":{"role":"user","content":"next"}}`+"\n")...)
	body = append(body, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"NEXT TURN TEXT"}]}}`+"\n")...)
	res := extract(t, body, fixturePromptID)
	if strings.Contains(res.Text(), "NEXT TURN TEXT") {
		t.Fatal("the next turn's text was attributed to this one")
	}
	if res.Complete {
		t.Fatal("no turn_duration was written, yet Complete is set")
	}
}

func TestExtractTurnSkipsSidechainRecords(t *testing.T) {
	body := []byte(`{"type":"user","promptId":"p1","message":{"content":"go"}}
{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"SUBAGENT TEXT"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"main reply"}]}}
{"type":"system","subtype":"turn_duration"}
`)
	if got := extract(t, body, "p1").Text(); got != "main reply" {
		t.Fatalf("Text() = %q", got)
	}
}

// readTurnFile starts at the pre-paste size of the transcript; these pin what it
// does when that offset is exact, mid-record, or stale.
func TestReadTurnFileFromAnOffset(t *testing.T) {
	b := fixture(t, "turn_success_tools_2.1.289.jsonl")
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := bytes.Index(b, []byte(`"promptId":"`+fixturePromptID+`","type":"user"`))
	lineStart := int64(bytes.LastIndexByte(b[:prompt], '\n') + 1)
	want := "First segment: I will list the workspace.\n\nSecond segment: the workspace holds file-a and file-b."
	cases := map[string]int64{
		"exactly at the prompt's line":          lineStart,
		"mid-way through the previous record":   lineStart / 2,
		"past the prompt (file replaced since)": int64(len(b)) - 10,
		"past the end of the file":              int64(len(b)) * 4,
		"zero":                                  0,
	}
	for name, from := range cases {
		res, err := readTurnFile(path, fixturePromptID, from)
		if err != nil || res.Text() != want || !res.Complete {
			t.Errorf("%s (from=%d): %q complete=%v err=%v", name, from, res.Text(), res.Complete, err)
		}
	}
}
