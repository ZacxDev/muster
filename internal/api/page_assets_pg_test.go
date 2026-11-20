package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

// A PAGE'S AUTHOR ASSETS through the real server against a real Postgres
// (page_assets.go; internal/pages asset.go): a mirror host's upload route, its
// two caps and its type rule, each refusal by its own guard; who reads an
// asset (the owner's view of a version that uses it, a live link of its page
// at the current version, nobody else), cached by the browser, a link's reads
// capped; the lifecycle (an old version keeps its asset; deleting the page
// deletes them); and a dispatched agent, which cannot attach one yet. The
// page's owner is user 1, whose host "ws" lists the session focusSID (the
// mirror fixture). Every name is invented.

// assetMP4 is the head of an MP4 file as http.DetectContentType knows one: a
// 24-byte ftyp box whose major brand is mp42.
var assetMP4 = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom-frames")

// sized is head padded with zeros to n bytes (the type is read from the head).
func sized(head []byte, n int) []byte {
	b := make([]byte, n)
	copy(b, head)
	return b
}

// countingBody counts what the handler read of a request body.
type countingBody struct {
	r io.Reader
	n int
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
func (c *countingBody) Close() error { return nil }

// wsCreator is a page made from host ws (what its mirror token proves).
var wsCreator = pages.Creator{MirrorHost: "ws", Label: "ws · build"}

// assetWorld is the mirror fixture with pages on and an upload store that
// takes 25 MiB files (the asset cap, 15 MiB, is then the tighter one).
type assetWorld struct {
	*attnFixture
	ws, members string
	store       *pages.PGStore
	objects     *uploadstest.Objects
	// session is who a session cookie acts as (0: the operator, user 1).
	session int64
}

func newAssetWorld(t *testing.T) *assetWorld {
	t.Helper()
	w := &assetWorld{objects: &uploadstest.Objects{}}
	f, ws, _, members := newMirrorMCPFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		sg, err := uploads.NewSigner("page-asset-world-signing-key-0123456789abcdef")
		if err != nil {
			t.Fatal(err)
		}
		ext.Uploads = &uploads.Service{Store: uploads.NewPG(pool), Objects: w.objects, Signer: sg, PublicURL: "https://muster.example.test",
			MaxBytes: 25 << 20, LinkTTL: time.Hour}
		w.store = pages.NewPG(pool)
		ext.Pages = w.store
	})
	w.attnFixture, w.ws, w.members = f, ws, members
	f.srv.sessionUserOverride = func(*http.Request) int64 {
		if w.session > 0 {
			return w.session
		}
		return 1
	}
	return w
}

// page makes a page of user 1's as c, removed again when the test ends.
func (w *assetWorld) page(t *testing.T, c pages.Creator, title string) int64 {
	t.Helper()
	b, n, err := pages.Parse([]byte(`[{"type":"markdown","text":"soon"}]`))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := w.store.Create(asUser(1), c, title, b, n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = w.pool.Exec(sys, `DELETE FROM uploads WHERE owner_id = 1 AND object_key LIKE $1`, "pages/"+strconv.FormatInt(p.ID, 10)+"/%")
		_, _ = w.pool.Exec(sys, `DELETE FROM pages WHERE id = $1`, p.ID)
	})
	return p.ID
}

// post sends data to page id as host tok's muster-mirror does, typed ct;
// chunked sends it with no Content-Length (the length unknown until it ends).
// The status, the answer and how many bytes of the body the server read.
func (w *assetWorld) post(t *testing.T, tok string, id int64, name, ct string, data []byte, chunked bool) (int, map[string]any, int) {
	t.Helper()
	body := &countingBody{r: bytes.NewReader(data)}
	r := httptest.NewRequest(http.MethodPost, "/mirror/pages/"+strconv.FormatInt(id, 10)+"/assets?filename="+name, nil)
	r.Body = body
	r.ContentLength = int64(len(data))
	if chunked {
		r.ContentLength = -1
	}
	for k, v := range mirrorAs(tok, focusSID) {
		r.Header.Set(k, v)
	}
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, body.n
}

func (w *assetWorld) assetRows(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM page_assets WHERE page_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// objectsUnder counts the fake bucket's objects under page id's prefix.
func (w *assetWorld) objectsUnder(id int64) int {
	n := 0
	for _, k := range w.objects.Keys() {
		if strings.HasPrefix(k, "pages/"+strconv.FormatInt(id, 10)+"/") {
			n++
		}
	}
	return n
}

// tool calls a page tool as host ws's session focusSID.
func (w *assetWorld) tool(t *testing.T, name string, args any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(args)
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaMirrorHost})
	text, ok := w.srv.dispatchCallerTool(ctx, mirrorCaller("ws", focusSID), name, string(b))
	if !ok {
		t.Fatalf("%s was not dispatched", name)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s answered %q", name, text)
	}
	return out
}

// get is a GET with hdr.
func (w *assetWorld) get(t *testing.T, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return w.call(t, http.MethodGet, path, nil, hdr)
}

// TestAnAssetOverItsCapIsRefusedByItsOwnGuard: a 16 MiB file (15 MiB + 1)
// onto an EMPTY page — so the page's 100 MiB has room for it — is refused by
// the asset's own cap: 413 asset_too_large, before a byte is read when its
// length is known, and while it streams when it is not; nothing is stored.
// Control: 2 MiB is taken.
func TestAnAssetOverItsCapIsRefusedByItsOwnGuard(t *testing.T) {
	w := newAssetWorld(t)
	id := w.page(t, wsCreator, "Reels")
	big := sized(assetMP4, 16<<20)
	for _, chunked := range []bool{false, true} {
		code, out, read := w.post(t, w.ws, id, "reel.mp4", "video/mp4", big, chunked)
		if code != http.StatusRequestEntityTooLarge || out["code"] != pageAssetTooLargeCode || out["error"] != "the file is over 15 MiB, the most one page asset may be" {
			t.Fatalf("16 MiB (chunked %v): %d %v", chunked, code, out)
		}
		// Known length: refused before a byte is read (the length check).
		// Unknown: refused by the cap held while it streams, once past it.
		if (!chunked && read != 0) || (chunked && read <= int(pages.MaxAssetBytes)) {
			t.Fatalf("16 MiB (chunked %v) was refused after reading %d bytes", chunked, read)
		}
	}
	if n, o := w.assetRows(t, id), w.objectsUnder(id); n != 0 || o != 0 {
		t.Fatalf("a refused asset left %d row(s) and %d object(s)", n, o)
	}
	code, out, _ := w.post(t, w.ws, id, "reel.mp4", "video/mp4", sized(assetMP4, 2<<20), false)
	if code != http.StatusCreated || out["type"] != "video/mp4" || pnum(out["size"]) != 2<<20 || !strings.HasPrefix(out["ref"].(string), "asset:") {
		t.Fatalf("control: 2 MiB: %d %v", code, out)
	}
}

// TestAPageOverItsTotalIsRefusedByItsOwnGuard: a page already holding 90 MiB
// (six 15 MiB assets) refuses an 11 MiB file — under the asset's own cap, so
// only the page's total can refuse it — with 413 page_assets_full, known
// length or streamed; nothing is stored. Control: a 9 MiB file fits.
func TestAPageOverItsTotalIsRefusedByItsOwnGuard(t *testing.T) {
	w := newAssetWorld(t)
	id := w.page(t, wsCreator, "Reels")
	for i := 0; i < 6; i++ {
		pid, _ := uploads.NewPublicID()
		if _, err := w.store.AddAsset(asUser(1), wsCreator, id, pages.Asset{PublicID: pid, ContentType: "video/mp4", Size: pages.MaxAssetBytes}); err != nil {
			t.Fatal(err)
		}
	}
	eleven := sized(assetMP4, 11<<20)
	for _, chunked := range []bool{false, true} {
		code, out, read := w.post(t, w.ws, id, "reel.mp4", "video/mp4", eleven, chunked)
		if code != http.StatusRequestEntityTooLarge || out["code"] != pageAssetsFullCode ||
			out["error"] != "this page's assets are limited to 100 MiB in all; it has room for 10.0 MB more" {
			t.Fatalf("11 MiB onto 90 (chunked %v): %d %v", chunked, code, out)
		}
		// Known length: refused before a byte is read (the room check).
		// Unknown: by the room cap held while it streams, past 10 MiB.
		if (!chunked && read != 0) || (chunked && read <= 10<<20) {
			t.Fatalf("11 MiB onto 90 (chunked %v) was refused after reading %d bytes", chunked, read)
		}
	}
	if n, o := w.assetRows(t, id), w.objectsUnder(id); n != 6 || o != 0 {
		t.Fatalf("a refused asset left %d row(s) (want the 6 seeded) and %d object(s)", n, o)
	}
	if code, out, _ := w.post(t, w.ws, id, "reel.mp4", "video/mp4", sized(assetMP4, 9<<20), false); code != http.StatusCreated {
		t.Fatalf("control: 9 MiB onto 90: %d %v", code, out)
	}
}

// TestAnAssetIsTheTypeItsBytesAre: a type that is not one of the six — named,
// or only in the bytes — and a declared type the bytes contradict are refused
// (415), and nothing is stored; the six, declared rightly or not at all, are
// taken.
func TestAnAssetIsTheTypeItsBytesAre(t *testing.T) {
	w := newAssetWorld(t)
	id := w.page(t, wsCreator, "Types")
	html := []byte("<!DOCTYPE html><html><script>alert(1)</script></html>")
	for _, c := range []struct {
		name, ct string
		data     []byte
		want     string
	}{
		{"page.png", "image/png", html, "the file is text/html; charset=utf-8; a page asset is PNG, JPEG, WebP, GIF, MP4 or WebM"},
		{"clip.png", "image/png", assetMP4, "the file was sent as image/png, but its bytes are video/mp4"},
		{"page.html", "text/html", html, "the file was sent as text/html; a page asset is PNG, JPEG, WebP, GIF, MP4 or WebM"},
		{"doc.bin", "", []byte("%PDF-1.7\n"), "the file is application/pdf; a page asset is PNG, JPEG, WebP, GIF, MP4 or WebM"},
	} {
		code, out, _ := w.post(t, w.ws, id, c.name, c.ct, c.data, false)
		if code != http.StatusUnsupportedMediaType || out["code"] != "unsupported_media_type" || out["error"] != c.want {
			t.Errorf("%s as %q: %d %v, want 415 %q", c.name, c.ct, code, out, c.want)
		}
	}
	if n, o := w.assetRows(t, id), w.objectsUnder(id); n != 0 || o != 0 {
		t.Fatalf("refused files left %d row(s) and %d object(s)", n, o)
	}
	for _, c := range []struct{ name, ct string }{{"shot.png", "image/png"}, {"shot2.png", "application/octet-stream"}, {"shot3.png", ""}} {
		if code, out, _ := w.post(t, w.ws, id, c.name, c.ct, askMediaPNG, false); code != http.StatusCreated || out["type"] != "image/png" {
			t.Errorf("control: a PNG as %q: %d %v", c.ct, code, out)
		}
	}
	if code, out, _ := w.post(t, w.ws, id, "clip.mp4", "video/mp4", assetMP4, false); code != http.StatusCreated {
		t.Errorf("control: an MP4: %d %v", code, out)
	}
}

// TestOnlyThePagesAuthorAddsAnAsset: host ws's token cannot add to a page of
// the same owner that ANOTHER creator made (an agent's) — 404 for the page —
// and another owner's host of the same name is refused; nothing is stored.
// Control: the same token adds to a page ws made.
func TestOnlyThePagesAuthorAddsAnAsset(t *testing.T) {
	w := newAssetWorld(t)
	agents := w.page(t, pages.Creator{AgentID: w.op.ID, Label: w.op.Name}, "An agent's")
	mine := w.page(t, wsCreator, "Mine")
	if code, out, _ := w.post(t, w.ws, agents, "shot.png", "image/png", askMediaPNG, false); code != http.StatusNotFound || out["code"] != "not_found" {
		t.Errorf("host ws added to an agent's page of the same owner: %d %v", code, out)
	}
	if code, out, _ := w.post(t, w.members, mine, "shot.png", "image/png", askMediaPNG, false); code < 400 {
		t.Errorf("another owner's host added to the page: %d %v", code, out)
	}
	for _, id := range []int64{agents, mine} {
		if n, o := w.assetRows(t, id), w.objectsUnder(id); n != 0 || o != 0 {
			t.Fatalf("refused uploads left %d row(s) and %d object(s) on page %d", n, o, id)
		}
	}
	if code, out, _ := w.post(t, w.ws, mine, "shot.png", "image/png", askMediaPNG, false); code != http.StatusCreated {
		t.Fatalf("control: ws adds to its own page: %d %v", code, out)
	}
}

// publishWith uploads one PNG and one MP4 to page id and publishes a version
// using both; their ids and the version.
func (w *assetWorld) publishWith(t *testing.T, id int64) (img, vid string, version int) {
	t.Helper()
	code, out, _ := w.post(t, w.ws, id, "shot.png", "image/png", askMediaPNG, false)
	if code != http.StatusCreated {
		t.Fatalf("upload the image: %d %v", code, out)
	}
	img = out["asset_id"].(string)
	code, out, _ = w.post(t, w.ws, id, "clip.mp4", "video/mp4", assetMP4, false)
	if code != http.StatusCreated {
		t.Fatalf("upload the video: %d %v", code, out)
	}
	vid = out["asset_id"].(string)
	blocks, _ := json.Marshal([]map[string]any{
		{"type": "markdown", "text": "Look: ![the shot](asset:" + img + ")"},
		{"type": "html", "html": `<video id="v" src="asset:` + vid + `" controls></video>`},
	})
	up := w.tool(t, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(blocks)})
	if up["error"] != nil || pnum(up["version"]) < 2 {
		t.Fatalf("page_update with the assets: %v", up)
	}
	return img, vid, int(pnum(up["version"]))
}

func (w *assetWorld) liveLink(t *testing.T, id int64) pages.Link {
	t.Helper()
	l, err := w.store.CreateLink(asUser(1), id, pages.LinkLive, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestAnAssetIsReadByWhoCanReadItsPageAndNoOneElse: the owner's view of the
// version that uses it, and a live link of its page, read it — inert, the
// same bytes, and cacheable (an id's bytes never change); the content names it
// for the SDK and draws the markdown image (its sandbox unchanged). Another
// owner's session, a link of ANOTHER page of the same owner, the owner's view
// of a version that does not use it, and a revoked link read nothing.
func TestAnAssetIsReadByWhoCanReadItsPageAndNoOneElse(t *testing.T) {
	w := newAssetWorld(t)
	id := w.page(t, wsCreator, "Review")
	other := w.page(t, wsCreator, "Other")
	img, vid, v := w.publishWith(t, id)
	sess := map[string]string{"Cookie": sessionCookieFor(w.srv)}
	vs := strconv.Itoa(v)
	pid := strconv.FormatInt(id, 10)

	// The content: both ids for the SDK, the markdown image as an <img>, and
	// the sandbox exactly as it was.
	cw := w.get(t, "/pages/"+pid+"/versions/"+vs+"/content", merge(sess, map[string]string{"Sec-Fetch-Dest": "iframe"}))
	assertContentHeaders(t, cw, "the owner's content")
	if body := cw.Body.String(); !strings.Contains(body, `ASSETS=["`+img+`","`+vid+`"]`) && !strings.Contains(body, `ASSETS=["`+vid+`","`+img+`"]`) {
		t.Errorf("the content does not name its two assets for the SDK: %s", body)
	} else if blocks := pageContentBlocks(t, body); !strings.Contains(blocks, `<img src="asset:`+img+`" alt="the shot">`) || !strings.Contains(blocks, `<video id="v" src="asset:`+vid+`" controls></video>`) {
		t.Errorf("the blocks do not draw the two assets: %s", blocks)
	}

	// The owner's view of the version: both, inert, the stored bytes, kept by
	// the browser.
	for _, c := range []struct {
		id, ct string
		data   []byte
	}{{img, "image/png", askMediaPNG}, {vid, "video/mp4", assetMP4}} {
		r := w.get(t, "/pages/"+pid+"/versions/"+vs+"/assets/"+c.id, sess)
		if r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), c.data) || r.Header().Get("Content-Type") != c.ct ||
			r.Header().Get("X-Content-Type-Options") != "nosniff" || r.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" ||
			!strings.Contains(r.Header().Get("Content-Security-Policy"), "sandbox") || r.Header().Get("Cache-Control") != "private, max-age=31536000, immutable" {
			t.Errorf("the owner reads %s: %d %v", c.ct, r.Code, r.Header())
		}
	}
	// Version 1 does not use them.
	if r := w.get(t, "/pages/"+pid+"/versions/1/assets/"+img, sess); r.Code != http.StatusNotFound {
		t.Errorf("the owner's view of version 1 reads an asset it does not use: %d", r.Code)
	}
	// Another owner's session: nothing.
	w.session = w.member
	if r := w.get(t, "/pages/"+pid+"/versions/"+vs+"/assets/"+img, sess); r.Code != http.StatusNotFound {
		t.Errorf("another owner read the asset: %d %s", r.Code, r.Body.String())
	}
	w.session = 0

	// A live link of the page reads both; a link of the OTHER page reads
	// neither (its page's version does not use them).
	l := w.liveLink(t, id)
	lo := w.liveLink(t, other)
	for _, a := range []string{img, vid} {
		if r := w.get(t, pages.LinkPath(l.Token)+"/assets/"+a, nil); r.Code != http.StatusOK || r.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" ||
			r.Header().Get("Cache-Control") != "private, max-age=31536000, immutable" || len(r.Header().Values("Set-Cookie")) != 0 {
			t.Errorf("the page's link reads %s: %d %v", a, r.Code, r.Header())
		}
		if r := w.get(t, pages.LinkPath(lo.Token)+"/assets/"+a, nil); r.Code != http.StatusNotFound || r.Body.String() != pageLinkGone {
			t.Errorf("another page's link read %s: %d %s", a, r.Code, r.Body.String())
		}
	}
	// Revoked: nothing.
	if _, err := w.store.RevokeLink(asUser(1), id, l.ID); err != nil {
		t.Fatal(err)
	}
	if r := w.get(t, pages.LinkPath(l.Token)+"/assets/"+img, nil); r.Code != http.StatusNotFound || r.Body.String() != pageLinkGone {
		t.Errorf("a revoked link read the asset: %d %s", r.Code, r.Body.String())
	}
}

// readAs is a share link's asset read from client address addr.
func (w *assetWorld) readAs(t *testing.T, path, addr string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = addr + ":40000"
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

// throttled reports whether r is the asset throttle's refusal saying msg:
// 429, those words, Retry-After, uncacheable.
func throttled(r *httptest.ResponseRecorder, msg string) bool {
	return r.Code == http.StatusTooManyRequests && strings.TrimSpace(r.Body.String()) == msg && r.Header().Get("Retry-After") == "60" &&
		r.Header().Get("Cache-Control") == "no-store"
}

// TestOneViewerOfALinkDoesNotThrottleAnother: the read cap is per link AND
// client address. With it lowered to 3 (and the byte budget left at its
// 2 GiB), one viewer's 4th read is refused with the read cap's own words,
// while a SECOND viewer of the same link still reads, as does the first
// viewer through another link of the page.
func TestOneViewerOfALinkDoesNotThrottleAnother(t *testing.T) {
	w := newAssetWorld(t)
	defer func(n int) { pageAssetLimit = n }(pageAssetLimit)
	pageAssetLimit = 3
	id := w.page(t, wsCreator, "Throttle")
	img, _, _ := w.publishWith(t, id)
	l, l2 := w.liveLink(t, id), w.liveLink(t, id)
	path := pages.LinkPath(l.Token) + "/assets/" + img
	for i := 0; i < 3; i++ {
		if r := w.readAs(t, path, "198.51.100.7"); r.Code != http.StatusOK {
			t.Fatalf("viewer A's read %d under the cap: %d", i+1, r.Code)
		}
	}
	if r := w.readAs(t, path, "198.51.100.7"); !throttled(r, pageAssetThrottled) {
		t.Fatalf("viewer A's 4th read past a cap of 3: %d %q %v", r.Code, r.Body.String(), r.Header())
	}
	if r := w.readAs(t, path, "198.51.100.8"); r.Code != http.StatusOK {
		t.Fatalf("viewer B was throttled by viewer A's reads: %d %q", r.Code, r.Body.String())
	}
	if r := w.readAs(t, pages.LinkPath(l2.Token)+"/assets/"+img, "198.51.100.7"); r.Code != http.StatusOK {
		t.Fatalf("viewer A through another link of the page: %d", r.Code)
	}
}

// TestALinksAssetBytesAreBudgeted: every viewer of a link together may draw
// at most the link's byte budget a minute. With it lowered to 100 bytes and
// the read cap left at 600, three viewers — each one read, so no viewer's cap
// is near — read a 40-byte asset: 40 and 80 are served; the third would make
// 120, past 100, and is refused with the budget's own words. Another link of
// the same page still reads (the budget is per link).
func TestALinksAssetBytesAreBudgeted(t *testing.T) {
	w := newAssetWorld(t)
	defer func(n int) { pageAssetByteBudget = n }(pageAssetByteBudget)
	pageAssetByteBudget = 100
	if len(askMediaPNG) != 40 {
		t.Fatalf("fixture: the image is %d bytes, the sums below take 40", len(askMediaPNG))
	}
	id := w.page(t, wsCreator, "Budget")
	img, _, _ := w.publishWith(t, id)
	l, l2 := w.liveLink(t, id), w.liveLink(t, id)
	path := pages.LinkPath(l.Token) + "/assets/" + img
	for _, addr := range []string{"198.51.100.21", "198.51.100.22"} {
		if r := w.readAs(t, path, addr); r.Code != http.StatusOK {
			t.Fatalf("a read under the budget from %s: %d %q", addr, r.Code, r.Body.String())
		}
	}
	if r := w.readAs(t, path, "198.51.100.23"); !throttled(r, pageAssetOverBudget) {
		t.Fatalf("a read past the link's byte budget: %d %q %v", r.Code, r.Body.String(), r.Header())
	}
	if r := w.readAs(t, pages.LinkPath(l2.Token)+"/assets/"+img, "198.51.100.23"); r.Code != http.StatusOK {
		t.Fatalf("another link of the page was refused by the first's budget: %d", r.Code)
	}
}

// failingRevokes is an upload store whose Revoke fails while fail is set.
type failingRevokes struct {
	uploads.Store
	fail bool
}

func (f *failingRevokes) Revoke(ctx context.Context, publicID string) (uploads.Upload, error) {
	if f.fail {
		return uploads.Upload{}, errors.New("the database went away")
	}
	return f.Store.Revoke(ctx, publicID)
}

// TestAnAssetWhoseRevokeFailedIsRevokedWithItsPage: page_asset_delete deletes
// an unused asset's row, but its upload row's revoke fails (forced), so that
// row is still live and no asset names it. Deleting the page then revokes it
// all the same (Files lists every live upload under the page's prefix): no
// live upload row of the page is left.
func TestAnAssetWhoseRevokeFailedIsRevokedWithItsPage(t *testing.T) {
	w := newAssetWorld(t)
	store := &failingRevokes{Store: w.srv.ext.Uploads.Store}
	w.srv.ext.Uploads.Store = store
	id := w.page(t, wsCreator, "Revoke")
	_, rec, _ := w.post(t, w.ws, id, "spare.png", "image/png", askMediaPNG, false)
	aid, _ := rec["asset_id"].(string)
	live := func() int {
		var n int
		if err := w.pool.QueryRow(authz.System(w.ctx), `SELECT count(*) FROM uploads WHERE object_key LIKE $1 AND revoked_at IS NULL`,
			"pages/"+strconv.FormatInt(id, 10)+"/%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	store.fail = true
	if out := w.tool(t, pageAssetDeleteTool, map[string]any{"page_id": id, "asset_id": aid}); out["deleted"] != true {
		t.Fatalf("page_asset_delete of an unused asset: %v", out)
	}
	store.fail = false
	if n := live(); n != 1 {
		t.Fatalf("fixture: after the failed revoke %d live upload row(s), want the 1 left behind", n)
	}
	if r := w.call(t, http.MethodPost, "/pages/"+strconv.FormatInt(id, 10)+"/delete", nil, w.operator()); r.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", r.Code, r.Body.String())
	}
	if n := live(); n != 0 {
		t.Fatalf("deleting the page left %d live upload row(s) under its prefix", n)
	}
}

// TestAnOldVersionKeepsItsAssetsAndDeletingThePageTakesThem: once a version
// that does not use the image is current, the owner's view of the old version
// still reads it while a link (the current version) does not, and
// page_asset_delete refuses it; page_get lists both assets; deleting the page
// (the owner's Delete) removes every object under its prefix, revokes their
// rows, and nothing reads them after.
func TestAnOldVersionKeepsItsAssetsAndDeletingThePageTakesThem(t *testing.T) {
	w := newAssetWorld(t)
	id := w.page(t, wsCreator, "Review")
	img, vid, v := w.publishWith(t, id)
	if up := w.tool(t, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"markdown","text":"no media now"}]`)}); up["error"] != nil {
		t.Fatalf("page_update without the assets: %v", up)
	}
	sess := map[string]string{"Cookie": sessionCookieFor(w.srv)}
	pid, vs := strconv.FormatInt(id, 10), strconv.Itoa(v)
	if r := w.get(t, "/pages/"+pid+"/versions/"+vs+"/assets/"+img, sess); r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), askMediaPNG) {
		t.Fatalf("version %d's image after a newer version: %d", v, r.Code)
	}
	l := w.liveLink(t, id)
	if r := w.get(t, pages.LinkPath(l.Token)+"/assets/"+img, nil); r.Code != http.StatusNotFound {
		t.Errorf("a link (the current version, which does not use it) read the old version's image: %d", r.Code)
	}
	del := w.tool(t, pageAssetDeleteTool, map[string]any{"page_id": id, "asset_id": img})
	if msg, _ := del["error"].(string); !strings.Contains(msg, "an asset is kept while any version refers to it") {
		t.Fatalf("page_asset_delete of an asset in use: %v", del)
	}
	got := w.tool(t, pageGetTool, map[string]any{"page_id": id})
	if as, _ := got["assets"].([]any); len(as) != 2 || pnum(got["asset_bytes"]) != int64(len(askMediaPNG)+len(assetMP4)) {
		t.Fatalf("page_get's assets: %v", got)
	}
	if n := w.objectsUnder(id); n != 2 {
		t.Fatalf("the bucket holds %d of the page's objects, want its 2 assets", n)
	}

	// The owner deletes the page.
	if r := w.call(t, http.MethodPost, "/pages/"+pid+"/delete", nil, w.operator()); r.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", r.Code, r.Body.String())
	}
	if n, o := w.assetRows(t, id), w.objectsUnder(id); n != 0 || o != 0 {
		t.Fatalf("deleting the page left %d asset row(s) and %d object(s)", n, o)
	}
	for _, a := range []string{img, vid} {
		u, err := w.srv.ext.Uploads.Store.Get(asUser(1), a)
		if err != nil || !u.Revoked() {
			t.Errorf("asset %s's upload row after the page's delete: %+v %v", a, u, err)
		}
	}
	if r := w.get(t, "/pages/"+pid+"/versions/"+vs+"/assets/"+img, sess); r.Code != http.StatusNotFound {
		t.Errorf("a deleted page's asset still reads: %d", r.Code)
	}
}

// TestPageAssetsAreAMirroredSessionsOnlyForNow: a dispatched agent is not
// offered the asset tools (its list has neither; a mirrored session's has
// both), has no route to send a file on (the connect surface names none), and
// calling either tool anyway is told so — even naming an asset of its own
// page. A mirrored session naming its page's asset id is told the ref.
func TestPageAssetsAreAMirroredSessionsOnlyForNow(t *testing.T) {
	w := newAssetWorld(t)
	var agentNames, mirrorNames []string
	for _, a := range w.srv.agentTools() {
		agentNames = append(agentNames, a.def.Name)
	}
	for _, d := range w.srv.mirrorTools() {
		mirrorNames = append(mirrorNames, d.Name)
	}
	if !slices.Contains(agentNames, pageCreateTool) || !slices.Contains(mirrorNames, pageAssetUploadTool) || !slices.Contains(mirrorNames, pageAssetDeleteTool) {
		t.Fatalf("control: an agent lists page_create (%v), a mirrored session the asset tools (%v)", slices.Contains(agentNames, pageCreateTool), mirrorNames)
	}
	for _, n := range []string{pageAssetUploadTool, pageAssetDeleteTool} {
		if slices.Contains(agentNames, n) {
			t.Errorf("a dispatched agent is offered %s", n)
		}
	}
	for _, p := range ConnectSurfacePrefixes {
		if strings.Contains(p, "pages") {
			t.Errorf("the connect surface serves %s", p)
		}
	}
	agentPage := w.page(t, pages.Creator{AgentID: w.op.ID, Label: w.op.Name}, "An agent's")
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaAgent})
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{pageAssetUploadTool, map[string]any{"page_id": agentPage, "path": "shots/a.png"}},
		{pageAssetUploadTool, map[string]any{"page_id": agentPage, "asset_id": "AAAAAAAAAAAAAAAAAAAAAA"}},
		{pageAssetDeleteTool, map[string]any{"page_id": agentPage, "asset_id": "AAAAAAAAAAAAAAAAAAAAAA"}},
	} {
		b, _ := json.Marshal(c.args)
		text, ok := w.srv.dispatchCallerTool(ctx, callerOfAgent(w.op), c.name, string(b))
		if !ok || !strings.Contains(text, "available from mirrored sessions only, for now") {
			t.Errorf("an agent's %s %v: %q", c.name, c.args, text)
		}
	}
	// The mirrored session: its page's asset is reported with its ref.
	id := w.page(t, wsCreator, "Mine")
	_, rec, _ := w.post(t, w.ws, id, "shot.png", "image/png", askMediaPNG, false)
	aid, _ := rec["asset_id"].(string)
	if out := w.tool(t, pageAssetUploadTool, map[string]any{"page_id": id, "asset_id": aid}); out["ref"] != "asset:"+aid || out["type"] != "image/png" {
		t.Fatalf("a mirrored session's asset: %v", out)
	}
}
