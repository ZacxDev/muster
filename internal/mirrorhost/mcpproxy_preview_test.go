package mirrorhost

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/previewhost"
)

// /preview-mcp (docs/design/previews.md, "The MCP tools"): a SEPARATE MCP
// server on the same loopback endpoint, behind the same guard as /mcp,
// answered on this machine — so /mcp is exactly what it was, a muster outage
// still fails it loudly, and no preview_* request ever reaches muster.

// musterTools is what the fake muster answers a tools/list with.
const musterTools = `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"attention_ask"},{"name":"task_get"}]}}` + "\n"

// previewProxy is proxyFor(base) with the real preview tools mounted, bound
// with the proxy's own CallerWindow.
func previewProxy(base string) *MCPProxy {
	p := proxyFor(base)
	h := previewhost.NewHost(previewhost.HostConfig{Config: previewhost.Config{MaxOpen: 8}, Logger: log.New(io.Discard, "", 0)})
	p.SetPreview(&previewhost.MCPServer{Host: h, Caller: p.CallerWindow})
	return p
}

// postTo is postBody to path.
func postTo(t *testing.T, h http.Handler, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// caller is a request from pane of this host's tmux server, with the secret.
func caller(pane string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: pane, MCPTmuxHeader: proxyTMUX}
}

func toolNames(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	r := decodeReply(t, "tools/list", w)
	var l struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &l); err != nil {
		t.Fatalf("tools/list result %s: %v", r.Result, err)
	}
	names := []string{}
	for _, tl := range l.Tools {
		names = append(names, tl.Name)
	}
	return names
}

const listBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// TestEachServerListsItsOwnTools: /preview-mcp lists exactly the three
// preview tools, and /mcp is forwarded and answered with muster's list byte
// for byte — not one preview tool appended.
func TestEachServerListsItsOwnTools(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, musterTools)
	})
	p := previewProxy(srv.URL)
	if got := toolNames(t, postTo(t, p, PreviewMCPPath, caller("%7"), listBody)); !slices.Equal(got, []string{"preview_open", "preview_list", "preview_close"}) {
		t.Fatalf("/preview-mcp lists %v", got)
	}
	if up.n() != 0 {
		t.Fatalf("/preview-mcp's tools/list reached muster")
	}
	w := postTo(t, p, "/mcp", caller("%7"), listBody)
	if w.Code != http.StatusOK || w.Body.String() != musterTools {
		t.Fatalf("/mcp answered %d %q; want muster's answer unchanged %q", w.Code, w.Body, musterTools)
	}
	if up.n() != 1 {
		t.Fatalf("/mcp's tools/list reached muster %d time(s); want 1", up.n())
	}
}

// TestWithMusterDownMCPStillFailsAndThePreviewToolsWork: muster unreachable,
// /mcp answers 502 as before — the outage stays loud — while /preview-mcp
// answers initialize, tools/list and a preview_list.
func TestWithMusterDownMCPStillFailsAndThePreviewToolsWork(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()
	p := previewProxy(dead)
	for _, body := range []string{listBody, proxyCall, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`} {
		w := postTo(t, p, "/mcp", caller("%7"), body)
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "muster could not be reached") {
			t.Fatalf("/mcp with muster down, %s: %d %s; want the 502", body, w.Code, w.Body)
		}
	}
	init := decodeReply(t, "initialize", postTo(t, p, PreviewMCPPath, caller("%7"), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if !strings.Contains(string(init.Result), `"name":"muster-preview"`) {
		t.Fatalf("/preview-mcp initialize: %s", init.Result)
	}
	if got := toolNames(t, postTo(t, p, PreviewMCPPath, caller("%7"), listBody)); len(got) != 3 {
		t.Fatalf("/preview-mcp with muster down lists %v", got)
	}
	r := decodeReply(t, "preview_list", postTo(t, p, PreviewMCPPath, caller("%8"), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"preview_list","arguments":{}}}`))
	if r.Error != nil || !strings.Contains(string(r.Result), `{\"previews\":[]}`) {
		t.Fatalf("preview_list with muster down: %s %v", r.Result, r.Error)
	}
}

// TestNoPreviewRequestReachesMuster: every kind of request to /preview-mcp —
// the preview tools' calls included — reaches muster ZERO times. The positive
// control: one muster tool call through /mcp counts one.
func TestNoPreviewRequestReachesMuster(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":7,"result":{}}`) })
	p := previewProxy(srv.URL)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		listBody,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"preview_open","arguments":{"port":5173}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"preview_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"preview_close","arguments":{"id":"p-1"}}}`,
		proxyCall, // a MUSTER tool, sent to the wrong server
		`{"jsonrpc":"2.0","id":6,"method":"server/discover"}`,
		`not json`,
	} {
		if w := postTo(t, p, PreviewMCPPath, caller("%7"), body); w.Code != http.StatusOK && w.Code != http.StatusAccepted {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if n := up.n(); n != 0 {
		t.Fatalf("muster received %d request(s) from /preview-mcp; want 0", n)
	}
	if w := postTo(t, p, "/mcp", caller("%7"), proxyCall); w.Code != http.StatusOK {
		t.Fatalf("control: %d %s", w.Code, w.Body)
	}
	if n := up.n(); n != 1 {
		t.Fatalf("control: one muster tool call through /mcp reached muster %d time(s); want 1", n)
	}
}

// TestBothServersAreBehindTheSameGuard: on each path, no secret or a wrong
// one is 401, any Origin is 403 (with the right secret), a GET is 405 — and
// the preview tools are never reached; while unmounted, /preview-mcp is 404
// like any unknown path.
func TestBothServersAreBehindTheSameGuard(t *testing.T) {
	_, srv := newUpstream(t, func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{}`) })
	var reached atomic.Int32
	p := proxyFor(srv.URL)
	p.SetPreview(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	for _, path := range []string{"/mcp", PreviewMCPPath} {
		for name, c := range map[string]struct {
			hdr  map[string]string
			want int
		}{
			"no secret":     {map[string]string{MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX}, http.StatusUnauthorized},
			"wrong secret":  {map[string]string{"Authorization": "Bearer " + proxySecret + "x", MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX}, http.StatusUnauthorized},
			"an Origin":     {map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX, "Origin": "http://127.0.0.1"}, http.StatusForbidden},
			"a null Origin": {map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX, "Origin": "null"}, http.StatusForbidden},
		} {
			if w := postTo(t, p, path, c.hdr, listBody); w.Code != c.want {
				t.Errorf("%s, %s: %d %s; want %d", path, name, w.Code, w.Body, c.want)
			}
		}
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+proxySecret)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s GET: %d", path, w.Code)
		}
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the preview tools were reached %d time(s) past a refusal", n)
	}
	if w := postTo(t, p, PreviewMCPPath, caller("%7"), listBody); w.Code != http.StatusOK || reached.Load() != 1 {
		t.Fatalf("control: an allowed request: %d, reached %d", w.Code, reached.Load())
	}
	if w := postTo(t, proxyFor(srv.URL), PreviewMCPPath, caller("%7"), listBody); w.Code != http.StatusNotFound {
		t.Fatalf("never mounted /preview-mcp: %d; want 404", w.Code)
	}
}

// splitWindows: @1's active pane %7 runs Claude Code, its split %9 another,
// and %10 a shell; @2's only pane %8 is a shell.
type splitWindows struct{}

func (splitWindows) Windows(_ context.Context) ([]WindowInfo, error) {
	return []WindowInfo{
		{Session: "work", ID: "@1", Pane: "%7", Claude: &ClaudePane{Pane: "%7", Gen: proxySID}, Others: []ClaudePane{{Pane: "%9", Gen: splitSID}},
			panes: []pane{{Session: "work", ID: "%9", PID: 91}, {Session: "work", ID: "%10", PID: 92}}},
		{Session: "work", ID: "@2", Pane: "%8"},
	}, nil
}

// TestThePreviewCallerIsThePanesWindow: any pane of a window — the active
// one, a split running Claude Code, a split running a shell — names that
// window, and a pane with NO Claude Code session works too (unlike
// session(), which muster's tools need); each window's pane gets its own
// window, and an unknown pane none. Through the endpoint, the handler sees
// the window of the pane that called.
func TestThePreviewCallerIsThePanesWindow(t *testing.T) {
	p := proxyFor("http://127.0.0.1:1")
	p.Windows = splitWindows{}
	for pane, want := range map[string]string{"%7": "@1", "%9": "@1", "%10": "@1", "%8": "@2"} {
		r := httptest.NewRequest(http.MethodPost, PreviewMCPPath, nil)
		r.Header.Set(MCPPaneHeader, pane)
		if got, err := p.CallerWindow(r, ""); err != nil || got != want {
			t.Errorf("pane %s: %q, %v; want %s", pane, got, err, want)
		}
	}
	// The contrast: muster's tools need a Claude session in the pane.
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set(MCPPaneHeader, "%10")
	if _, err := p.session(r.Context(), r); err == nil {
		t.Fatal("session() resolved a shell pane; the contrast is gone")
	}
	for _, pane := range []string{"%99", ""} {
		r := httptest.NewRequest(http.MethodPost, PreviewMCPPath, nil)
		r.Header.Set(MCPPaneHeader, pane)
		if got, err := p.CallerWindow(r, ""); err == nil {
			t.Errorf("pane %q resolved to %q", pane, got)
		}
	}

	p.SetPreview(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		win, err := p.CallerWindow(r, "")
		if err != nil {
			win = "error: " + err.Error()
		}
		_, _ = io.WriteString(w, win)
	}))
	for pane, want := range map[string]string{"%10": "@1", "%8": "@2"} {
		if w := postTo(t, p, PreviewMCPPath, caller(pane), listBody); w.Body.String() != want {
			t.Errorf("through the endpoint, pane %s: %q; want %s", pane, w.Body, want)
		}
	}
}

// TestAnOutsiderOfThePreviewServerGetsNoTools: a caller from another tmux
// server (its pane ids name other panes) is answered as a healthy
// "muster-preview" with no tools, and the preview tools never see it.
func TestAnOutsiderOfThePreviewServerGetsNoTools(t *testing.T) {
	var reached atomic.Int32
	p := proxyFor("http://127.0.0.1:1")
	p.SetPreview(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	hdr := map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", MCPTmuxHeader: outsiderTMUX}
	if got := toolNames(t, postTo(t, p, PreviewMCPPath, hdr, listBody)); len(got) != 0 {
		t.Fatalf("an outsider lists %v", got)
	}
	init := decodeReply(t, "initialize", postTo(t, p, PreviewMCPPath, hdr, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if !strings.Contains(string(init.Result), `"name":"muster-preview"`) {
		t.Fatalf("outsider initialize: %s", init.Result)
	}
	if r := decodeReply(t, "call", postTo(t, p, PreviewMCPPath, hdr, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"preview_list","arguments":{}}}`)); r.Error == nil {
		t.Fatalf("an outsider's call was answered: %s", r.Result)
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the preview tools saw an outsider %d time(s)", n)
	}
}
