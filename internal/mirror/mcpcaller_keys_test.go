package mirror

import (
	"slices"
	"testing"
)

// TestMCPCallerKeysAreRunsMCPCaller: MCPCallerKeys is exactly the keys
// RunsMCPCaller accepts for a window — every key it lists runs there, and
// every candidate key that runs there is listed (a Claude window's session,
// its split pane's, an opencode window's key only while its host sends to
// it) — so a window's pages (pages.SessionRef) are the pages its MCP door
// could have recorded, no more, no fewer.
func TestMCPCallerKeysAreRunsMCPCaller(t *testing.T) {
	const a, b, c = "0d1e2f3a-aaaa-4bbb-8ccc-0000000000aa", "0d1e2f3a-aaaa-4bbb-8ccc-0000000000bb", "ses_e2e01"
	oc := OpenCodeCallerKey(c)
	if oc == "" {
		t.Fatal("CONTROL: the opencode key is empty")
	}
	for _, tc := range []struct {
		name string
		w    Window
		want []string
	}{
		{"a Claude window", Window{Claude: true, ClaudeSession: a}, []string{a}},
		{"a Claude window with a split pane's session", Window{Claude: true, ClaudeSession: a, OtherClaude: []string{b}}, []string{a, b}},
		{"a shell with a Claude split pane", Window{OtherClaude: []string{b}}, []string{b}},
		{"an opencode window its host sends to", Window{Runtime: RuntimeOpenCode, AgentSession: c, AgentSend: true}, []string{oc}},
		{"an opencode window bound by a verified title", Window{Runtime: RuntimeOpenCode, AgentSession: c, AgentBind: AgentBindVerified}, []string{oc}},
		{"an opencode window its host does NOT send to", Window{Runtime: RuntimeOpenCode, AgentSession: c}, nil},
		{"a shell", Window{}, nil},
	} {
		got := tc.w.MCPCallerKeys()
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: MCPCallerKeys = %v, want %v", tc.name, got, tc.want)
		}
		for _, k := range got {
			if !tc.w.RunsMCPCaller(k) {
				t.Errorf("%s: lists %q, which it does not run", tc.name, k)
			}
		}
		for _, k := range []string{a, b, oc} {
			if tc.w.RunsMCPCaller(k) && !slices.Contains(got, k) {
				t.Errorf("%s: runs %q but does not list it", tc.name, k)
			}
		}
	}
}
