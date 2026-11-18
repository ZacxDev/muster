package mirrorhost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// AN OPENCODE CALLER OF /preview-mcp (mcpproxy.go, "AN OPENCODE CALLER OF THE
// PREVIEW TOOLS"): opencode's one shared service cannot name a pane, so its
// requests carry X-Muster-Client: opencode and each tools/call names its
// session in params._meta["ai.opencode/sessionID"]; the call is bound to the
// ONE window showing that session (or its nearest ancestor), and fails closed
// otherwise.

// ocWindows: @1 is a Claude Code window (pane %7); @3 runs opencode on
// ses_launch, bound by its launcher record; @4 on ses_title, a verified title
// binding; @5 on ses_unverified, a title match muster does not send to; @6
// and @7 both show ses_twice.
type ocWindows struct{}

func (ocWindows) Windows(_ context.Context) ([]WindowInfo, error) {
	oc := func(pane, gen string, send bool, bind string) *AgentPane {
		return &AgentPane{Runtime: mirror.RuntimeOpenCode, Pane: pane, Gen: gen, Send: send, Bind: bind}
	}
	return []WindowInfo{
		{Session: "work", ID: "@1", Pane: "%7", Claude: &ClaudePane{Pane: "%7", Gen: proxySID}},
		{Session: "work", ID: "@3", Pane: "%11", Agent: oc("%11", "ses_launch", true, "")},
		{Session: "work", ID: "@4", Pane: "%12", Agent: oc("%12", "ses_title", false, mirror.AgentBindVerified)},
		{Session: "work", ID: "@5", Pane: "%13", Agent: oc("%13", "ses_unverified", false, mirror.AgentBindStream)},
		{Session: "work", ID: "@6", Pane: "%14", Agent: oc("%14", "ses_twice", true, "")},
		{Session: "work", ID: "@7", Pane: "%15", Agent: oc("%15", "ses_twice", true, "")},
	}, nil
}

// ocAncestors: ses_child is a subagent's session under ses_launch, ses_grand
// one under ses_child; ses_orphan's parent is no window's.
func ocAncestors(id string) []string {
	return map[string][]string{
		"ses_child":  {"ses_launch"},
		"ses_grand":  {"ses_child", "ses_launch"},
		"ses_orphan": {"ses_gone"},
	}[id]
}

func ocProxy(base string) *MCPProxy {
	p := previewProxy(base)
	p.Windows = ocWindows{}
	p.OpenCodeAncestors = ocAncestors
	return p
}

// ocHeaders is what opencode's MCP entry sends: the secret and the static
// client header — no pane, no tmux server (the service is in none).
func ocHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + proxySecret, MCPClientHeader: MCPClientOpenCode}
}

func ocRequest(hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, PreviewMCPPath+"?codemode=false", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

// TestAnOpenCodeCallIsBoundToItsSessionsWindow: the session the call names
// resolves to the window showing it — by a launcher binding or a verified
// title binding — and a subagent's child session (any depth) to its root's.
func TestAnOpenCodeCallIsBoundToItsSessionsWindow(t *testing.T) {
	p := ocProxy("http://127.0.0.1:1")
	for sid, want := range map[string]string{"ses_launch": "@3", "ses_title": "@4", "ses_child": "@3", "ses_grand": "@3", " ses_title ": "@4"} {
		if got, err := p.CallerWindow(ocRequest(ocHeaders()), sid); err != nil || got != want {
			t.Errorf("session %q: %q, %v; want %s", sid, got, err, want)
		}
	}
}

// TestAnOpenCodeCallNoWindowShowsFailsClosed: no session id, an id no window
// shows, one whose ancestors no window shows, a title match muster does not
// send to, and a session two windows show: each is an error — never the
// Claude Code window, never a guess.
//
// 🔴 REGRESSION TEST for the fail-closed branch: before this change an
// opencode call was resolved by its pane headers alone, which opencode's
// shared service can only fill with ITS pane.
func TestAnOpenCodeCallNoWindowShowsFailsClosed(t *testing.T) {
	p := ocProxy("http://127.0.0.1:1")
	for sid, why := range map[string]string{
		"":               "names no session",
		"   ":            "names no session",
		"ses_unknown":    "in no window",
		"ses_orphan":     "in no window",
		"ses_unverified": "not verified",
		"ses_twice":      "shown in 2 windows",
		proxySID:         "in no window", // a CLAUDE session's id is not an opencode one
	} {
		got, err := p.CallerWindow(ocRequest(ocHeaders()), sid)
		if err == nil || got != "" {
			t.Errorf("session %q resolved to %q", sid, got)
			continue
		}
		if !strings.Contains(err.Error(), why) {
			t.Errorf("session %q: %v; want it to say %q", sid, err, why)
		}
	}
	// No ancestry at all (a host with no opencode source): a child session is
	// not found, and its root still is.
	p.OpenCodeAncestors = nil
	if got, err := p.CallerWindow(ocRequest(ocHeaders()), "ses_child"); err == nil {
		t.Errorf("with no ancestry, ses_child resolved to %q", got)
	}
	if got, err := p.CallerWindow(ocRequest(ocHeaders()), "ses_launch"); err != nil || got != "@3" {
		t.Errorf("control, with no ancestry: %q, %v", got, err)
	}
}

// TestPaneHeadersDecideUnlessTheCallerIsOpenCode: a request WITHOUT the
// opencode marker is resolved by its pane exactly as before, whatever
// session its _meta names (Claude Code's headers win); a request WITH it is
// resolved by the session only — pane headers there can only be opencode's
// service's own, and are ignored.
func TestPaneHeadersDecideUnlessTheCallerIsOpenCode(t *testing.T) {
	p := ocProxy("http://127.0.0.1:1")
	cc := caller("%7")
	if got, err := p.CallerWindow(ocRequest(cc), "ses_launch"); err != nil || got != "@1" {
		t.Fatalf("Claude Code headers and an opencode id: %q, %v; want the pane's @1", got, err)
	}
	if got, err := p.CallerWindow(ocRequest(cc), ""); err != nil || got != "@1" {
		t.Fatalf("Claude Code headers alone: %q, %v; want @1", got, err)
	}
	both := caller("%7")
	both[MCPClientHeader] = MCPClientOpenCode
	if got, err := p.CallerWindow(ocRequest(both), "ses_launch"); err != nil || got != "@3" {
		t.Fatalf("an opencode caller with pane headers: %q, %v; want its session's @3", got, err)
	}
	if got, err := p.CallerWindow(ocRequest(both), ""); err == nil {
		t.Fatalf("an opencode caller with pane headers and no session resolved to %q (the pane's)", got)
	}
	// The marker is read case-insensitively; any other value is not it.
	odd := ocHeaders()
	odd[MCPClientHeader] = " OpenCode "
	if got, err := p.CallerWindow(ocRequest(odd), "ses_title"); err != nil || got != "@4" {
		t.Fatalf("marker %q: %q, %v", odd[MCPClientHeader], got, err)
	}
	other := caller("%7")
	other[MCPClientHeader] = "opencode-ish"
	if got, err := p.CallerWindow(ocRequest(other), "ses_launch"); err != nil || got != "@1" {
		t.Fatalf("marker %q: %q, %v; want the pane path", other[MCPClientHeader], got, err)
	}
}

// ocCall is a preview_list tools/call naming session sid in its _meta ("":
// no _meta).
func ocCall(sid string) string {
	params := map[string]any{"name": "preview_list", "arguments": map[string]any{}}
	if sid != "" {
		params["_meta"] = map[string]any{"ai.opencode/sessionID": sid, "progressToken": 1}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call", "params": params})
	return string(b)
}

// TestOpenCodeThroughTheEndpoint: opencode's service — the secret and the
// marker, no tmux server — gets initialize WITH the instructions and the
// three tools (no session needed), a call naming a shown session answers,
// and one naming none, or an unknown one, is the no_window tool error. The
// guard still applies: no secret is 401.
func TestOpenCodeThroughTheEndpoint(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{}`) })
	p := ocProxy(srv.URL)
	init := decodeReply(t, "initialize", postTo(t, p, PreviewMCPPath+"?codemode=false", ocHeaders(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
	if !strings.Contains(string(init.Result), `"instructions":"muster's preview tools`) {
		t.Fatalf("opencode's initialize carries no instructions: %s", init.Result)
	}
	if got := toolNames(t, postTo(t, p, PreviewMCPPath, ocHeaders(), listBody)); !slices.Equal(got, []string{"preview_open", "preview_list", "preview_close"}) {
		t.Fatalf("opencode lists %v", got)
	}
	if r := decodeReply(t, "call", postTo(t, p, PreviewMCPPath, ocHeaders(), ocCall("ses_child"))); !strings.Contains(string(r.Result), `{\"previews\":[]}`) {
		t.Fatalf("a shown session's preview_list: %s %v", r.Result, r.Error)
	}
	for _, sid := range []string{"", "ses_unknown", "ses_twice"} {
		r := decodeReply(t, "call", postTo(t, p, PreviewMCPPath, ocHeaders(), ocCall(sid)))
		if !strings.Contains(string(r.Result), `\"error\":\"no_window\"`) || !strings.Contains(string(r.Result), `"isError":true`) {
			t.Errorf("session %q: %s %v; want the no_window tool error", sid, r.Result, r.Error)
		}
	}
	noSecret := ocHeaders()
	delete(noSecret, "Authorization")
	if w := postTo(t, p, PreviewMCPPath, noSecret, listBody); w.Code != http.StatusUnauthorized {
		t.Fatalf("opencode with no secret: %d", w.Code)
	}
	if up.n() != 0 {
		t.Fatalf("opencode's preview requests reached muster %d time(s)", up.n())
	}
}

// jsonAnswer is an upstream answering body as muster does (application/json).
func jsonAnswer(body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// ocMusterCall is a muster tools/call (attention_ask) naming session sid in
// its _meta ("": no _meta).
func ocMusterCall(sid string) string {
	params := map[string]any{"name": "attention_ask", "arguments": map[string]any{"question": "q?"}}
	if sid != "" {
		params["_meta"] = map[string]any{"ai.opencode/sessionID": sid}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": params})
	return string(b)
}

// TestAnOpenCodeCallToMusterIsBoundToItsSession: on /mcp, opencode's service
// — the secret and the marker, no tmux server — is forwarded: initialize and
// tools/list marked as opencode's and with no session; a tools/call bound by
// the SAME rule as the preview tools (a launcher binding, a verified title
// binding, a child session's root) and forwarded with that window's session
// KEY and no Claude session. Pane headers beside the marker change nothing.
//
// 🔴 REGRESSION TEST: at the base an opencode-marked caller on /mcp was the
// outsider (no tools, nothing forwarded).
func TestAnOpenCodeCallToMusterIsBoundToItsSession(t *testing.T) {
	up, srv := newUpstream(t, jsonAnswer(musterTools))
	p := ocProxy(srv.URL)
	if got := toolNames(t, postTo(t, p, "/mcp", ocHeaders(), listBody)); !slices.Equal(got, []string{"attention_ask", "task_get"}) {
		t.Fatalf("opencode's tools/list on /mcp: %v; want muster's list", got)
	}
	if up.n() != 1 {
		t.Fatalf("tools/list reached muster %d time(s)", up.n())
	}
	first := up.calls[0]
	if first.Header.Get(MCPClientHeader) != MCPClientOpenCode || first.Header.Get(MCPSessionHeader) != "" || first.Header.Get(MCPAgentSessionHeader) != "" {
		t.Fatalf("tools/list forwarded with client %q, session %q, agent session %q", first.Header.Get(MCPClientHeader),
			first.Header.Get(MCPSessionHeader), first.Header.Get(MCPAgentSessionHeader))
	}
	withPanes := caller("%7")
	withPanes[MCPClientHeader] = MCPClientOpenCode
	for i, c := range []struct {
		hdr       map[string]string
		sid, want string
	}{
		{ocHeaders(), "ses_launch", "opencode:ses_launch"},
		{ocHeaders(), "ses_title", "opencode:ses_title"},
		{ocHeaders(), "ses_grand", "opencode:ses_launch"},
		{withPanes, "ses_child", "opencode:ses_launch"},
	} {
		if w := postTo(t, p, "/mcp", c.hdr, ocMusterCall(c.sid)); w.Code != http.StatusOK {
			t.Fatalf("session %q: %d %s", c.sid, w.Code, w.Body)
		}
		r := up.calls[up.n()-1]
		if up.n() != i+2 || r.Header.Get(MCPAgentSessionHeader) != c.want || r.Header.Get(MCPSessionHeader) != "" ||
			r.Header.Get(MCPClientHeader) != MCPClientOpenCode || r.Header.Get("Authorization") != "Bearer "+proxyToken {
			t.Fatalf("session %q forwarded as agent %q, claude %q, client %q (%d calls); want agent %q, no claude session",
				c.sid, r.Header.Get(MCPAgentSessionHeader), r.Header.Get(MCPSessionHeader), r.Header.Get(MCPClientHeader), up.n(), c.want)
		}
		if up.body[up.n()-1] != ocMusterCall(c.sid) {
			t.Fatalf("the body was changed: %s", up.body[up.n()-1])
		}
	}
}

// TestAnOpenCodeCallToMusterNoWindowShowsFailsClosed: on /mcp, a call naming
// no session, an id no window shows, a title match muster does not send to, a
// session two windows show, or a CLAUDE session's id is the binding's own
// refusal — a tool error, NOT forwarded — never the pane's Claude session.
func TestAnOpenCodeCallToMusterNoWindowShowsFailsClosed(t *testing.T) {
	up, srv := newUpstream(t, jsonAnswer(musterTools))
	p := ocProxy(srv.URL)
	withPanes := caller("%7")
	withPanes[MCPClientHeader] = MCPClientOpenCode
	for _, hdr := range []map[string]string{ocHeaders(), withPanes} {
		for sid, why := range map[string]string{
			"":               "names no session",
			"ses_unknown":    "is in no window this host mirrors",
			"ses_orphan":     "is in no window this host mirrors",
			"ses_unverified": "its binding is not verified",
			"ses_twice":      "is shown in 2 windows",
			proxySID:         "is in no window this host mirrors",
		} {
			if text := toolErrorText(t, postTo(t, p, "/mcp", hdr, ocMusterCall(sid))); !strings.Contains(text, why) {
				t.Errorf("session %q: %s; want it to say %q", sid, text, why)
			}
		}
	}
	if up.n() != 0 {
		t.Fatalf("an unbound opencode call reached muster %d time(s)", up.n())
	}
}

// TestTheOpenCodeMarkerOpensNothingElse: the marker is honoured on the two MCP
// servers' paths only — any other path is 404, marked or not, and nothing is
// forwarded. A request WITHOUT it is checked exactly as before: a caller
// outside this host's tmux server (no pane headers) is the outsider, and a
// Claude Code pane's call is forwarded with its Claude session and neither
// opencode header — whatever opencode-looking headers it sends itself.
func TestTheOpenCodeMarkerOpensNothingElse(t *testing.T) {
	up, srv := newUpstream(t, jsonAnswer(musterTools))
	p := ocProxy(srv.URL)
	for _, path := range []string{"/", "/mcp/", "/mirror/mcp", "/preview-mcp/x", "/MCP"} {
		if w := postTo(t, p, path, ocHeaders(), listBody); w.Code != http.StatusNotFound {
			t.Errorf("an opencode-marked request to %s: %d, want 404", path, w.Code)
		}
	}
	if up.n() != 0 {
		t.Fatalf("a marked request to another path reached muster %d time(s)", up.n())
	}
	bare := map[string]string{"Authorization": "Bearer " + proxySecret}
	odd := ocHeaders()
	odd[MCPClientHeader] = "opencode-ish"
	for name, hdr := range map[string]map[string]string{"no marker, no pane": bare, "another marker value": odd} {
		if got := toolNames(t, postTo(t, p, "/mcp", hdr, listBody)); len(got) != 0 {
			t.Errorf("%s: /mcp lists %v", name, got)
		}
		if r := decodeReply(t, "call", postTo(t, p, "/mcp", hdr, ocMusterCall("ses_launch"))); r.Error == nil {
			t.Errorf("%s: /mcp answered a call naming a shown session: %s", name, r.Result)
		}
		if got := toolNames(t, postTo(t, p, PreviewMCPPath, hdr, listBody)); len(got) != 0 {
			t.Errorf("%s: /preview-mcp lists %v", name, got)
		}
	}
	if up.n() != 0 {
		t.Fatalf("an unmarked outsider reached muster %d time(s)", up.n())
	}
	// A Claude Code pane naming an opencode session in _meta AND sending the
	// proxy's own upstream header itself: bound by its pane, as before.
	cc := caller("%7")
	cc[MCPAgentSessionHeader] = "opencode:ses_launch"
	if w := postTo(t, p, "/mcp", cc, ocMusterCall("ses_launch")); w.Code != http.StatusOK {
		t.Fatalf("a Claude Code call: %d %s", w.Code, w.Body)
	}
	r := up.calls[up.n()-1]
	if r.Header.Get(MCPSessionHeader) != proxySID || r.Header.Get(MCPAgentSessionHeader) != "" || r.Header.Get(MCPClientHeader) != "" {
		t.Fatalf("a Claude Code call forwarded as claude %q, agent %q, client %q", r.Header.Get(MCPSessionHeader),
			r.Header.Get(MCPAgentSessionHeader), r.Header.Get(MCPClientHeader))
	}
	// And one with no pane header in this tmux server is refused here, as before.
	noPane := map[string]string{"Authorization": "Bearer " + proxySecret, MCPTmuxHeader: proxyTMUX}
	if text := toolErrorText(t, postTo(t, p, "/mcp", noPane, ocMusterCall("ses_launch"))); !strings.Contains(text, "name the calling session: send "+MCPPaneHeader) {
		t.Fatalf("a call with no pane: %s", text)
	}
	if up.n() != 1 {
		t.Fatalf("%d forwarded, want 1", up.n())
	}
}

// TestAncestorsWalkTheServersSessionList: a child's parents, nearest first,
// from opencode's own session list; a root and an unknown id have none.
func TestAncestorsWalkTheServersSessionList(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_root": "Root"})
	srv.PutSession("ses_kid", "Kid", "ses_root")
	srv.PutSession("ses_grandkid", "Grandkid", "ses_kid")
	if got := src.Ancestors("ses_grandkid"); !slices.Equal(got, []string{"ses_kid", "ses_root"}) {
		t.Fatalf("ses_grandkid: %v", got)
	}
	if got := src.Ancestors("ses_kid"); !slices.Equal(got, []string{"ses_root"}) {
		t.Fatalf("ses_kid: %v", got)
	}
	for _, id := range []string{"ses_root", "ses_nobody", ""} {
		if got := src.Ancestors(id); len(got) != 0 {
			t.Errorf("%q: %v; want none", id, got)
		}
	}
	var none *OpenCodeSource
	if got := none.Ancestors("ses_kid"); got != nil {
		t.Fatalf("a nil source: %v", got)
	}
}
