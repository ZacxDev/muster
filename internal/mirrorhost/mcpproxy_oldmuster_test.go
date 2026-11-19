package mirrorhost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A CURRENT HOST PROXY IN FRONT OF AN OLD muster (#833). A muster built
// before the MCP request bound was derived from the page limit read every
// request under a flat 64 KiB and answered any read failure with the reply
// below (HTTP 200, id null). Behind a current muster-mirror, which forwards
// up to 1088 KiB, a 66,000-byte page_update got exactly that: a "malformed
// request" naming no hop and no limit (measured with a server built from
// such a commit: 65,536 bytes read, 66,000 not).

// legacyUnreadReply is that old server's whole answer, byte for byte as its
// writeJSON wrote it (a map, so its keys sorted).
const legacyUnreadReply = `{"error":{"code":-32700,"message":"could not read the request"},"id":null,"jsonrpc":"2.0"}` + "\n"

// oldMuster plays that server: it reads under 64 KiB and answers a body past
// it with legacyUnreadReply, any other with a page_update's success.
func oldMuster(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10)); err != nil {
		_, _ = io.WriteString(w, legacyUnreadReply)
		return
	}
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"{\"page_id\":3,\"version\":2}"}]}}`+"\n")
}

// sizedCall is a JSON-RPC request of method (a page_update when tools/call)
// padded to exactly n bytes.
func sizedCall(t *testing.T, method string, n int) string {
	t.Helper()
	mk := func(pad int) string {
		var params any = map[string]any{"name": "page_update", "arguments": map[string]any{"page_id": 3, "title": "probe",
			"blocks": []map[string]string{{"type": "markdown", "text": strings.Repeat("x", pad)}}}}
		if method != "tools/call" {
			params = map[string]any{"pad": strings.Repeat("x", pad)}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 5, "method": method, "params": params})
		return string(b)
	}
	body := mk(n - len(mk(0)))
	if len(body) != n {
		t.Fatalf("fixture: %d bytes, want %d", len(body), n)
	}
	return body
}

func oldMusterProxy(t *testing.T, h http.HandlerFunc) (*MCPProxy, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return proxyFor(srv.URL), u.Host
}

var oldMusterHdr = map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX}

// TestAnOldMustersUnreadReplyNamesItsCause: the old server's reply to a
// request over 64 KiB reaches the caller as a tool error that names that
// muster, the request's size, the old and new limits and the release that
// raised them — not as -32700. At origin/main the proxy relayed the old
// reply as it was.
func TestAnOldMustersUnreadReplyNamesItsCause(t *testing.T) {
	p, host := oldMusterProxy(t, oldMuster)
	w := postBody(t, p, oldMusterHdr, sizedCall(t, "tools/call", 66000))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %.300s", w.Code, w.Body)
	}
	var rep struct {
		ID     json.RawMessage `json:"id"`
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("not JSON: %.300s", w.Body)
	}
	if rep.Error != nil || !rep.Result.IsError || len(rep.Result.Content) != 1 || string(rep.ID) != "5" {
		t.Fatalf("want a tool error for call 5, got %.400s", w.Body)
	}
	var tool struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(rep.Result.Content[0].Text), &tool)
	want := "muster at " + host + " could not read this MCP request of 66000 bytes: it reads at most 64 KiB per request, " +
		"so it is probably older than v0.239.0 (from v0.239.0 on, page_create and page_update take up to 1088 KiB); " +
		"upgrade that muster, or keep the request under 64 KiB"
	if tool.Error != want {
		t.Fatalf("the tool error:\n got %q\nwant %q", tool.Error, want)
	}
}

// TestAnOldMustersUnreadReplyToAnotherMethodIsAnInvalidRequest: anything but
// a tools/call gets the same words as a JSON-RPC error (-32600, as muster
// refuses a too-large request), with the request's id — never -32700.
func TestAnOldMustersUnreadReplyToAnotherMethodIsAnInvalidRequest(t *testing.T) {
	p, host := oldMusterProxy(t, oldMuster)
	w := postBody(t, p, oldMusterHdr, sizedCall(t, "tools/list", 70001))
	var rep struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil || rep.Error == nil {
		t.Fatalf("%d %.300s", w.Code, w.Body)
	}
	if rep.Error.Code != -32600 || string(rep.ID) != "5" ||
		!strings.HasPrefix(rep.Error.Message, "muster at "+host+" could not read this MCP request of 70001 bytes: it reads at most 64 KiB per request") {
		t.Fatalf("got %+v id %s", rep.Error, rep.ID)
	}
}

// TestOnlyAnOldMustersExactUnreadReplyIsRewritten: everything else is relayed
// byte for byte — a body the old server read, the same reply to a request it
// could have read (another cause), a current muster's own -32700 (which names
// muster), and a streamed answer (keep-alive whitespace first) to a large call.
func TestOnlyAnOldMustersExactUnreadReplyIsRewritten(t *testing.T) {
	current := `{"error":{"code":-32700,"message":"muster could not read this MCP request: unexpected EOF"},"id":null,"jsonrpc":"2.0"}` + "\n"
	otherCode := `{"error":{"code":-32603,"message":"could not read the request"},"id":null,"jsonrpc":"2.0"}` + "\n"
	streamed := "\n\n" + `{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"{\"page_id\":3,\"version\":4}"}]}}` + "\n"
	for _, c := range []struct {
		name   string
		h      http.HandlerFunc
		method string
		n      int
		want   string
		status int // 0: 200
	}{
		{"the legacy reply under a failure status", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, legacyUnreadReply)
		}, "tools/call", 400000, legacyUnreadReply, http.StatusServiceUnavailable},
		{"read by the old server", oldMuster, "tools/call", 60001,
			`{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"{\"page_id\":3,\"version\":2}"}]}}` + "\n", 0},
		{"the legacy reply to a small request", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, legacyUnreadReply)
		}, "tools/call", 3001, legacyUnreadReply, 0},
		{"a current muster's -32700", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, current)
		}, "tools/call", 400000, current, 0},
		{"the legacy words under another code", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, otherCode)
		}, "tools/call", 400000, otherCode, 0},
		{"a streamed answer", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "\n\n")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, streamed[2:])
		}, "tools/call", 400000, streamed, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _ := oldMusterProxy(t, c.h)
			w := postBody(t, p, oldMusterHdr, sizedCall(t, c.method, c.n))
			status := c.status
			if status == 0 {
				status = http.StatusOK
			}
			if w.Code != status || w.Body.String() != c.want {
				t.Fatalf("%d\n got %q\nwant %q", w.Code, w.Body.String(), c.want)
			}
		})
	}
}

// TestALargeCallsKeepAliveStillReachesTheCallerAtOnce: muster keeps a long
// call open with whitespace, and the caller must see it while the call runs
// (relayStream). Looking for the old reply must not hold that back: the
// upstream here sends its keep-alive and then waits until the caller has read
// it, so a proxy that buffered the answer first never delivers it in time.
func TestALargeCallsKeepAliveStillReachesTheCallerAtOnce(t *testing.T) {
	seen := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "\n")
		w.(http.Flusher).Flush()
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":5,"result":{"content":[]}}`+"\n")
	}))
	t.Cleanup(up.Close)
	front := httptest.NewServer(proxyFor(up.URL))
	t.Cleanup(front.Close)
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/mcp", strings.NewReader(sizedCall(t, "tools/call", 400000)))
	for k, v := range oldMusterHdr {
		req.Header.Set(k, v)
	}
	// The request and the first read both run against the clock: a proxy
	// that held the answer back holds back its headers too.
	got := make(chan byte, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		b := make([]byte, 1)
		if n, _ := resp.Body.Read(b); n == 1 {
			got <- b[0]
		}
	}()
	select {
	case b := <-got:
		close(seen)
		if b != '\n' {
			t.Fatalf("the first byte is %q, want the keep-alive", b)
		}
	case <-time.After(2 * time.Second):
		close(seen)
		t.Fatal("the keep-alive was held back while the call ran")
	}
}
