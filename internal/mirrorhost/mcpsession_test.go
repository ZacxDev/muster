package mirrorhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// These tests name the session header by its literal value, not by the
// package's constant, so they build against code that predates it.
const sessionHeader = "Mcp-Session-Id"

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`

// initSession initializes as the Claude Code session in pane: that caller's
// headers, with the session id the answer issued ("" when it issued none).
func initSession(t *testing.T, p *MCPProxy, pane string) (map[string]string, string) {
	t.Helper()
	w := postBody(t, p, caller(pane), initBody)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize: %d %s", w.Code, w.Body)
	}
	hdr := caller(pane)
	id := w.Header().Get(sessionHeader)
	if id != "" {
		hdr[sessionHeader] = id
	}
	return hdr, id
}

// openSession is initSession, failing when no session was issued.
func openSession(t *testing.T, p *MCPProxy, pane string) map[string]string {
	t.Helper()
	hdr, id := initSession(t, p, pane)
	if id == "" {
		t.Fatalf("initialize issued no %s", sessionHeader)
	}
	return hdr
}

// sessionStream opens GET /mcp on url with hdr, answering the response.
func sessionStream(t *testing.T, ctx context.Context, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/mcp", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// heldSession is initSession, carrying a made-up id when none was issued:
// what a client that kept an id across a restart sends either way.
func heldSession(t *testing.T, p *MCPProxy, pane string) map[string]string {
	t.Helper()
	hdr, id := initSession(t, p, pane)
	if id == "" {
		hdr[sessionHeader] = "0123456789abcdef0123456789abcdef"
	}
	return hdr
}

// withAccept is hdr with the Accept a streamable-HTTP client sends.
func withAccept(hdr map[string]string) map[string]string {
	out := map[string]string{"Accept": "application/json, text/event-stream"}
	for k, v := range hdr {
		out[k] = v
	}
	return out
}

// sseMessages is the data of each event in an event-stream body.
func sseMessages(body string) []string {
	var out []string
	for _, ev := range strings.Split(body, "\n\n") {
		var data []string
		for _, l := range strings.Split(ev, "\n") {
			if d, ok := strings.CutPrefix(l, "data: "); ok {
				data = append(data, d)
			}
		}
		if len(data) > 0 {
			out = append(out, strings.Join(data, "\n"))
		}
	}
	return out
}

const listChangedData = `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`

// TestAStaleSessionIsToldOnItsNextAnswer is the reported defect: a session
// that listed the tools, holds NO open GET stream, and then the served set
// changes (muster upgraded; the watch re-read it). Its next request's answer
// must carry notifications/tools/list_changed — first, as an event stream —
// and then the call's own response; once told, it is not told again until it
// lists and the set changes again. A session that listed the CURRENT set (the
// control) is never told.
func TestAStaleSessionIsToldOnItsNextAnswer(t *testing.T) {
	primary := newFakeInstance(t, []map[string]any{tool("attention_ask", "question!"), tool("agents_list")}, nil, true)
	p := proxyFor(primary.srv.URL)
	p.ToolsRefresh = time.Hour // the watch is driven by hand
	ctx := context.Background()

	stale, _ := initSession(t, p, "%7")
	fresh, _ := initSession(t, p, "%7")
	p.pollTools(ctx) // the watch's first read: the baseline
	_ = postBody(t, p, stale, listBody)

	// Control first: the set has not changed, so the answer is plain JSON.
	w := postBody(t, p, withAccept(stale), callBody("agents_list", nil))
	if ct := w.Header().Get("Content-Type"); ct != "application/json" || strings.Contains(w.Body.String(), "list_changed") {
		t.Fatalf("CONTROL: an unchanged set was announced: %q %s", ct, w.Body)
	}

	// muster is upgraded: a new tool. The watch re-reads it.
	primary.set([]map[string]any{tool("attention_ask", "question!"), tool("agents_list"), tool("page_create", "title!")}, nil)
	p.pollTools(ctx)
	_ = postBody(t, p, fresh, listBody) // fresh lists the NEW set

	w = postBody(t, p, withAccept(stale), callBody("agents_list", nil))
	msgs := sseMessages(w.Body.String())
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" || len(msgs) != 2 || msgs[0] != listChangedData {
		t.Fatalf("the stale session's next answer did not carry list_changed first: %q %q", ct, w.Body)
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(msgs[1]), &resp) != nil || string(resp.ID) != "7" || len(resp.Result.Content) != 1 || !strings.Contains(resp.Result.Content[0].Text, `"ran":"agents_list"`) {
		t.Fatalf("the call's own response was not the second message: %s", msgs[1])
	}

	// Told once: the next answer is plain again.
	if w = postBody(t, p, withAccept(stale), callBody("agents_list", nil)); strings.Contains(w.Body.String(), "list_changed") {
		t.Fatalf("told twice about one change: %s", w.Body)
	}
	// The session that listed the new set is never told.
	if w = postBody(t, p, withAccept(fresh), callBody("agents_list", nil)); strings.Contains(w.Body.String(), "list_changed") {
		t.Fatalf("CONTROL: a session holding the current list was told: %s", w.Body)
	}
	// It re-lists, the set changes again (a feature toggle drops a tool): told again.
	_ = postBody(t, p, stale, listBody)
	primary.set([]map[string]any{tool("attention_ask", "question!"), tool("page_create", "title!")}, nil)
	p.pollTools(ctx)
	if msgs = sseMessages(postBody(t, p, withAccept(stale), callBody("agents_list", nil)).Body.String()); len(msgs) != 2 || msgs[0] != listChangedData {
		t.Fatalf("not told about the second change: %q", msgs)
	}
}

// TestAStreamOpenedAfterAChangeIsToldAtOnce: a session whose GET stream was
// down when the set changed (the mirror's listener was busy, the client was
// reconnecting it) is told as soon as the stream opens, not at the next
// change — and one holding the current list is not.
func TestAStreamOpenedAfterAChangeIsToldAtOnce(t *testing.T) {
	primary := newFakeInstance(t, []map[string]any{tool("agents_list")}, nil, true)
	p := proxyFor(primary.srv.URL)
	p.ToolsRefresh = time.Hour
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stale := heldSession(t, p, "%7")
	fresh := heldSession(t, p, "%7")
	_ = postBody(t, p, stale, listBody)
	primary.set([]map[string]any{tool("agents_list"), tool("notify_owner", "text!")}, nil)
	p.pollTools(ctx)
	_ = postBody(t, p, fresh, listBody)

	first := func(hdr map[string]string) (string, bool) {
		resp := sessionStream(t, ctx, srv.URL, hdr)
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET: %d", resp.StatusCode)
		}
		got := make(chan string, 1)
		go func() {
			buf := make([]byte, 4096)
			n, _ := resp.Body.Read(buf)
			got <- string(buf[:n])
		}()
		select {
		case s := <-got:
			return s, true
		case <-time.After(400 * time.Millisecond):
			return "", false
		}
	}
	if s, ok := first(stale); !ok || len(sseMessages(s)) != 1 || sseMessages(s)[0] != listChangedData {
		t.Fatalf("the stale session's new stream: %v %q", ok, s)
	}
	if s, ok := first(fresh); ok && strings.Contains(s, "list_changed") {
		t.Fatalf("CONTROL: a session holding the current list was told: %q", s)
	}
}

// claudeCodeExpired is Claude Code's own test of an expired MCP session's
// error text (2.1.289's bundled client, isMcpSessionExpiredError): a 400 is an
// expired session only when its text matches this.
var claudeCodeExpired = regexp.MustCompile(`(?i)Server not initialized|No valid session ID|Mcp-Session-Id header is required`)

// TestAnUnknownSessionIsAnsweredAsExpired: a session id this process never
// issued — the mirror restarted, and the client kept the old one — is the
// transport's 404 on a POST and on the GET stream, and its call is NOT run;
// a request with no session id at all (a client connected before ids were
// issued) is the 400 whose words Claude Code takes as an expired session.
// Both make Claude Code initialize again and so read the current list. The
// controls: the process that issued the id runs the call; an opencode caller
// and a proxy with the watch off are not held to sessions.
func TestAnUnknownSessionIsAnsweredAsExpired(t *testing.T) {
	primary := newFakeInstance(t, []map[string]any{tool("agents_list")}, nil, true)
	before := proxyFor(primary.srv.URL)
	before.ToolsRefresh = time.Hour
	sess := heldSession(t, before, "%7")

	// The mirror restarts: a new process, the same muster.
	after := proxyFor(primary.srv.URL)
	after.ToolsRefresh = time.Hour
	w := postBody(t, after, withAccept(sess), callBody("agents_list", nil))
	var rep struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if w.Code != http.StatusNotFound || json.Unmarshal(w.Body.Bytes(), &rep) != nil || rep.Error.Code != -32001 {
		t.Fatalf("an unknown session's call: %d %s", w.Code, w.Body)
	}
	if n := len(primary.calls()); n != 0 {
		t.Fatalf("a call under an unknown session ran upstream (%d)", n)
	}
	srv := httptest.NewServer(after)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if resp := sessionStream(t, ctx, srv.URL, sess); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("an unknown session's GET: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// No session id at all.
	w = postBody(t, after, withAccept(caller("%7")), callBody("agents_list", nil))
	if w.Code != http.StatusBadRequest || !claudeCodeExpired.MatchString("Error POSTing to endpoint: "+w.Body.String()) {
		t.Fatalf("a call with no session: %d %s", w.Code, w.Body)
	}
	if n := len(primary.calls()); n != 0 {
		t.Fatalf("a call with no session ran upstream (%d)", n)
	}

	// Controls.
	if w = postBody(t, before, withAccept(sess), callBody("agents_list", nil)); w.Code != http.StatusOK || len(primary.calls()) != 1 {
		t.Fatalf("CONTROL: the issuing process: %d %s", w.Code, w.Body)
	}
	off := proxyFor(primary.srv.URL)
	if w = postBody(t, off, caller("%7"), callBody("agents_list", nil)); w.Code != http.StatusOK || len(primary.calls()) != 2 {
		t.Fatalf("CONTROL: the watch off: %d %s", w.Code, w.Body)
	}
	if w = postBody(t, after, map[string]string{"Authorization": "Bearer " + proxySecret, MCPClientHeader: MCPClientOpenCode}, listBody); w.Code != http.StatusOK {
		t.Fatalf("CONTROL: an opencode caller: %d %s", w.Code, w.Body)
	}
}
