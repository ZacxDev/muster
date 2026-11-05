package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/features"
)

// "Hidden, but explained" on both MCP doors (features.go's mcpDisabledTool):
// a tool left out because its feature is off stays out of tools/list, and a
// call to it answers which feature and how to turn it on — while a name that is
// not a tool at all is still unknown.

// checkDisabledCall drives one door: name is unlisted, its call explains with
// wantFeature's message, and a made-up name is still refused as unknown.
func checkDisabledCall(t *testing.T, rpc func(body string) *mcpReply, set features.Set, name, wantFeature string) {
	t.Helper()
	tools := toolNamesOf(servedTools(t, rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	for _, n := range tools {
		if n == name {
			t.Fatalf("%s is listed although its feature is off: %v", name, tools)
		}
	}
	f, _ := set.Get(wantFeature)
	want := f.DisabledMessage()
	if !strings.HasPrefix(want, "disabled: "+wantFeature+" — ") {
		t.Fatalf("fixture: %q", want)
	}

	r := rpc(mcpToolCall(name, map[string]any{}))
	if r == nil || r.Error != nil {
		t.Fatalf("%s: a JSON-RPC error (%+v), want a tool error that explains", name, r)
	}
	text, isErr := toolTextRaw(t, r)
	var body map[string]string
	if !isErr || json.Unmarshal([]byte(text), &body) != nil || body["error"] != want {
		t.Fatalf("%s: isError=%v text %s, want error %q", name, isErr, text, want)
	}

	// muster_help{tool} explains it the same way.
	text, isErr = toolTextRaw(t, rpc(mcpToolCall("muster_help", map[string]any{"tool": name})))
	if !isErr || !strings.Contains(text, "disabled: "+wantFeature) || strings.Contains(text, "unknown tool") {
		t.Fatalf("muster_help{tool:%s}: %v %s", name, isErr, text)
	}

	// muster_help{} lists the feature among the disabled ones, with the tool.
	text, isErr = toolTextRaw(t, rpc(mcpToolCall("muster_help", map[string]any{})))
	var list struct {
		Disabled []mcpOffFeature `json:"disabled_features"`
	}
	if isErr || json.Unmarshal([]byte(text), &list) != nil {
		t.Fatalf("muster_help{}: %s", text)
	}
	found := false
	for _, d := range list.Disabled {
		if d.Feature == wantFeature {
			found = strings.Contains(strings.Join(d.Tools, ","), name) && d.Enable == want
		}
	}
	if !found {
		t.Fatalf("muster_help{} does not list %s (with %s) among %+v", wantFeature, name, list.Disabled)
	}

	// The control: a name no feature gates is still unknown.
	r = rpc(mcpToolCall("no_such_muster_tool", map[string]any{}))
	if r == nil || r.Error == nil || !strings.Contains(r.Error.Message, "unknown tool no_such_muster_tool") {
		t.Fatalf("an unknown name: %+v", r)
	}
}

// TestTheAgentDoorExplainsAToolWhoseFeatureIsOff: POST /mcp, plugins off — the
// chief's task_create_gated.
func TestTheAgentDoorExplainsAToolWhoseFeatureIsOff(t *testing.T) {
	f := newConnFixture(t)
	set := testFeatureSet(t, nil)
	f.srv.UseFeatures(set)
	tok := "features-agent-token-0123456789"
	f.seedAgent(t, "features-agent", tok)
	rpc := func(body string) *mcpReply { r, _ := f.mcp(t, tok, body, nil); return r }
	checkDisabledCall(t, rpc, set, gatedTaskToolDef().Name, features.Plugins)

	// With no registry installed, the same unlisted name is unknown, as before.
	f.srv.UseFeatures(features.Set{})
	r := rpc(mcpToolCall(gatedTaskToolDef().Name, map[string]any{}))
	if r == nil || r.Error == nil || !strings.Contains(r.Error.Message, "unknown tool") {
		t.Fatalf("no registry: %+v", r)
	}
}

// TestTheMirrorDoorExplainsAToolWhoseFeatureIsOff: POST /mirror/mcp, session
// routing off — route_reply.
func TestTheMirrorDoorExplainsAToolWhoseFeatureIsOff(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	set := testFeatureSet(t, nil)
	f.srv.UseFeatures(set)
	rpc := func(body string) *mcpReply {
		_, r, raw := f.mirrorRPC(t, mirrorAs(ws, focusSID), body)
		if r == nil {
			t.Fatalf("/mirror/mcp: %s", raw)
		}
		return r
	}
	checkDisabledCall(t, rpc, set, routeReplyToolDef().Name, features.SessionRouting)

	// A feature the registry calls ON whose tool is unlisted for another
	// reason stays unknown: the explanation is for an OFF feature only.
	f.srv.UseFeatures(testFeatureSet(t, map[string]features.Status{features.SessionRouting: {State: features.On}}))
	r := rpc(mcpToolCall(routeReplyToolDef().Name, map[string]any{}))
	if r == nil || r.Error == nil || !strings.Contains(r.Error.Message, "unknown tool") {
		t.Fatalf("feature on, tool unlisted: %+v", r)
	}
}

// disabledOn is the features one door's muster_help{} lists as disabled.
func disabledOn(t *testing.T, rpc func(string) *mcpReply) map[string]bool {
	t.Helper()
	text, isErr := toolTextRaw(t, rpc(mcpToolCall("muster_help", map[string]any{})))
	var list struct {
		Disabled []mcpOffFeature `json:"disabled_features"`
	}
	if isErr || json.Unmarshal([]byte(text), &list) != nil {
		t.Fatalf("muster_help{}: %s", text)
	}
	out := map[string]bool{}
	for _, d := range list.Disabled {
		out[d.Feature] = true
	}
	return out
}

// TestMusterHelpListsOnlyItsOwnDoorsFeatures: the mirror door's muster_help
// lists the features ITS tools need (session routing here) and never one only
// the agent door's tools need (focus, officers); the agent door lists focus
// (#441 round 1, R10).
func TestMusterHelpListsOnlyItsOwnDoorsFeatures(t *testing.T) {
	off := map[string]features.Status{features.Focus: {State: features.Off, Reason: "needs EXAMPLE_OPENROUTER_KEY"}}
	f, ws, _, _ := newMirrorMCPFixture(t)
	f.srv.UseFeatures(testFeatureSet(t, off))
	mirror := disabledOn(t, func(body string) *mcpReply {
		_, r, raw := f.mirrorRPC(t, mirrorAs(ws, focusSID), body)
		if r == nil {
			t.Fatalf("/mirror/mcp: %s", raw)
		}
		return r
	})
	if !mirror[features.SessionRouting] || mirror[features.Focus] {
		t.Fatalf("mirror door disabled_features = %v, want session-routing and not focus", mirror)
	}
	g := newGatedFixture(t)
	g.srv.UseFeatures(testFeatureSet(t, off))
	agent := disabledOn(t, func(body string) *mcpReply { r, _ := g.mcp(t, g.chiefTok, body, nil); return r })
	if !agent[features.Focus] {
		t.Fatalf("agent door disabled_features = %v, want focus", agent)
	}
}

// TestAToolIsOfferedExactlyWhenItsFeatureIsOn: the chief's focus tools, with
// the focus store wired, are listed when the registry says focus is on and
// not listed (a call explains) when it says off — the listing and the
// registry agree (#441 round 1, R10).
func TestAToolIsOfferedExactlyWhenItsFeatureIsOn(t *testing.T) {
	g := newGatedFixture(t)
	rpc := func(body string) *mcpReply { r, _ := g.mcp(t, g.chiefTok, body, nil); return r }
	focusTool := focusToolDefs()[0].Name
	listed := func() bool {
		for _, n := range toolNamesOf(servedTools(t, rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))) {
			if n == focusTool {
				return true
			}
		}
		return false
	}
	g.srv.UseFeatures(testFeatureSet(t, nil)) // focus on
	if !listed() {
		t.Fatalf("focus on: %s is not listed", focusTool)
	}
	g.srv.UseFeatures(testFeatureSet(t, map[string]features.Status{features.Focus: {State: features.Off, Reason: "needs EXAMPLE_OPENROUTER_KEY"}}))
	if listed() {
		t.Fatalf("focus off in the registry: %s is still listed", focusTool)
	}
	text, isErr := toolTextRaw(t, rpc(mcpToolCall(focusTool, map[string]any{})))
	if !isErr || !strings.Contains(text, "disabled: focus") {
		t.Fatalf("a call with focus off: %v %s", isErr, text)
	}
}

// TestTheAgentPageAndChiefPanelShowTheMutedMic: the agent page's chat and the
// chief panel draw the muted mic on a server with no dictation (#441 round 1,
// R7) — wired in agents.go and chief_panel_ui.go.
func TestTheAgentPageAndChiefPanelShowTheMutedMic(t *testing.T) {
	f := newConnFixture(t)
	id := f.seedAgent(t, "mic-agent", "mic-agent-token-0123456789")
	// The chat form (and its mic) is drawn for a running agent.
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET status = 'running' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	// The bare agent URL redirects to its active chat session; follow it.
	target := "/agents/mic-agent"
	if loc := f.call(t, "GET", target, nil, f.operator()).Header().Get("Location"); loc != "" {
		target = loc
	}
	page := f.call(t, "GET", target, nil, f.operator()).Body.String()
	if !strings.Contains(page, `id="chat-dictate-off"`) {
		t.Fatalf("the agent page draws no muted mic:\n%.3000s", page)
	}
	g := newGatedFixture(t)
	// The session acts as the chief's owner (the isolation walk's seam).
	g.srv.sessionUserOverride = func(*http.Request) int64 { return g.owner }
	panel := g.call(t, "GET", "/ui/chief/panel", nil, g.operator()).Body.String()
	if !strings.Contains(panel, `id="chat-dictate-off"`) {
		t.Fatalf("the chief panel draws no muted mic:\n%.600s", panel)
	}
}

// TestAMisconfiguredFeatureKeepsItsWiredTools: plugins asked for with no
// agent gateway are MISCONFIGURED (messages never reach the chief) — and the
// plugin store's tools still work, so they stay listed: the plugins e2e spec's
// claude-code chief drafts a reply over MCP on exactly that server. Only an
// OFF feature's tools are taken away (featureOff).
func TestAMisconfiguredFeatureKeepsItsWiredTools(t *testing.T) {
	g := newGatedFixture(t)
	rpc := func(body string) *mcpReply { r, _ := g.mcp(t, g.chiefTok, body, nil); return r }
	draft := replyDraftToolDef().Name
	listed := func() bool {
		for _, n := range toolNamesOf(servedTools(t, rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))) {
			if n == draft {
				return true
			}
		}
		return false
	}
	g.srv.UseFeatures(testFeatureSet(t, map[string]features.Status{features.Plugins: {State: features.Misconfigured, Reason: "no agent gateway"}}))
	if !listed() {
		t.Fatalf("plugins misconfigured, store wired: %s is not listed", draft)
	}
	g.srv.UseFeatures(testFeatureSet(t, map[string]features.Status{features.Plugins: {State: features.Off, Reason: "EXAMPLE_PLUGINS is unset"}}))
	if listed() {
		t.Fatalf("plugins off: %s is listed", draft)
	}
}
