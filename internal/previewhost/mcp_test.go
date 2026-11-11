package previewhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// ------------------------------------------------------------ MCP harness

// testPanes is the Caller the preview tests bind with: pane → window, as
// muster-mirror's CallerWindow resolves it from a listing.
var testPanes = map[string]string{"%1": "@1", "%2": "@2", "%3": "@1"}

func testCaller(r *http.Request) (string, error) {
	if w, ok := testPanes[r.Header.Get("X-Muster-Pane")]; ok {
		return w, nil
	}
	return "", errors.New("pane is in no window this host mirrors")
}

// rpcReply is one JSON-RPC answer, its tool result decoded.
type rpcReply struct {
	Status int
	Raw    string
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Result json.RawMessage `json:"result"`
}

// tool is a tools/call result: the text decoded, and isError.
type toolOut struct {
	IsError bool
	Body    map[string]any
}

func (r rpcReply) tool(t *testing.T) toolOut {
	t.Helper()
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if r.Error != nil {
		t.Fatalf("a JSON-RPC error, want a tool result: %s", r.Raw)
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("not a tool result: %s", r.Raw)
	}
	out := toolOut{IsError: res.IsError}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out.Body); err != nil {
		t.Fatalf("tool text is not a JSON object: %s", res.Content[0].Text)
	}
	return out
}

// code is a refusal's error code, failing the test on a success.
func (o toolOut) code(t *testing.T) string {
	t.Helper()
	if !o.IsError {
		t.Fatalf("a success %v, want a refusal", o.Body)
	}
	c, _ := o.Body["error"].(string)
	return c
}

func rpc(t *testing.T, s http.Handler, pane, body string) rpcReply {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/preview-mcp", strings.NewReader(body))
	if pane != "" {
		r.Header.Set("X-Muster-Pane", pane)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	out := rpcReply{Status: w.Code, Raw: w.Body.String()}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("not JSON: %q", w.Body.String())
		}
	}
	return out
}

// callTool is a tools/call of name with args from pane.
func callTool(t *testing.T, s http.Handler, pane, name string, args any) toolOut {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	return rpc(t, s, pane, string(b)).tool(t)
}

// countingProbe is probeHTTP, counted.
func countingProbe(n *atomic.Int32) func(context.Context, int) error {
	return func(ctx context.Context, port int) error {
		n.Add(1)
		return probeHTTP(ctx, port)
	}
}

// webServer is an HTTP server on 127.0.0.1 and its port.
func webServer(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // any HTTP answer opens it
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	p, _ := strconv.Atoi(u.Port())
	return p
}

// ------------------------------------------------------------- protocol

// TestPreviewMCPInitializeCarriesInstructions: initialize carries the server
// instructions that teach an agent previews exist (Claude Code shows them in
// every session's system prompt even when it defers the tools). The WHOLE
// text is pinned, whitespace-normalised, as a literal: a reword must change
// this test on purpose.
func TestPreviewMCPInitializeCarriesInstructions(t *testing.T) {
	s := &MCPServer{Host: NewHost(HostConfig{Logger: log.New(io.Discard, "", 0)}), Caller: testCaller}
	r := rpc(t, s, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	var init struct {
		Instructions *string `json:"instructions"`
	}
	if err := json.Unmarshal(r.Result, &init); err != nil || init.Instructions == nil {
		t.Fatalf("initialize carries no instructions: %s", r.Raw)
	}
	const want = "muster's preview tools (on the `preview` MCP server: preview_open, preview_list, preview_close) " +
		"show your owner a web page you serve on this machine. When you start a web or dev server your owner " +
		"should look at, bind it to loopback (127.0.0.1) on a port this host allows; you cannot see the " +
		"allowlist, so if preview_open answers port_not_allowed, restart the server on another port. Once it " +
		"answers HTTP, call preview_open{port, label} with a short label, and tell your owner it is under the " +
		"Previews control on this session's view in muster: no URL is returned, so do not make one up. Servers " +
		"on allowed ports are usually also offered to your owner automatically; preview_open is how you name " +
		"one and open it on purpose. Call preview_close{id} when it is no longer needed."
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if got := norm(*init.Instructions); got != norm(want) {
		t.Fatalf("instructions:\n got %q\nwant %q", got, norm(want))
	}
}

// TestPreviewMCPAnswersTheProtocolItself: initialize names muster-preview and
// negotiates as muster does, ping answers {}, a notification is 202 with no
// body, tools/list is EXACTLY the three tools, an unknown method is "method
// not found", an unknown tool is refused, and what is not JSON-RPC is -32600.
func TestPreviewMCPAnswersTheProtocolItself(t *testing.T) {
	s := &MCPServer{Host: NewHost(HostConfig{Logger: log.New(io.Discard, "", 0)}), Caller: testCaller, Version: "v9"}

	r := rpc(t, s, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct{ Name, Version string } `json:"serverInfo"`
	}
	if err := json.Unmarshal(r.Result, &init); err != nil || init.ProtocolVersion != "2025-03-26" || init.Capabilities.Tools == nil || init.ServerInfo.Name != "muster-preview" || init.ServerInfo.Version != "v9" {
		t.Fatalf("initialize: %s", r.Raw)
	}
	if r := rpc(t, s, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`); !strings.Contains(r.Raw, `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("an unknown version is answered with the newest: %s", r.Raw)
	}
	if r := rpc(t, s, "", `{"jsonrpc":"2.0","id":2,"method":"ping"}`); string(r.Result) != "{}" {
		t.Fatalf("ping: %s", r.Raw)
	}
	if r := rpc(t, s, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.Status != http.StatusAccepted || r.Raw != "" {
		t.Fatalf("notification: %d %q", r.Status, r.Raw)
	}
	r = rpc(t, s, "", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
		if tl.Description == "" || tl.InputSchema["type"] != "object" {
			t.Errorf("tool %s: no description or schema", tl.Name)
		}
	}
	if !slices.Equal(names, []string{"preview_open", "preview_list", "preview_close"}) {
		t.Fatalf("tools/list = %v; want exactly preview_open, preview_list, preview_close", names)
	}
	if r := rpc(t, s, "", `{"jsonrpc":"2.0","id":4,"method":"server/discover"}`); r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("unknown method: %s", r.Raw)
	}
	if r := rpc(t, s, "%1", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"attention_ask","arguments":{}}}`); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("a muster tool on /preview-mcp: %s", r.Raw)
	}
	if r := rpc(t, s, "", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`); r.Error == nil || r.Error.Code != -32600 {
		t.Fatalf("a batch: %s", r.Raw)
	}
}

// TestPreviewOpenRefusesBadArguments: a missing or out-of-range port, a long
// label or path, a path that is not a path, an unknown argument, and
// preview_list / preview_close arguments that do not validate are each a
// TOOL result with isError and invalid_arguments — which the model reads and
// can correct — not a JSON-RPC error; nothing is probed. (An unknown tool
// stays a JSON-RPC -32602: TestPreviewMCPAnswersTheProtocolItself.)
func TestPreviewOpenRefusesBadArguments(t *testing.T) {
	h := NewHost(HostConfig{Config: Config{Ports: mustPorts(t, "5173")}, Logger: log.New(io.Discard, "", 0)})
	var probes atomic.Int32
	s := &MCPServer{Host: h, Caller: testCaller, probe: countingProbe(&probes)}
	for _, args := range []string{
		`{}`, `{"port":0}`, `{"port":65536}`, `{"port":"5173"}`,
		`{"port":5173,"label":"` + strings.Repeat("x", 61) + `"}`,
		`{"port":5173,"path":"x"}`, `{"port":5173,"path":"//evil.example/"}`, `{"port":5173,"path":"/a b"}`,
		`{"port":5173,"path":"/` + strings.Repeat("x", 512) + `"}`,
		`{"port":5173,"url":"http://x"}`,
	} {
		r := rpc(t, s, "%1", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"preview_open","arguments":`+args+`}}`)
		if r.Error != nil {
			t.Errorf("%s: a JSON-RPC error %s; want an isError tool result", args, r.Raw)
			continue
		}
		if got := r.tool(t).code(t); got != CodeInvalidArguments {
			t.Errorf("%s: %q; want invalid_arguments", args, got)
		}
	}
	for _, c := range []struct{ tool, args string }{{"preview_list", `{"x":1}`}, {"preview_close", `{}`}, {"preview_close", `{"id":7}`}} {
		r := rpc(t, s, "%1", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+c.tool+`","arguments":`+c.args+`}}`)
		if r.Error != nil {
			t.Errorf("%s %s: a JSON-RPC error %s; want an isError tool result", c.tool, c.args, r.Raw)
			continue
		}
		if got := r.tool(t).code(t); got != CodeInvalidArguments {
			t.Errorf("%s %s: %q; want invalid_arguments", c.tool, c.args, got)
		}
	}
	if n := probes.Load(); n != 0 {
		t.Fatalf("%d probes for invalid arguments", n)
	}
}

func mustPorts(t *testing.T, ports ...string) PortSet {
	t.Helper()
	ps, err := ParsePorts(ports)
	must(t, err)
	return ps
}
