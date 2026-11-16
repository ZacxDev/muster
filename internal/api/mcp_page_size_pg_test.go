package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/notifypref"
	"github.com/ZacxDev/muster/internal/pages"
)

// A PAGE OF SCREENSHOTS OVER MCP, EVERY DOOR AND EVERY HOP. page_create
// documents a page of up to pages.MaxVersionBytes with its images inline as
// data: URIs; until the MCP request bound was derived from that limit
// (mirror.MaxMCPRequestBytes) every MCP request was capped at 64 KiB, by
// muster and by the host proxy alike, so a page with a couple of screenshots
// could not be published at all (measured 2026-11-16: ~440 KB, HTTP 413 from
// a mirrored session's local proxy). Every name and image here is invented.

// screenshotBlocks is a page of n html blocks, each one inline base64 "WebP"
// of per bytes (random, so it does not compress into anything smaller), as a
// mirrored session writes it: its blocks, as JSON, are returned with it.
func screenshotBlocks(seed int64, n, per int) (json.RawMessage, int) {
	rng := rand.New(rand.NewSource(seed))
	blocks := make([]map[string]string, 0, n)
	for i := 0; i < n; i++ {
		img := make([]byte, per)
		_, _ = rng.Read(img)
		blocks = append(blocks, map[string]string{"type": "html",
			"html": `<figure><img alt="screen ` + string(rune('a'+i)) + `" src="data:image/webp;base64,` + base64.StdEncoding.EncodeToString(img) + `"></figure>`})
	}
	raw, _ := json.Marshal(blocks)
	return raw, len(raw)
}

func pageCreateBody(blocks json.RawMessage) string {
	return mcpToolCall(pageCreateTool, map[string]any{"title": "Ten screenshots", "blocks": blocks})
}

// sizeFixture is the mirror fixture (host ws's session focusSID) with the page
// store on, and the real host proxy in front of a real muster listener.
type sizeFixture struct {
	*attnFixture
	ws    string
	proxy *mirrorhost.MCPProxy
}

const sizeProxySecret = "size-local-secret-0123456789abcdef0123456789"

// sizeWindows is the host proxy's tmux view: pane %7 runs focusSID.
type sizeWindows struct{}

func (sizeWindows) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return []mirrorhost.WindowInfo{{Session: "main", ID: "@1", Pane: "%7", Claude: &mirrorhost.ClaudePane{Pane: "%7", Gen: focusSID}}}, nil
}

type sizeSocket struct{}

func (sizeSocket) ServerSocket(context.Context) (string, error) {
	return "/run/user/1000/tmux-1000/default", nil
}

func newSizeFixture(t *testing.T) *sizeFixture {
	t.Helper()
	f, ws, _, _ := newMirrorMCPFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ext.Pages = pages.NewPG(pool)
		ext.NotifyPrefs = notifypref.NewPG(pool)
		ext.PublicURL = pagesPublicURL
	})
	t.Cleanup(func() {
		f.srv.WaitForPushes()
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM pages WHERE owner_id = 1`)
	})
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	p := &mirrorhost.MCPProxy{URL: ts.URL, Token: ws, AllowHTTP: true, Secret: sizeProxySecret, Windows: sizeWindows{}, Socket: sizeSocket{}}
	return &sizeFixture{attnFixture: f, ws: ws, proxy: p}
}

// viaProxy posts body to the host proxy as Claude Code in pane %7 does.
func (f *sizeFixture) viaProxy(t *testing.T, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+sizeProxySecret)
	r.Header.Set(mirrorhost.MCPPaneHeader, "%7")
	r.Header.Set(mirrorhost.MCPTmuxHeader, "/run/user/1000/tmux-1000/default,4242,0")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.proxy.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Body)
	return w.Code, strings.TrimSpace(string(b))
}

// replyText is a tools/call reply's text and isError, from its raw body.
func replyText(t *testing.T, body string) (string, bool) {
	t.Helper()
	var r mcpReply
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("not JSON-RPC: %.300s", body)
	}
	if r.Error != nil {
		t.Fatalf("a JSON-RPC error, not a tool result: %+v", r.Error)
	}
	return toolTextRaw(t, &r)
}

// createdPage is the page a successful page_create's text names, read back
// from the store: its id, and its stored version's size.
func (f *sizeFixture) createdPage(t *testing.T, text string) (int64, int) {
	t.Helper()
	var out struct {
		PageID int64 `json:"page_id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.PageID <= 0 {
		t.Fatalf("page_create answered %.300s", text)
	}
	var size int
	if err := f.pool.QueryRow(authz.System(context.Background()),
		`SELECT size_bytes FROM page_versions WHERE page_id = $1`, out.PageID).Scan(&size); err != nil {
		t.Fatalf("page %d's version: %v", out.PageID, err)
	}
	return out.PageID, size
}

// TestAPageOfScreenshotsIsCreatedOverEveryMCPDoor: a ~450 KiB page — ten
// inline screenshots, well under the documented 512 KiB — is created through
// the agent door, the mirror door, and a mirrored session's REAL host proxy in
// front of a real muster listener. On origin/main muster read none of it
// ("could not read the request") and the proxy refused it 413.
func TestAPageOfScreenshotsIsCreatedOverEveryMCPDoor(t *testing.T) {
	f := newSizeFixture(t)
	blocks, size := screenshotBlocks(1, 10, 34<<10)
	if size < 440<<10 || size > 496<<10 {
		t.Fatalf("fixture: the blocks are %d bytes, want ~450 KiB", size)
	}
	body := pageCreateBody(blocks)

	// Each door is its own subtest, so each one's verdict is reported.
	t.Run("agent door", func(t *testing.T) { // POST /mcp
		r, code := f.mcp(t, f.opTok, body, nil)
		if code != http.StatusOK || r == nil || r.Error != nil {
			t.Fatalf("%d %+v", code, r)
		}
		text, isErr := toolTextRaw(t, r)
		if isErr {
			t.Fatalf("refused: %.300s", text)
		}
		f.createdPage(t, text)
	})
	t.Run("mirror door", func(t *testing.T) { // POST /mirror/mcp, directly
		code, mr, raw := f.mirrorRPC(t, mirrorAs(f.ws, focusSID), body)
		if code != http.StatusOK || mr == nil || mr.Error != nil {
			t.Fatalf("%d %.300s", code, raw)
		}
		text, isErr := toolTextRaw(t, mr)
		if isErr {
			t.Fatalf("refused: %.300s", text)
		}
		f.createdPage(t, text)
	})
	t.Run("host proxy", func(t *testing.T) { // Claude Code -> its host's proxy -> muster over HTTP
		code, raw := f.viaProxy(t, body)
		if code != http.StatusOK {
			t.Fatalf("%d %.300s", code, raw)
		}
		text, isErr := replyText(t, raw)
		if isErr {
			t.Fatalf("refused: %.300s", text)
		}
		if _, stored := f.createdPage(t, text); stored < 440<<10 {
			t.Fatalf("the stored version is %d bytes: the page was cut on the way", stored)
		}
	})
}

// TestAPageOverTheLimitIsRefusedByThePageToolNotTheTransport: a page past
// 512 KiB (but inside what a request may carry) reaches page_create and gets
// ITS refusal, through the whole proxy path; one past the request bound is
// refused 413 by whichever hop read it, in words naming that hop.
func TestAPageOverTheLimitIsRefusedByThePageToolNotTheTransport(t *testing.T) {
	f := newSizeFixture(t)
	over, size := screenshotBlocks(2, 12, 46<<10)
	if size <= 512<<10 || size >= 1<<20 {
		t.Fatalf("fixture: %d bytes is not between the page limit and the request bound", size)
	}
	code, raw := f.viaProxy(t, pageCreateBody(over))
	if code != http.StatusOK {
		t.Fatalf("an over-limit page: %d %.300s, want the page tool's own refusal", code, raw)
	}
	if text, isErr := replyText(t, raw); !isErr || !strings.Contains(text, "a version holds at most 512 KiB") {
		t.Fatalf("an over-limit page: %v %.300s", isErr, text)
	}
	code, mr, raw := f.mirrorRPC(t, mirrorAs(f.ws, focusSID), pageCreateBody(over))
	if text, isErr := toolTextRaw(t, mr); code != http.StatusOK || !isErr || !strings.Contains(text, "a version holds at most 512 KiB") {
		t.Fatalf("an over-limit page, mirror door: %d %v %.300s", code, isErr, raw)
	}

	// Past the request bound: refused unread, 413, by the hop that read it.
	huge, _ := screenshotBlocks(3, 30, 40<<10)
	code, raw = f.viaProxy(t, pageCreateBody(huge))
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(raw, "muster-mirror on this host refused this MCP request") ||
		!strings.Contains(raw, "1088 KiB") {
		t.Fatalf("a body past the bound, at the proxy: %d %.300s", code, raw)
	}
	w := f.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(pageCreateBody(huge)),
		map[string]string{"Authorization": "Bearer " + f.ws, mirrorSessionHeader: focusSID, "Content-Type": "application/json"})
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `"muster refused this MCP request`) {
		t.Fatalf("a body past the bound, at muster: %d %.300s", w.Code, w.Body)
	}
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM pages WHERE owner_id = 1`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d pages were created by refused calls (%v)", n, err)
	}
}

// TestEveryOtherMCPRequestKeepsTheTightBound: the read bound is a page
// writer's; any other tool past 64 KiB is refused as a tool error before it
// runs, and any other method past it as a JSON-RPC error — through the proxy
// too. The control: the same call under 64 KiB is not refused for its size.
func TestEveryOtherMCPRequestKeepsTheTightBound(t *testing.T) {
	f := newSizeFixture(t)
	get := func(pad int) string {
		return mcpToolCall(pageGetTool, map[string]any{"page_id": 424242, "pad": strings.Repeat("x", pad)})
	}
	const bound = "muster takes at most 64 KiB for it"
	code, raw := f.viaProxy(t, get(80<<10))
	if text, isErr := replyText(t, raw); code != http.StatusOK || !isErr || !strings.Contains(text, "this page_get call is 80 KiB; "+bound) {
		t.Fatalf("page_get at 80 KiB: %d %.300s", code, text)
	}
	code, mr, raw := f.mirrorRPC(t, mirrorAs(f.ws, focusSID), get(80<<10))
	if text, isErr := toolTextRaw(t, mr); code != http.StatusOK || !isErr || !strings.Contains(text, bound) {
		t.Fatalf("page_get at 80 KiB, mirror door: %d %.300s", code, raw)
	}
	r, _ := f.mcp(t, f.opTok, get(80<<10), nil)
	if text, isErr := toolTextRaw(t, r); !isErr || !strings.Contains(text, bound) {
		t.Fatalf("page_get at 80 KiB, agent door: %.300s", text)
	}
	// The control: under the bound it runs, and answers as page_get does.
	code, raw = f.viaProxy(t, get(60<<10))
	if text, isErr := replyText(t, raw); code != http.StatusOK || !isErr || strings.Contains(text, bound) || !strings.Contains(text, "no such page: 424242") {
		t.Fatalf("page_get at 60 KiB: %d %.300s", code, text)
	}
	// Another method past 64 KiB: a JSON-RPC error naming the bound.
	list := `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"pad":"` + strings.Repeat("x", 80<<10) + `"}}`
	code, mr, raw = f.mirrorRPC(t, mirrorAs(f.ws, focusSID), list)
	if code != http.StatusOK || mr == nil || mr.Error == nil || !strings.Contains(mr.Error.Message, "only a tools/call of page_create or page_update may be larger than 64 KiB") {
		t.Fatalf("tools/list at 80 KiB: %d %.300s", code, raw)
	}
}
