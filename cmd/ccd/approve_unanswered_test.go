package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/podagent"
)

// mcpEvent is a prompt for a muster tool the Compose chief does NOT pre-allow.
const mcpEvent = `{"tool_name":"mcp__muster__task_comment","tool_input":{"task_id":7,"text":"x"},"permission_mode":"default","cwd":"/data/workspace","session_id":"s1"}`

// TestAnUnansweredPromptIsDeniedNotLeftHanging: in a container where nobody
// can answer a permission prompt (CCD_APPROVE_UNANSWERED=deny, the Compose
// chief), a prompt for an un-allowed tool — a muster tool, Bash, a question —
// is DENIED at once, and the relay is never contacted, even one that would
// approve. Control: without the flag the same prompt reaches the relay, and
// with no relay it defers (the terminal prompt) as before.
func TestAnUnansweredPromptIsDeniedNotLeftHanging(t *testing.T) {
	for name, ev := range map[string]string{
		"an un-allowed muster tool": mcpEvent,
		"bash":                      bashEvent,
		"a question":                `{"tool_name":"AskUserQuestion","tool_input":{"questions":[]},"permission_mode":"default"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRelay{t: t, states: []string{"decided:approve"}}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			t.Setenv(envConnectURL, srv.URL)
			t.Setenv(envAgentToken, "agent-own-token")
			t.Setenv(podagent.EnvApproveUnanswered, "deny")
			var out, errb bytes.Buffer
			if code := runApprove(nil, strings.NewReader(ev), &out, &errb); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if out.String() != approveDeny {
				t.Fatalf("stdout %q, want the deny decision %q", out.String(), approveDeny)
			}
			if !strings.Contains(errb.String(), "no operator can answer a permission prompt in this container") {
				t.Fatalf("the denial is not logged with its reason: %q", errb.String())
			}
			if calls := f.callList(); len(calls) != 0 {
				t.Fatalf("the relay was contacted (it would have approved): %v", calls)
			}
		})
	}
	t.Run("control: no flag reaches the relay", func(t *testing.T) {
		f := &fakeRelay{t: t, states: []string{"decided:approve"}}
		srv := httptest.NewServer(f.handler())
		defer srv.Close()
		t.Setenv(envConnectURL, srv.URL)
		t.Setenv(envAgentToken, "agent-own-token")
		t.Setenv(podagent.EnvApproveUnanswered, "")
		var out, errb bytes.Buffer
		runApprove(nil, strings.NewReader(mcpEvent), &out, &errb)
		if out.String() != approveAllow || len(f.callList()) == 0 {
			t.Fatalf("without the flag: stdout %q, relay calls %v; want the relay's approve", out.String(), f.callList())
		}
	})
	t.Run("control: no flag and no relay defers", func(t *testing.T) {
		t.Setenv(envConnectURL, "")
		t.Setenv(envAgentToken, "")
		t.Setenv(podagent.EnvApproveUnanswered, "")
		var out, errb bytes.Buffer
		runApprove(nil, strings.NewReader(mcpEvent), &out, &errb)
		if out.Len() != 0 {
			t.Fatalf("without the flag and a relay: stdout %q, want nothing (defer)", out.String())
		}
	})
}

// TestAnUnknownUnansweredValueNeverAllows: any value but "deny" is ignored
// (logged) and today's path runs — so a typo can never turn into an allow,
// and with no relay it defers.
func TestAnUnknownUnansweredValueNeverAllows(t *testing.T) {
	for _, v := range []string{"allow", "yes", "DENY", "1"} {
		t.Setenv(envConnectURL, "")
		t.Setenv(envAgentToken, "")
		t.Setenv(podagent.EnvApproveUnanswered, v)
		var out, errb bytes.Buffer
		runApprove(nil, strings.NewReader(mcpEvent), &out, &errb)
		if out.Len() != 0 {
			t.Errorf("%s=%q: stdout %q, want nothing", podagent.EnvApproveUnanswered, v, out.String())
		}
		if !strings.Contains(errb.String(), "ignoring it") {
			t.Errorf("%s=%q is not reported as ignored: %q", podagent.EnvApproveUnanswered, v, errb.String())
		}
	}
}
