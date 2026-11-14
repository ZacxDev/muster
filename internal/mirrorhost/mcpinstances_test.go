package mirrorhost

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A FAKE MUSTER INSTANCE: answers initialize, tools/list (its session list to
// a bound caller, its remote list — marked, unless it plays an older muster —
// to a remote one) and tools/call (echoing the tool and arguments it ran), and
// records every request it was sent.
type fakeInstance struct {
	t       *testing.T
	mu      sync.Mutex
	session []map[string]any
	remote  []map[string]any
	mark    bool
	seen    []seenRequest
	srv     *httptest.Server
}

type seenRequest struct {
	Method, Path string
	Header       http.Header
	RPC          string
	Body         string
}

func newFakeInstance(t *testing.T, session, remote []map[string]any, mark bool) *fakeInstance {
	t.Helper()
	f := &fakeInstance{t: t, session: session, remote: remote, mark: mark}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeInstance) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	f.seen = append(f.seen, seenRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), RPC: req.Method, Body: string(b)})
	session, remote, mark := f.session, f.remote, f.mark
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "muster", "version": "t"}, "instructions": "muster's tools."}
	case "tools/list":
		if r.Header.Get(MCPClientHeader) == MCPClientRemote {
			res := map[string]any{"tools": remote}
			if mark {
				res["_meta"] = map[string]any{remoteListMetaKey: remoteListMetaValue}
			}
			result = res
		} else {
			result = map[string]any{"tools": session}
		}
	case "tools/call":
		text, _ := json.Marshal(map[string]any{"ran": req.Params.Name, "args": json.RawMessage(orNull(req.Params.Arguments))})
		result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}}
	default:
		result = map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func orNull(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}

func (f *fakeInstance) set(session, remote []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if session != nil {
		f.session = session
	}
	if remote != nil {
		f.remote = remote
	}
}

func (f *fakeInstance) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

// calls is the tools/call requests it was sent.
func (f *fakeInstance) calls() []seenRequest {
	var out []seenRequest
	for _, r := range f.requests() {
		if r.RPC == "tools/call" {
			out = append(out, r)
		}
	}
	return out
}

// tool is a served tool definition with the named string arguments; a name
// ending in "!" is required.
func tool(name string, args ...string) map[string]any {
	props := map[string]any{}
	var req []any
	for _, a := range args {
		if n, ok := strings.CutSuffix(a, "!"); ok {
			a = n
			req = append(req, a)
		}
		props[a] = map[string]any{"type": "string", "description": a + "."}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if req != nil {
		schema["required"] = req
	}
	return map[string]any{"name": name, "description": name + " does it.", "inputSchema": schema}
}

const laToken = "mmt_LLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLLL"

// twoInstances: the primary ("default") offers its sessions attention_ask
// (session-bound), dispatch_request with worker_pool, agents_list, and offers a
// remote caller dispatch_request and agents_list; "la" offers a remote caller
// dispatch_request WITHOUT worker_pool, agents_list and la_only.
func twoInstances(t *testing.T) (*MCPProxy, *fakeInstance, *fakeInstance) {
	t.Helper()
	dispatchPrimary := tool("dispatch_request", "prompt!", "worker_pool")
	primary := newFakeInstance(t,
		[]map[string]any{tool("attention_ask", "question!"), dispatchPrimary, tool("agents_list")},
		[]map[string]any{dispatchPrimary, tool("agents_list")}, true)
	la := newFakeInstance(t, nil,
		[]map[string]any{tool("dispatch_request", "prompt!"), tool("agents_list"), tool("la_only", "x")}, true)
	p := proxyFor(primary.srv.URL)
	p.Primary = "default"
	p.Extras = []MCPInstance{{Name: "la", URL: la.srv.URL, Token: laToken}}
	return p, primary, la
}

func callBody(name string, args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	return string(b)
}

// callResult is a tools/call answer's texts and error flag.
func callResult(t *testing.T, w *httptest.ResponseRecorder) ([]string, bool) {
	t.Helper()
	var r struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("not a tools/call answer: %d %s", w.Code, w.Body)
	}
	var texts []string
	for _, c := range r.Result.Content {
		texts = append(texts, c.Text)
	}
	return texts, r.Result.IsError
}

// TestACallRunsOnTheInstanceItNames: with no instance a call runs on the
// primary, bound to the caller's session as ever; with instance "la" it runs
// on la ONLY — la's own token, the remote marker and the calling session, the
// instance argument removed — and each answer names the instance that gave it.
func TestACallRunsOnTheInstanceItNames(t *testing.T) {
	p, primary, la := twoInstances(t)

	texts, isErr := callResult(t, postBody(t, p, caller("%7"), callBody("agents_list", nil)))
	if isErr || len(texts) != 2 || texts[0] != "[muster instance: default]" || !strings.Contains(texts[1], `"ran":"agents_list"`) {
		t.Fatalf("default: %q", texts)
	}
	if c := primary.calls(); len(c) != 1 || c[0].Header.Get(MCPSessionHeader) != proxySID || c[0].Header.Get("Authorization") != "Bearer "+proxyToken {
		t.Fatalf("the primary's calls: %+v", c)
	}
	if n := len(la.calls()); n != 0 {
		t.Fatalf("la was called %d time(s) for a call naming no instance", n)
	}

	texts, isErr = callResult(t, postBody(t, p, caller("%7"), callBody("agents_list", map[string]any{"instance": "la"})))
	if isErr || len(texts) != 2 || texts[0] != "[muster instance: la]" || !strings.Contains(texts[1], `"ran":"agents_list"`) {
		t.Fatalf("la: %q", texts)
	}
	c := la.calls()
	if len(c) != 1 {
		t.Fatalf("la's calls: %d", len(c))
	}
	h := c[0].Header
	if h.Get("Authorization") != "Bearer "+laToken || h.Get(MCPClientHeader) != MCPClientRemote || h.Get(MCPRemoteSessionHeader) != proxySID || h.Get(MCPSessionHeader) != "" {
		t.Fatalf("la's call headers: %v", h)
	}
	if strings.Contains(c[0].Body, `"instance"`) {
		t.Fatalf("the instance argument reached la: %s", c[0].Body)
	}
	if n := len(primary.calls()); n != 1 {
		t.Fatalf("the primary ran %d calls; a call naming la must not reach it", n)
	}
	// Naming the primary is the same as naming none.
	if texts, _ := callResult(t, postBody(t, p, caller("%7"), callBody("agents_list", map[string]any{"instance": "default"}))); texts[0] != "[muster instance: default]" {
		t.Fatalf("instance default: %q", texts)
	}
}

// TestAnUnknownInstanceIsRefusedWithNoFallback: a call naming an instance the
// mirror does not know is refused, naming the ones it does — and nothing is
// sent anywhere, the primary included. A non-string instance is refused too.
func TestAnUnknownInstanceIsRefusedWithNoFallback(t *testing.T) {
	p, primary, la := twoInstances(t)
	text := toolErrorText(t, postBody(t, p, caller("%7"), callBody("agents_list", map[string]any{"instance": "nyc"})))
	if !strings.Contains(text, `unknown muster instance \"nyc\"`) || !strings.Contains(text, "default (primary), la") {
		t.Fatalf("refusal: %s", text)
	}
	text = toolErrorText(t, postBody(t, p, caller("%7"), callBody("agents_list", map[string]any{"instance": 3})))
	if !strings.Contains(text, "instance must be a string") {
		t.Fatalf("a number: %s", text)
	}
	if a, b := len(primary.calls()), len(la.calls()); a != 0 || b != 0 {
		t.Fatalf("calls sent: primary %d, la %d; want none", a, b)
	}
}

// TestASessionBoundToolIsRefusedOffThePrimary: attention_ask, which no server
// offers a remote caller, is refused for la — tied to this session — and runs
// on the primary without instance (the control).
func TestASessionBoundToolIsRefusedOffThePrimary(t *testing.T) {
	p, primary, la := twoInstances(t)
	text := toolErrorText(t, postBody(t, p, caller("%7"), callBody("attention_ask", map[string]any{"question": "q?", "instance": "la"})))
	if !strings.Contains(text, "attention_ask is tied to this session") || !strings.Contains(text, "mirrored to default") {
		t.Fatalf("refusal: %s", text)
	}
	if n := len(la.calls()); n != 0 {
		t.Fatalf("la ran %d calls", n)
	}
	if texts, isErr := callResult(t, postBody(t, p, caller("%7"), callBody("attention_ask", map[string]any{"question": "q?"}))); isErr || !strings.Contains(texts[1], `"ran":"attention_ask"`) {
		t.Fatalf("control, on the primary: %q", texts)
	}
	if n := len(primary.calls()); n != 1 {
		t.Fatalf("primary calls: %d", n)
	}
}

// TestASpawnedWindowCannotReachAnotherInstance: a window muster started for
// the chief (its dispatch tag) is refused another instance's admin tools —
// that instance cannot read this host's listing to refuse it itself.
func TestASpawnedWindowCannotReachAnotherInstance(t *testing.T) {
	p, _, la := twoInstances(t)
	p.Windows = spawnedWindows{}
	text := toolErrorText(t, postBody(t, p, caller("%7"), callBody("agents_list", map[string]any{"instance": "la"})))
	if !strings.Contains(text, "started by muster for the chief") {
		t.Fatalf("refusal: %s", text)
	}
	if n := len(la.calls()); n != 0 {
		t.Fatalf("la ran %d calls", n)
	}
}

type spawnedWindows struct{}

func (spawnedWindows) Windows(context.Context) ([]WindowInfo, error) {
	return []WindowInfo{{Session: "agents", ID: "@1", Pane: "%7", Dispatch: 42, Claude: &ClaudePane{Pane: "%7", Gen: proxySID}}}, nil
}

// servedTools is /mcp's tools/list for a Claude Code caller, by name.
func servedTools(t *testing.T, p *MCPProxy) (map[string]map[string]any, []string) {
	t.Helper()
	w := postBody(t, p, caller("%7"), listBody)
	var r struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("tools/list: %d %s", w.Code, w.Body)
	}
	out := map[string]map[string]any{}
	var names []string
	for _, tl := range r.Result.Tools {
		n, _ := tl["name"].(string)
		out[n] = tl
		names = append(names, n)
	}
	return out, names
}

func props(t *testing.T, tl map[string]any) (map[string]any, []any) {
	t.Helper()
	s, _ := tl["inputSchema"].(map[string]any)
	p, _ := s["properties"].(map[string]any)
	req, _ := s["required"].([]any)
	return p, req
}

// TestTheListIsTheUnionWithPerInstanceDifferences: the served list is every
// instance's tools; an admin tool takes instance (an enum of the instances
// offering it); an argument only some instances take says which, and a call
// passing it to another is REFUSED, never sent with the argument dropped; a
// tool the primary lacks requires instance and is refused without one; a
// session-bound tool takes no instance.
func TestTheListIsTheUnionWithPerInstanceDifferences(t *testing.T) {
	p, _, la := twoInstances(t)
	tools, names := servedTools(t, p)
	if strings.Join(names, " ") != "attention_ask dispatch_request agents_list la_only" {
		t.Fatalf("served %v", names)
	}
	if ps, _ := props(t, tools["attention_ask"]); ps["instance"] != nil {
		t.Fatalf("attention_ask takes instance: %v", ps)
	}
	ps, req := props(t, tools["dispatch_request"])
	inst, _ := ps["instance"].(map[string]any)
	if enum, _ := json.Marshal(inst["enum"]); string(enum) != `["default","la"]` {
		t.Fatalf("dispatch_request's instance: %v", inst)
	}
	wp, _ := ps["worker_pool"].(map[string]any)
	if d, _ := wp["description"].(string); !strings.Contains(d, "Only on muster instance default") {
		t.Fatalf("worker_pool: %v", wp)
	}
	if r, _ := json.Marshal(req); string(r) != `["prompt"]` {
		t.Fatalf("dispatch_request requires %s", r)
	}
	ps, req = props(t, tools["la_only"])
	inst, _ = ps["instance"].(map[string]any)
	if enum, _ := json.Marshal(inst["enum"]); string(enum) != `["la"]` || !strings.Contains(string(mustJSON(req)), `"instance"`) {
		t.Fatalf("la_only: instance %v, required %v", inst, req)
	}

	// The argument la lacks: refused for la, naming it.
	text := toolErrorText(t, postBody(t, p, caller("%7"), callBody("dispatch_request", map[string]any{"prompt": "go", "worker_pool": "gpu", "instance": "la"})))
	if !strings.Contains(text, `takes no \"worker_pool\"`) || !strings.Contains(text, "instance la") {
		t.Fatalf("worker_pool on la: %s", text)
	}
	if n := len(la.calls()); n != 0 {
		t.Fatalf("la ran %d calls", n)
	}
	// Control: without it, la runs it.
	if texts, isErr := callResult(t, postBody(t, p, caller("%7"), callBody("dispatch_request", map[string]any{"prompt": "go", "instance": "la"}))); isErr || texts[0] != "[muster instance: la]" {
		t.Fatalf("control: %q", texts)
	}
	// The tool the primary lacks, named without instance: refused, naming la.
	text = toolErrorText(t, postBody(t, p, caller("%7"), callBody("la_only", map[string]any{"x": "1"})))
	if !strings.Contains(text, "default does not offer la_only") || !strings.Contains(text, "one of: la") {
		t.Fatalf("la_only on the primary: %s", text)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// TestAnOlderInstanceIsRefusedByName: an instance whose remote list lacks the
// mark (a muster from before MCP-only hosts) contributes no tool, and a call
// naming it is refused saying why — never sent there.
func TestAnOlderInstanceIsRefusedByName(t *testing.T) {
	p, _, la := twoInstances(t)
	la.mu.Lock()
	la.mark = false
	la.mu.Unlock()
	if _, names := servedTools(t, p); strings.Join(names, " ") != "attention_ask dispatch_request agents_list" {
		t.Fatalf("served %v", names)
	}
	text := toolErrorText(t, postBody(t, p, caller("%7"), callBody("agents_list", map[string]any{"instance": "la"})))
	if !strings.Contains(text, "too old") {
		t.Fatalf("refusal: %s", text)
	}
	if n := len(la.calls()); n != 0 {
		t.Fatalf("la ran %d calls", n)
	}
}

// TestListChangedIsSentWhenAnUpstreamListChanges: initialize advertises
// tools.listChanged; a GET holds the stream open; a watch that finds the same
// lists sends nothing (the control), and one that finds la's list changed
// sends notifications/tools/list_changed on the stream — and so does a change
// of the primary's own list with no extra instance at all. With the watch off,
// GET is the transport's 405 and initialize is relayed unchanged.
func TestListChangedIsSentWhenAnUpstreamListChanges(t *testing.T) {
	for _, multi := range []bool{true, false} {
		p, primary, la := twoInstances(t)
		if !multi {
			p.Extras = nil
		}
		p.ToolsRefresh = time.Hour // pollTools is driven by hand
		srv := httptest.NewServer(p)
		t.Cleanup(srv.Close)

		r := decodeReply(t, "initialize", postBody(t, p, caller("%7"), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
		if !strings.Contains(string(r.Result), `"listChanged":true`) {
			t.Fatalf("multi=%v: initialize: %s", multi, r.Result)
		}

		// Cleanups run last-first: the stream is closed before srv.Close,
		// which waits for open connections — so a failing check below ends
		// the test instead of hanging it.
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/mcp", nil)
		for k, v := range caller("%7") {
			req.Header.Set(k, v)
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("multi=%v: GET: %v %+v", multi, err, resp)
		}
		t.Cleanup(func() { resp.Body.Close() })
		events := make(chan string, 8)
		go func() {
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				if l := sc.Text(); strings.HasPrefix(l, "data: ") {
					events <- l
				}
			}
		}()
		p.pollTools(ctx) // the baseline
		p.pollTools(ctx) // nothing changed
		select {
		case e := <-events:
			t.Fatalf("multi=%v: an event with no change: %s", multi, e)
		case <-time.After(200 * time.Millisecond):
		}
		if multi {
			la.set(nil, []map[string]any{tool("dispatch_request", "prompt!", "worker_pool"), tool("agents_list"), tool("la_only", "x")})
		} else {
			primary.set([]map[string]any{tool("attention_ask", "question!"), tool("dispatch_request", "prompt!", "worker_pool", "model"), tool("agents_list")}, nil)
		}
		p.pollTools(ctx)
		select {
		case e := <-events:
			if e != `data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` {
				t.Fatalf("multi=%v: event %s", multi, e)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("multi=%v: no list_changed after an upstream change", multi)
		}
	}

	// The watch off: 405, and initialize unchanged.
	p, _, _ := twoInstances(t)
	p.Extras = nil
	g := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	for k, v := range caller("%7") {
		g.Header.Set(k, v)
	}
	g.Header.Set("Accept", "text/event-stream")
	gw := httptest.NewRecorder()
	p.ServeHTTP(gw, g)
	if gw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("watch off: GET %d", gw.Code)
	}
	r := decodeReply(t, "initialize", postBody(t, p, caller("%7"), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if strings.Contains(string(r.Result), "listChanged") {
		t.Fatalf("watch off: initialize %s", r.Result)
	}
}

// TestNoSessionDataReachesAnExtraInstance: through everything a session does
// — initialize, tools/list, the watch, a session-bound call, a call naming no
// instance, a refused one and one naming la — la is sent tools/list and the
// ONE call that named it, and nothing else: no initialize, no pane, tmux or
// Claude session header, no path but /mirror/mcp, always the remote marker
// and la's own token. The primary (the control) sees the session-bound calls.
func TestNoSessionDataReachesAnExtraInstance(t *testing.T) {
	p, primary, la := twoInstances(t)
	p.ToolsRefresh = time.Hour
	_ = postBody(t, p, caller("%7"), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	_ = postBody(t, p, caller("%7"), `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_, _ = servedTools(t, p)
	p.pollTools(context.Background())
	_ = postBody(t, p, caller("%7"), callBody("attention_ask", map[string]any{"question": "secret plan?"}))
	_ = postBody(t, p, caller("%7"), callBody("agents_list", nil))
	_ = postBody(t, p, caller("%7"), callBody("attention_ask", map[string]any{"question": "secret plan?", "instance": "la"}))
	_ = postBody(t, p, caller("%7"), callBody("agents_list", map[string]any{"instance": "la"}))

	seen := la.requests()
	if len(seen) == 0 {
		t.Fatal("POSITIVE CONTROL: la was sent nothing at all; the instance is not wired")
	}
	calls := 0
	for _, r := range seen {
		if r.Method != http.MethodPost || r.Path != "/mirror/mcp" {
			t.Errorf("la was sent %s %s", r.Method, r.Path)
		}
		if r.Header.Get(MCPClientHeader) != MCPClientRemote || r.Header.Get("Authorization") != "Bearer "+laToken {
			t.Errorf("la was sent a request without the remote marker or with another token: %v", r.Header)
		}
		for _, h := range []string{MCPSessionHeader, MCPPaneHeader, MCPTmuxHeader, MCPAgentSessionHeader} {
			if r.Header.Get(h) != "" {
				t.Errorf("la was sent %s", h)
			}
		}
		if strings.Contains(r.Body, "secret plan") {
			t.Errorf("la was sent a session-bound call's arguments: %s", r.Body)
		}
		switch r.RPC {
		case "tools/list":
		case "tools/call":
			calls++
			if !strings.Contains(r.Body, `"name":"agents_list"`) {
				t.Errorf("la ran %s", r.Body)
			}
		default:
			t.Errorf("la was sent %s", r.RPC)
		}
	}
	if calls != 1 {
		t.Fatalf("la ran %d calls; want exactly the one that named it", calls)
	}
	if n := len(primary.calls()); n != 2 {
		t.Fatalf("CONTROL: the primary ran %d calls; want the two that named no instance", n)
	}
}

// TestAToolsOwnInstanceArgumentIsLeftAlone: a primary tool that already takes
// an argument named instance is served as it is and called with it intact,
// on the primary — never routed by it.
func TestAToolsOwnInstanceArgumentIsLeftAlone(t *testing.T) {
	p, primary, la := twoInstances(t)
	primary.set([]map[string]any{tool("flow_run", "flow!", "instance")}, []map[string]any{tool("flow_run", "flow!", "instance")})
	tools, _ := servedTools(t, p)
	ps, _ := props(t, tools["flow_run"])
	if inst, _ := ps["instance"].(map[string]any); inst["enum"] != nil {
		t.Fatalf("flow_run's own instance argument was replaced: %v", inst)
	}
	if texts, isErr := callResult(t, postBody(t, p, caller("%7"), callBody("flow_run", map[string]any{"flow": "f", "instance": "personal"}))); isErr || !strings.Contains(texts[0], `"instance":"personal"`) {
		t.Fatalf("flow_run: %q", texts)
	}
	if a, b := len(primary.calls()), len(la.calls()); a != 1 || b != 0 {
		t.Fatalf("calls: primary %d, la %d", a, b)
	}
}
