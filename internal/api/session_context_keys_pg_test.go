package api

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// TestSessionContextAnswersExactlyItsKeys pins session_context's answer to
// EXACTLY these keys, a literal list (operator decision, PR #537 round 0
// Q1/D3): no dispatch target and no model to start a successor on — no
// suggested_target, no suggested_model, nor any other spelling of one. What a
// successor runs on arrives in phase 2, with the verbatim kickoff copy and the
// successor_of binding. A key added or dropped fails here, so a suggestion
// cannot come back under another name unreviewed.
//
// `model` is the session's OWN dispatch's model (null for a window the owner
// opened), a fact about the session as repo and branch are.
func TestSessionContextAnswersExactlyItsKeys(t *testing.T) {
	f := newViewChiefFixture(t)
	f.mirror.v2.mu.Lock()
	f.mirror.v2.batches = vcTranscript(vcOpenCodeSID)
	f.mirror.v2.mu.Unlock()
	raw := f.srv.toolSessionContext(asUser(1), `{"subject":"`+vcOpenCodeKey+`"}`)
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	if _, isErr := out["error"]; isErr {
		t.Fatalf("instrument check: the read was refused: %s", raw)
	}
	var keys []string
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := "base,branch,cwd,dispatch,github_repo,host,model,repo,runtime,status,subject,tail,title,tmux,truncated,untrusted,window"
	if got := strings.Join(keys, ","); got != want {
		t.Errorf("session_context answers the keys\n  %s\nwant exactly\n  %s\n(no suggested target or model before phase 2)", got, want)
	}
	for _, k := range keys {
		if strings.Contains(k, "suggest") || strings.Contains(k, "target") {
			t.Errorf("session_context answers %q: a dispatch suggestion, withheld until phase 2", k)
		}
	}
}
