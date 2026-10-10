package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postResponses(t *testing.T, url, auth, prompt string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "ignored", "stream": true,
		"input": []map[string]any{{"type": "message", "role": "user", "content": prompt}},
	})
	req, _ := http.NewRequest("POST", url+"/v1/responses", bytes.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func errType(t *testing.T, body string) string {
	t.Helper()
	var e struct {
		Error failure `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("failure body is not the typed JSON: %q", body)
	}
	return e.Error.Type
}

func TestResponsesRejectsAWrongOrMissingBearerWithoutTouchingTheSession(t *testing.T) {
	srv, cli := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	ts := gatewayServer(t, srv)
	for _, auth := range []string{"", strings.Repeat("f", 64), knownHooksToken} {
		code, body := postResponses(t, ts.URL, auth, "hi")
		if code != http.StatusUnauthorized || errType(t, body) != failUnauthorized {
			t.Fatalf("auth %q: %d %s", auth, code, body)
		}
	}
	if len(cli.pasted) != 0 {
		t.Fatalf("an unauthenticated request reached the terminal: %q", cli.pasted)
	}
}

func TestResponsesSuccessIsSSEEndingInResponseCompleted(t *testing.T) {
	srv, cli := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "list the workspace")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, "Second segment") {
		t.Fatalf("body: %s", body)
	}
	if strings.Contains(body, "PRIVATE REASONING") {
		t.Fatal("thinking reached the wire")
	}
	if len(cli.pasted) != 1 || cli.pasted[0] != "list the workspace" {
		t.Fatalf("pasted %q", cli.pasted)
	}
}

// 🔴 THE is_error GUARD. The transcript carries an isApiErrorMessage record but
// the CLI fired a plain Stop (no StopFailure) — a shape the hooks alone would read
// as success. The turn must still be a non-200. Mutation-tested: deleting the
// `res.Err != nil` branch in runTurn turns this red with an empty_reply (502)
// instead of the 429 asserted here, and the rate-limit case is the one where the
// difference is visible in the STATUS, not just the type.
func TestAnAPIErrorRecordFailsTheTurnEvenWhenTheHookSaysStop(t *testing.T) {
	cases := []struct {
		file   string
		status int
		typ    string
	}{
		{"turn_rate_limited.jsonl", http.StatusTooManyRequests, failRateLimited},
		{"turn_auth_failed_2.1.296.jsonl", http.StatusBadGateway, failAuth},
		{"turn_not_logged_in.jsonl", http.StatusBadGateway, failAuth},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			srv, _ := newScripted(t, cliScript{transcript: fixture(t, c.file), stopEvent: "Stop"})
			code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "hi")
			if code != c.status || errType(t, body) != c.typ {
				t.Fatalf("%d %s, want %d %s", code, body, c.status, c.typ)
			}
			if got := srv.auth.snapshot().Auth; got != c.typ {
				t.Fatalf("auth state %q after a %s turn", got, c.typ)
			}
		})
	}
}

// StopFailure with NO error record in the transcript (the hook is the only
// witness) is still a failure, classified from the hook's own `error`.
func TestStopFailureFailsTheTurnFromTheHookAlone(t *testing.T) {
	// A transcript with the prompt and nothing else.
	tr := []byte(`{"type":"user","promptId":"` + fixturePromptID + `","message":{"role":"user","content":"hi"}}` + "\n")
	srv, _ := newScripted(t, cliScript{transcript: tr, stopEvent: "StopFailure",
		stopError: "rate_limit", lastMsg: "You've hit your weekly limit · resets Oct 9, 9am (UTC)"})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "hi")
	if code != http.StatusTooManyRequests || errType(t, body) != failRateLimited || !strings.Contains(body, "weekly limit") {
		t.Fatalf("%d %s", code, body)
	}
}

// 🔴 THE EMPTY-REPLY GUARD: a turn that stopped cleanly but produced only thinking
// and tool calls is a 502, never a 200 with no text.
func TestATurnWithNoTextIsAnEmptyReplyFailure(t *testing.T) {
	var keep [][]byte
	for _, l := range bytes.SplitAfter(fixture(t, "turn_success_tools_2.1.289.jsonl"), []byte("\n")) {
		if !bytes.Contains(l, []byte(`"type":"text"`)) {
			keep = append(keep, l)
		}
	}
	srv, _ := newScripted(t, cliScript{transcript: bytes.Join(keep, nil), stopEvent: "Stop"})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "hi")
	if code != http.StatusBadGateway || errType(t, body) != failEmptyReply {
		t.Fatalf("%d %s", code, body)
	}
}

func TestNotReadyBeforeSessionStart(t *testing.T) {
	srv, cli := newScripted(t, cliScript{stopEvent: "Stop"})
	srv.mu.Lock()
	srv.sessionStarted = false
	srv.mu.Unlock()
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "hi")
	if code != http.StatusServiceUnavailable || errType(t, body) != failNotReady {
		t.Fatalf("%d %s", code, body)
	}
	if len(cli.pasted) != 0 {
		t.Fatal("a prompt was pasted into a session that has not reached its prompt")
	}
}

func TestBusyWhileATerminalTurnIsRunning(t *testing.T) {
	srv, cli := newScripted(t, cliScript{stopEvent: "Stop"})
	srv.onHook(hookEvent{Event: "UserPromptSubmit", PromptID: "typed-by-the-operator"})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "hi")
	if code != http.StatusConflict || errType(t, body) != failBusy {
		t.Fatalf("%d %s", code, body)
	}
	if len(cli.pasted) != 0 {
		t.Fatal("pasted into a busy session")
	}
	srv.onHook(hookEvent{Event: "Stop", PromptID: "typed-by-the-operator"})
	if srv.busy {
		t.Fatal("Stop did not clear busy")
	}
}

func TestAPasteThatNeverSubmitsTimesOutAfterOneRetryEnter(t *testing.T) {
	srv, cli := newScripted(t, cliScript{noSubmit: true})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "hi")
	if code != http.StatusGatewayTimeout || errType(t, body) != failNotSubmitted {
		t.Fatalf("%d %s", code, body)
	}
	if cli.enters != 1 {
		t.Fatalf("retry Enter pressed %d times, want 1", cli.enters)
	}
}

func TestATranscriptOutsideTheConfigDirIsRefused(t *testing.T) {
	srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"),
		stopEvent: "Stop", transcriptPath: "/etc/passwd"})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "hi")
	if code != http.StatusBadGateway || errType(t, body) != failTranscript {
		t.Fatalf("%d %s", code, body)
	}
}

func TestCRLFIsFoldedBeforeThePaste(t *testing.T) {
	srv, cli := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, "a\r\nb")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if cli.pasted[0] != "a\nb" {
		t.Fatalf("pasted %q", cli.pasted[0])
	}
}

// The unit half of the `!`/`/` guard (the real-tmux suite has the other).
func TestALeadingBangOrSlashIsDisarmedBeforeThePaste(t *testing.T) {
	cases := map[string]string{
		"!rm -rf /":            " !rm -rf /",
		"/compact":             " /compact",
		"/etc/hosts is broken": " /etc/hosts is broken",
		"plain":                "plain",
		" !already spaced":     " !already spaced",
		"a ! in the middle":    "a ! in the middle",
		"#, &, @, > and ?":     "#, &, @, > and ?",
	}
	for in, want := range cases {
		srv, cli := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
		if code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, in); code != http.StatusOK {
			t.Fatalf("%q: %d %s", in, code, body)
		}
		if cli.pasted[0] != want {
			t.Fatalf("%q pasted as %q, want %q", in, cli.pasted[0], want)
		}
	}
}

func TestBadRequests(t *testing.T) {
	srv, cli := newScripted(t, cliScript{})
	ts := gatewayServer(t, srv)
	for _, raw := range []string{`not json`, `{"input":[]}`, `{"input":[{"role":"assistant","content":"x"}]}`,
		`{"input":[{"role":"user","content":"   "}]}`, `{"input":42}`} {
		req, _ := http.NewRequest("POST", ts.URL+"/v1/responses", strings.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+pinnedDerivation)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || errType(t, string(b)) != failBadRequest {
			t.Fatalf("%s -> %d %s", raw, resp.StatusCode, b)
		}
	}
	if len(cli.pasted) != 0 {
		t.Fatal("a bad request reached the terminal")
	}
}

func TestPromptFromAcceptsTheShapesAClientSends(t *testing.T) {
	cases := map[string]string{
		`"bare string"`: "bare string",
		`[{"type":"message","role":"user","content":"musters shape"}]`:                                            "musters shape",
		`[{"role":"user","content":[{"type":"input_text","text":"a"},{"type":"text","text":"b"}]}]`:               "ab",
		`[{"role":"user","content":"first"},{"role":"assistant","content":"x"},{"role":"user","content":"last"}]`: "last",
	}
	for in, want := range cases {
		got, err := promptFrom(json.RawMessage(in))
		if err != nil || got != want {
			t.Errorf("%s -> %q, %v; want %q", in, got, err, want)
		}
	}
}

// The hook endpoint must not be reachable through the network-facing mux: a Stop
// forged from outside the pod would end a turn early.
func TestTheGatewayMuxDoesNotServeHooks(t *testing.T) {
	srv, _ := newScripted(t, cliScript{})
	srv.onHook(hookEvent{Event: "UserPromptSubmit", PromptID: "p"})
	ts := gatewayServer(t, srv)
	resp, err := http.Post(ts.URL+"/hook/Stop", "application/json", strings.NewReader(`{"hook_event_name":"Stop"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("gateway mux answered /hook/Stop with %d", resp.StatusCode)
	}
	if !srv.busy {
		t.Fatal("a hook POSTed to the gateway mux changed session state")
	}
	// Positive control: the hook mux DOES serve it.
	hs := httptest.NewServer(srv.hookHandler())
	defer hs.Close()
	resp, err = http.Post(hs.URL+"/hook/Stop", "application/json", strings.NewReader(`{"hook_event_name":"Stop"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || srv.busy {
		t.Fatalf("hook mux: %d busy=%v", resp.StatusCode, srv.busy)
	}
}

func TestHealthz(t *testing.T) {
	srv, cli := newScripted(t, cliScript{})
	ts := gatewayServer(t, srv)
	get := func() (int, health) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var h health
		if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, h
	}
	// Session started, terminal alive, credential UNPROVEN: not healthy.
	if code, h := get(); code != 503 || h.Auth != authUnknown || !h.SessionStarted {
		t.Fatalf("unproven credential: %d %+v", code, h)
	}
	srv.auth.set(authOK, "", "probe")
	if code, h := get(); code != 200 || !h.OK {
		t.Fatalf("all green: %d %+v", code, h)
	}
	cli.aliveErr = errors.New("no server running")
	if code, h := get(); code != 503 || h.Terminal == "ok" {
		t.Fatalf("dead tmux: %d %+v", code, h)
	}
	cli.aliveErr = nil
	srv.auth.set(authFailed, "401", "turn")
	if code, h := get(); code != 503 || h.Auth != authFailed {
		t.Fatalf("auth failed: %d %+v", code, h)
	}
	srv.auth.set(authOK, "", "probe")
	srv.mu.Lock()
	srv.sessionStarted = false
	srv.mu.Unlock()
	if code, _ := get(); code != 503 {
		t.Fatalf("no session: %d", code)
	}
}

func TestAStopFailureHookUpdatesTheAuthStateEvenWithNoTurnPending(t *testing.T) {
	srv, _ := newScripted(t, cliScript{})
	srv.auth.set(authOK, "", "probe")
	srv.onHook(hookEvent{Event: "StopFailure", Error: "authentication_failed", LastAssistantMessage: "401"})
	if got := srv.auth.snapshot().Auth; got != authFailed {
		t.Fatalf("auth = %q", got)
	}
}
