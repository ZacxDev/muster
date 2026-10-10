package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// The recorded payloads (pinned CLI, deliberately invalid token) decode into the
// fields ccd branches on. If the CLI renames one, this is where it shows.
func TestRecordedHookPayloadsCarryTheFieldsCcdReads(t *testing.T) {
	read := func(name string) hookEvent {
		var ev hookEvent
		if err := json.Unmarshal(fixture(t, name), &ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	ss := read("hook_SessionStart_2.1.296.json")
	if ss.Event != "SessionStart" || ss.SessionID == "" || ss.Source != "startup" {
		t.Fatalf("SessionStart: %+v", ss)
	}
	ups := read("hook_UserPromptSubmit_2.1.296.json")
	if ups.Event != "UserPromptSubmit" || ups.PromptID != fixturePromptID || ups.TranscriptPath == "" ||
		ups.Prompt != "para one\n\npara two with \"quotes\" and $HOME" {
		t.Fatalf("UserPromptSubmit: %+v", ups)
	}
	sf := read("hook_StopFailure_2.1.296.json")
	if sf.Event != "StopFailure" || sf.PromptID != fixturePromptID || sf.Error != "authentication_failed" ||
		!strings.Contains(sf.LastAssistantMessage, "401") {
		t.Fatalf("StopFailure: %+v", sf)
	}
	// The prompt id the hooks carry is the promptId the transcript records carry.
	if !extract(t, fixture(t, "turn_auth_failed_2.1.296.jsonl"), sf.PromptID).Found {
		t.Fatal("the hooks' prompt_id does not locate the turn in the transcript")
	}
}

func TestRunHookForwardsTheEventPayload(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(204)
	}))
	defer ts.Close()
	t.Setenv("CCD_HOOK_URL", ts.URL)
	payload := fixture(t, "hook_StopFailure_2.1.296.json")
	var stderr bytes.Buffer
	if rc := runHook([]string{"StopFailure"}, bytes.NewReader(payload), &stderr); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/hook/StopFailure" || !bytes.Equal(gotBody, payload) {
		t.Fatalf("path %q body %q", gotPath, gotBody)
	}
}

// 🔴 A DOWN ccd MUST COST THE SESSION NOTHING: exit 2 from a Stop hook would keep
// the turn running, from UserPromptSubmit it would refuse the prompt.
func TestRunHookExitsZeroWhenCcdIsUnreachable(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close() // nothing listens there now
	t.Setenv("CCD_HOOK_URL", url)
	var stderr bytes.Buffer
	if rc := runHook([]string{"Stop"}, strings.NewReader(`{}`), &stderr); rc != 0 {
		t.Fatalf("rc = %d with ccd down", rc)
	}
	if stderr.Len() == 0 {
		t.Fatal("an unreachable ccd was not reported on stderr")
	}
	if rc := runHook(nil, strings.NewReader(`{}`), &stderr); rc != 0 {
		t.Fatalf("rc = %d on a usage error", rc)
	}
}

func TestRunHookIsInertWhenDisabled(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer ts.Close()
	t.Setenv("CCD_HOOK_URL", ts.URL)
	t.Setenv("CCD_HOOK_DISABLED", "1")
	if rc := runHook([]string{"Stop"}, strings.NewReader(`{}`), io.Discard); rc != 0 || called {
		t.Fatalf("rc=%d called=%v", rc, called)
	}
	// Positive control: without the variable the same call does reach the server.
	os.Unsetenv("CCD_HOOK_DISABLED")
	if rc := runHook([]string{"Stop"}, strings.NewReader(`{}`), io.Discard); rc != 0 || !called {
		t.Fatalf("control: rc=%d called=%v", rc, called)
	}
}

func TestLoopbackOnly(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:18790", "[::1]:18790", "localhost:18790"} {
		if err := loopbackOnly(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{":18790", "0.0.0.0:18790", "192.0.2.1:18790", "example.com:18790", "nonsense"} {
		if err := loopbackOnly(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestHookLogCapturesRawPayloadsOnlyWhenSet(t *testing.T) {
	srv, _ := newScripted(t, cliScript{})
	hs := httptest.NewServer(srv.hookHandler())
	defer hs.Close()
	payload := fixture(t, "hook_StopFailure_2.1.296.json")
	post := func() {
		resp, err := http.Post(hs.URL+"/hook/StopFailure", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post() // HookLog unset: nothing written anywhere
	log := t.TempDir() + "/hooks.jsonl"
	srv.cfg.HookLog = log
	post()
	post()
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	line := append(bytes.TrimSpace(payload), '\n')
	if !bytes.Equal(got, append(append([]byte{}, line...), line...)) {
		t.Fatalf("hook log = %q", got)
	}
}
