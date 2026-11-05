package llmproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The provider's Anthropic-compatible endpoint REFUSES a `role: "system"`
// entry inside `messages` (Claude Code's mid-conversation system messages),
// for the pinned non-Anthropic models, with this error. It is the provider's
// own text, measured against its live endpoint with a request built the way
// the proxy builds one from a Claude Code body.
const midConversationSystemError = `{"type":"error","error":{"type":"invalid_request_error","message":"Mid-conversation reasoning ` +
	`effort (configuration_update) is not supported on example/pinned-model-20261017.","error_type":"invalid_request"},` +
	`"request_id":"gen-test-0001"}`

// claudeCodeBody is a request shaped like the one the pinned Claude Code CLI
// sends on a session's first turn: user text blocks, then a mid-conversation
// `system` message (the environment block, with cache_control), adaptive
// thinking, context_management, output_config and the rest.
const claudeCodeBody = `{"model":"claude-opus-5-5","messages":[` +
	`{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"say hi"}]},` +
	`{"role":"system","content":[{"type":"text","text":"# Environment\ncwd: /work","cache_control":{"type":"ephemeral"}}]}],` +
	`"system":[{"type":"text","text":"You are a Claude agent."},{"type":"text","text":"Be useful.","cache_control":{"type":"ephemeral"}}],` +
	`"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{}}}],` +
	`"metadata":{"user_id":"u"},"max_tokens":128000,"thinking":{"type":"adaptive","display":"omitted"},` +
	`"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"output_config":{"effort":"medium"},"stream":true}`

// TestAMidConversationSystemMessageIsFoldedIntoTheUserTurn: a `system` entry
// in `messages` never reaches the provider (which refuses every request that
// carries one); its content blocks join the adjacent user turn, byte for byte,
// and a tool_result stays first in the turn it is folded into. A body with no
// such message goes upstream unchanged (TestOnlyKnownMessagesFieldsGoUpstream).
func TestAMidConversationSystemMessageIsFoldedIntoTheUserTurn(t *testing.T) {
	cases := []struct {
		name, messages, want string
	}{
		{
			name:     "first turn: after the user's blocks",
			messages: "",
			want: `[{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"say hi"},` +
				`{"type":"text","text":"# Environment\ncwd: /work","cache_control":{"type":"ephemeral"}}]}]`,
		},
		{
			name: "between a tool_use and its tool_result: the result stays first",
			messages: `[{"role":"user","content":"go"},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},` +
				`{"role":"system","content":"note"},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]`,
			want: `[{"role":"user","content":"go"},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"note"}]}]`,
		},
		{
			name:     "first message, string content",
			messages: `[{"role":"system","content":"s"},{"role":"user","content":"hi"}]`,
			want:     `[{"role":"user","content":[{"type":"text","text":"s"},{"type":"text","text":"hi"}]}]`,
		},
		{
			name: "between two assistant turns: a user turn of its own",
			messages: `[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"system","content":"c"},` +
				`{"role":"assistant","content":"d"},{"role":"user","content":"e"}]`,
			want: `[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":[{"type":"text","text":"c"}]},` +
				`{"role":"assistant","content":"d"},{"role":"user","content":"e"}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "fold:system", providerLike)
			body := claudeCodeBody
			if tc.messages != "" {
				var m map[string]json.RawMessage
				if err := json.Unmarshal([]byte(body), &m); err != nil {
					t.Fatal(err)
				}
				m["messages"] = json.RawMessage(tc.messages)
				b, _ := json.Marshal(m)
				body = string(b)
			}
			resp, out := r.postBody(t, body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, out)
			}
			_, sentRaw := r.up.last()
			var sent map[string]json.RawMessage
			if err := json.Unmarshal(sentRaw, &sent); err != nil {
				t.Fatal(err)
			}
			if got := compact(t, sent["messages"]); got != compact(t, json.RawMessage(tc.want)) {
				t.Fatalf("upstream messages\n got %s\nwant %s", got, tc.want)
			}
			// The rest of the body is the allowlist's, untouched.
			if string(sent["model"]) != `"example/pinned-model"` || string(sent["thinking"]) != `{"type":"adaptive","display":"omitted"}` {
				t.Fatalf("model %s thinking %s", sent["model"], sent["thinking"])
			}
		})
	}
}

// providerLike is a stub that refuses what the provider was measured to
// refuse (a system message in `messages`, two user turns in a row), with the
// provider's own error, and streams otherwise.
func providerLike(w http.ResponseWriter, r *http.Request, n int) {
	var body struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	prev := ""
	for _, m := range body.Messages {
		if m.Role == "system" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, midConversationSystemError)
			return
		}
		if m.Role == "user" && prev == "user" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid Anthropic Messages API request"}}`)
			return
		}
		prev = m.Role
	}
	streamOK(0.001)(w, r, n)
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestAnUpstreamErrorIsRecordedWithItsTypeAndMessage: a non-2xx from the
// provider is relayed as before, and its error type and message are now in the
// audit record's detail and the log line — truncated, with the officer's key
// and anything key-shaped redacted — so a 400 can be diagnosed from the log.
func TestAnUpstreamErrorIsRecordedWithItsTypeAndMessage(t *testing.T) {
	const otherKey = "sk-or-v1-0000aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff"
	long := strings.Repeat("x", 400)
	cases := []struct {
		name, body, want string
	}{
		{
			name: "the provider's error",
			body: midConversationSystemError,
			want: `upstream_error="invalid_request_error: Mid-conversation reasoning effort (configuration_update) is not supported on ` +
				`example/pinned-model-20261017."`,
		},
		{
			name: "keys redacted",
			body: `{"error":{"type":"invalid_request_error","message":"bad key ` + testKey + ` and ` + otherKey + ` and Bearer abc.def"}}`,
			want: `upstream_error="invalid_request_error: bad key [redacted] and [redacted] and Bearer [redacted]"`,
		},
		{
			name: "truncated",
			body: `{"error":{"message":"` + long + `"}}`,
			want: `upstream_error="` + strings.Repeat("x", 300) + `…"`,
		},
		{
			name: "not JSON",
			body: "upstream\n  gateway   said no",
			want: `upstream_error="upstream gateway said no"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "err:body", func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body)
			})
			resp, out := r.post(t)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if tc.name == "the provider's error" && out != midConversationSystemError {
				t.Errorf("the provider's body was not relayed unchanged: %s", out)
			}
			recs := r.audits()
			if len(recs) != 1 {
				t.Fatalf("audit records %d", len(recs))
			}
			d := recs[0].Detail()
			if !strings.HasSuffix(d, " "+tc.want) {
				t.Errorf("audit detail does not end with the upstream error\n got %s\nwant suffix %s", d, tc.want)
			}
			if !strings.Contains(r.logs.String(), tc.want) {
				t.Errorf("log line lacks %s:\n%s", tc.want, r.logs.String())
			}
			for _, k := range []string{testKey, otherKey, "abc.def"} {
				if strings.Contains(d, k) || strings.Contains(r.logs.String(), k) {
					t.Errorf("%q reached the audit detail or the log", k)
				}
			}
		})
	}
}

// TestASuccessfulRequestRecordsNoUpstreamError: the field is absent on a 2xx,
// so an ok line is unchanged.
func TestASuccessfulRequestRecordsNoUpstreamError(t *testing.T) {
	r := newRig(t, "err:none", streamOK(0.001))
	if resp, _ := r.post(t); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if d := r.audits()[0].Detail(); strings.Contains(d, "upstream_error") {
		t.Fatalf("an ok request carries an upstream error: %s", d)
	}
}
