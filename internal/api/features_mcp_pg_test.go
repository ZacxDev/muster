package api

import (
	"encoding/json"
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

// TestTheMirrorDoorRecordsTheHostForTheSessionToolsReadout: a call on a
// host's MCP door is what /api/doctor and GET /api/features report as
// mcp_seen — and a host that never called is not.
func TestTheMirrorDoorRecordsTheHostForTheSessionToolsReadout(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	if mcp, _ := f.srv.sessionTools.seen(1, "ws"); mcp {
		t.Fatal("seen before any call")
	}
	_, r, raw := f.mirrorRPC(t, mirrorAs(ws, focusSID), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if r == nil {
		t.Fatal(raw)
	}
	if mcp, hook := f.srv.sessionTools.seen(1, "ws"); !mcp || hook {
		t.Fatalf("after a call: mcp=%v hook=%v, want true false", mcp, hook)
	}
	if mcp, _ := f.srv.sessionTools.seen(1, "laptop"); mcp {
		t.Fatal("a host that never called is reported seen")
	}

	// What the doctor and GET /api/features serve from it.
	w := f.call(t, "GET", DoctorPath, nil, map[string]string{"Authorization": "Bearer " + connTestHookToken})
	var rep DoctorReport
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rep) != nil {
		t.Fatalf("doctor: %d %s", w.Code, w.Body)
	}
	seen := map[string]bool{}
	for _, h := range rep.Mirror.Hosts {
		seen[h.Host] = h.MCPSeen
	}
	if _, listed := seen["ws"]; !listed || !seen["ws"] {
		t.Fatalf("doctor: ws not reported mcp_seen: %+v", rep.Mirror.Hosts)
	}
	w = f.call(t, "GET", FeaturesPath, nil, f.operator())
	var fr featuresReport
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &fr) != nil {
		t.Fatalf("features: %d %s", w.Code, w.Body)
	}
	got := map[string]bool{}
	for _, h := range fr.SessionTools {
		got[h.Host] = h.MCPSeen
	}
	if !got["ws"] || fr.SessionToolsNote == "" {
		t.Fatalf("GET /api/features: session tools %+v", fr.SessionTools)
	}
}
