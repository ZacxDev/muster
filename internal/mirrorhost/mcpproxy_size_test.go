package mirrorhost

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// htmlEscaped is json.Marshal's escape of <.
const htmlEscaped = "\\" + "u003c"

// pageCall is a page_create of about size bytes of blocks: one html block of
// ASCII base64-shaped text per 200 KiB (a block holds at most 256 KiB).
func pageCall(size int) string {
	var blocks []map[string]string
	for size > 0 {
		n := min(size, 200<<10)
		blocks = append(blocks, map[string]string{"type": "html", "html": `<img src="data:image/webp;base64,` + strings.Repeat("QUJD", n/4) + `">`})
		size -= n
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": "page_create", "arguments": map[string]any{"title": "Screens", "blocks": blocks}}})
	return string(b)
}

// TestAPageOfScreenshotsIsForwardedWhole: a ~450 KiB page_create — under the
// documented 512 KiB page — is forwarded to muster byte for byte (origin/main
// refused it here, 413, at a flat 64 KiB). One past the request bound is
// refused 413 HERE, in words naming this side, and is not forwarded.
func TestAPageOfScreenshotsIsForwardedWhole(t *testing.T) {
	up, srv := newUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":9,"result":{"content":[]}}`)
	})
	p := proxyFor(srv.URL)
	hdr := map[string]string{"Authorization": "Bearer " + proxySecret, MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX}

	body := pageCall(450 << 10)
	if len(body) < 450<<10 || len(body) > 470<<10 {
		t.Fatalf("fixture: %d bytes", len(body))
	}
	if w := postBody(t, p, hdr, body); w.Code != http.StatusOK {
		t.Fatalf("a 450 KiB page: %d %.300s", w.Code, w.Body)
	}
	if up.n() != 1 || up.body[0] != body {
		t.Fatalf("forwarded %d call(s); the body arrived whole: %v", up.n(), up.n() == 1 && up.body[0] == body)
	}

	huge := pageCall(1100 << 10)
	w := postBody(t, p, hdr, huge)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "muster-mirror on this host refused this MCP request") {
		t.Fatalf("past the bound: %d %.300s", w.Code, w.Body)
	}
	if up.n() != 1 {
		t.Fatalf("a body past the bound was forwarded (%d calls)", up.n())
	}
}

// TestARebuiltCallIsNeverLargerThanItsRequest: a tools/call the proxy takes
// apart and writes again (an instance argument removed, mcpCall.body) keeps
// its markup as sent — json.Marshal would write every < > & as <, six
// bytes for one, and push a page that fitted past muster's bound.
func TestARebuiltCallIsNeverLargerThanItsRequest(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"page_create","arguments":{"instance":"primary","title":"T & <b>","blocks":[{"type":"html","html":"` +
		strings.Repeat("<p>&amp;</p>", 2000) + `"}]}}}`
	c, ok := parseCall([]byte(in))
	if !ok {
		t.Fatal("parseCall")
	}
	if inst, bad := c.takeInstance(); bad || inst != "primary" {
		t.Fatalf("takeInstance: %q %v", inst, bad)
	}
	out := c.body()
	if len(out) >= len(in) || strings.Contains(string(out), htmlEscaped) || strings.Contains(string(out), `"instance"`) {
		t.Fatalf("rebuilt %d bytes from %d (escaped: %v)", len(out), len(in), strings.Contains(string(out), htmlEscaped))
	}
	var got, want map[string]any
	if json.Unmarshal(out, &got) != nil || json.Unmarshal([]byte(strings.Replace(in, `"instance":"primary",`, "", 1)), &want) != nil {
		t.Fatalf("not JSON: %.200s", out)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatal("the rebuilt call says something else")
	}
}
