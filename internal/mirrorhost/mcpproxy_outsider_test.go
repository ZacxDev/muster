package mirrorhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A caller from ANOTHER tmux server (docs/design/multi-tenant.md, "The local
// MCP endpoint", decision 5 as revised): on a host with one muster-mirror per
// muster server, every Claude Code session reaches every tenant's endpoint.
// Measured on Claude Code 2.1.289, a 400 there puts the error into the
// model's system prompt in every session; so the endpoint answers such a
// caller ITSELF as a healthy server with NO tools, and forwards nothing.

// outsiderTMUX is $TMUX in a pane of a tmux server this host does not mirror.
const outsiderTMUX = "/run/user/1000/tmux-1000/work,5151,0"

// rpcReply is one JSON-RPC response as the caller parses it.
type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeReply(t *testing.T, name string, w *httptest.ResponseRecorder) rpcReply {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s, want 200", name, w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s: Content-Type %q", name, ct)
	}
	var r rpcReply
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("%s: not one JSON-RPC response: %v: %s", name, err, w.Body)
	}
	if r.JSONRPC != "2.0" {
		t.Fatalf("%s: jsonrpc %q", name, r.JSONRPC)
	}
	return r
}

// TestAnOutsiderSeesAHealthyServerWithNoToolsAndNothingIsForwarded walks
// Claude Code's own sequence (server/discover, initialize,
// notifications/initialized, tools/list, then its GET fallback), plus a
// tools/call and another method, from another tmux server — and upstream
// receives ZERO requests. The positive control: the same sequence from this
// host's tmux server is forwarded, request for request.
func TestAnOutsiderSeesAHealthyServerWithNoToolsAndNothingIsForwarded(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"attention_ask"}]}}`)
	})
	var logs bytes.Buffer
	p := proxyFor(srv.URL)
	p.Version = "v-fixture-7"
	p.Logger = log.New(&logs, "", 0)
	hdr := map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", MCPTmuxHeader: outsiderTMUX}

	// 1. server/discover: muster's own "method not found", as against the
	// right server, so Claude Code goes on to initialize.
	r := decodeReply(t, "server/discover", postBody(t, p, hdr, `{"jsonrpc":"2.0","id":0,"method":"server/discover","params":{}}`))
	if r.Error == nil || r.Error.Code != -32601 || string(r.ID) != "0" || r.Result != nil {
		t.Fatalf("server/discover: %+v", r)
	}

	// 2. initialize: a valid result naming the protocol it asked for.
	r = decodeReply(t, "initialize", postBody(t, p, hdr, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.289"}}}`))
	if r.Error != nil || string(r.ID) != "1" {
		t.Fatalf("initialize: %+v", r)
	}
	var init struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      struct{ Name, Version string }
		Instructions    *string `json:"instructions"`
	}
	if err := json.Unmarshal(r.Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != "2025-03-26" || string(init.Capabilities["tools"]) != "{}" || len(init.Capabilities) != 1 ||
		init.ServerInfo.Name != "muster" || init.ServerInfo.Version != "v-fixture-7" {
		t.Fatalf("initialize result %s", r.Result)
	}
	// 🔴 Nothing for the model's context: no instructions at all.
	if init.Instructions != nil {
		t.Fatalf("initialize carries instructions %q: they reach the model's context", *init.Instructions)
	}
	// An unknown protocol revision gets the newest, as muster answers.
	r = decodeReply(t, "initialize (unknown version)", postBody(t, p, hdr, `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`))
	if !strings.Contains(string(r.Result), `"protocolVersion":"2025-06-18"`) || string(r.ID) != `"a"` {
		t.Fatalf("initialize, unknown version: %s", r.Result)
	}

	// 3. notifications/initialized: 202, no body.
	w := postBody(t, p, hdr, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("notification: %d %q", w.Code, w.Body)
	}

	// 4. tools/list: an EMPTY list (a JSON array, not null).
	r = decodeReply(t, "tools/list", postBody(t, p, hdr, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if r.Error != nil || string(r.Result) != `{"tools":[]}` {
		t.Fatalf("tools/list: %+v %s", r, r.Result)
	}

	// 5. The GET fallback: 405, the transport's healthy "no stream".
	g := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	for k, v := range hdr {
		g.Header.Set(k, v)
	}
	gw := httptest.NewRecorder()
	p.ServeHTTP(gw, g)
	if gw.Code != http.StatusMethodNotAllowed || gw.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET: %d Allow=%q", gw.Code, gw.Header().Get("Allow"))
	}

	// 6. tools/call and anything else: refused.
	r = decodeReply(t, "tools/call", postBody(t, p, hdr, proxyCall))
	if r.Error == nil || r.Error.Code != -32602 || r.Result != nil || string(r.ID) != "7" {
		t.Fatalf("tools/call: %+v", r)
	}
	// ping: answered with an empty result, as muster's own server answers it
	// (internal/api's serveMCP) — MCP requires an answer, and an error would
	// read as an unhealthy server.
	r = decodeReply(t, "ping", postBody(t, p, hdr, `{"jsonrpc":"2.0","id":3,"method":"ping"}`))
	if r.Error != nil || string(r.Result) != `{}` || string(r.ID) != "3" {
		t.Fatalf("ping: %+v %s", r, r.Result)
	}
	for _, body := range []string{`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`} {
		if r := decodeReply(t, body, postBody(t, p, hdr, body)); r.Error == nil || r.Error.Code != -32601 {
			t.Fatalf("%s: %+v", body, r)
		}
	}
	// What is not one request (a batch, garbage): muster's -32600.
	for _, body := range []string{`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, `not json`} {
		if r := decodeReply(t, body, postBody(t, p, hdr, body)); r.Error == nil || r.Error.Code != -32600 || string(r.ID) != "null" {
			t.Fatalf("%s: %+v", body, r)
		}
	}

	// 🔴 THE COUNT: upstream saw NOTHING of the outsider's eleven requests.
	if n := up.n(); n != 0 {
		t.Fatalf("upstream received %d request(s) from a caller outside this host's tmux server, want 0", n)
	}
	// Logged once, not per request.
	if c := strings.Count(logs.String(), "outside this host's tmux server"); c != 1 {
		t.Fatalf("logged %d times, want once: %q", c, logs.String())
	}

	// The positive control: the right tmux server's caller is FORWARDED, with
	// the host's token, request for request (and a tools/list comes back with
	// upstream's tools, not an empty list).
	hdr[MCPTmuxHeader] = proxyTMUX
	for i, body := range []string{
		`{"jsonrpc":"2.0","id":0,"method":"server/discover","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		proxyCall,
	} {
		w := postBody(t, p, hdr, body)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "attention_ask") {
			t.Fatalf("right server, %s: %d %s", body, w.Code, w.Body)
		}
		if up.n() != i+1 {
			t.Fatalf("right server, %s: upstream has %d requests, want %d", body, up.n(), i+1)
		}
		if got := up.calls[i].Header.Get("Authorization"); got != "Bearer "+proxyToken {
			t.Fatalf("forwarded with %q", got)
		}
	}
}

// TestAnOutsiderIsStillRefusedWithoutTheSecret: answering an outsider locally
// comes AFTER the secret and Origin checks — a caller without the secret is
// refused exactly as before, whatever tmux server it names.
func TestAnOutsiderIsStillRefusedWithoutTheSecret(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) {})
	p := proxyFor(srv.URL)
	for name, hdr := range map[string]map[string]string{
		"no secret":    {MCPTmuxHeader: outsiderTMUX},
		"wrong secret": {"Authorization": "Bearer nope", MCPTmuxHeader: outsiderTMUX},
		"an Origin":    {"Authorization": "Bearer " + proxySecret, MCPTmuxHeader: outsiderTMUX, "Origin": "https://evil.example"},
	} {
		w := postBody(t, p, hdr, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "protocolVersion") {
			t.Errorf("%s: answered initialize: %s", name, w.Body)
		}
	}
	if up.n() != 0 {
		t.Fatalf("upstream got %d", up.n())
	}
}

// brokenSocket is a ServerSocketer that cannot read this host's tmux server.
type brokenSocket struct{}

func (brokenSocket) ServerSocket(context.Context) (string, error) {
	return "", errors.New("tmux display-message: no server running")
}

// TestWhenThisHostsTmuxServerCannotBeReadNothingIsForwarded: with this host's
// own tmux server unreadable, no caller can be shown to be in it — not even
// one naming the very socket this host would mirror — so every caller is
// answered as an outsider: initialize and tools/list get the empty-tools
// answer, tools/call is refused, and upstream receives ZERO requests.
func TestWhenThisHostsTmuxServerCannotBeReadNothingIsForwarded(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"attention_ask"}]}}`)
	})
	var logs bytes.Buffer
	p := proxyFor(srv.URL)
	p.Socket = brokenSocket{}
	p.Logger = log.New(&logs, "", 0)
	for _, tmux := range []string{proxyTMUX, outsiderTMUX, ""} {
		hdr := map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", MCPTmuxHeader: tmux}
		r := decodeReply(t, "initialize", postBody(t, p, hdr, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`))
		if r.Error != nil || !strings.Contains(string(r.Result), `"capabilities":{"tools":{}}`) || strings.Contains(string(r.Result), "instructions") {
			t.Fatalf("$TMUX %q: initialize: %+v %s", tmux, r, r.Result)
		}
		r = decodeReply(t, "tools/list", postBody(t, p, hdr, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
		if r.Error != nil || string(r.Result) != `{"tools":[]}` {
			t.Fatalf("$TMUX %q: tools/list: %+v %s", tmux, r, r.Result)
		}
		r = decodeReply(t, "tools/call", postBody(t, p, hdr, proxyCall))
		if r.Error == nil || r.Error.Code != -32602 || r.Result != nil {
			t.Fatalf("$TMUX %q: tools/call: %+v", tmux, r)
		}
	}
	if n := up.n(); n != 0 {
		t.Fatalf("upstream received %d request(s) while this host's tmux server could not be read, want 0", n)
	}
	if !strings.Contains(logs.String(), "read this host's tmux server") {
		t.Fatalf("the reason was not logged: %q", logs.String())
	}
}
