package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/uploads"
)

// A PAGE'S AUTHOR ASSETS through the real server against a real Postgres
// (page_assets.go; internal/pages asset.go): the agent door's upload route,
// its two caps and its type rule, each refusal by its own guard; who reads an
// asset (the owner's view of a version that uses it, a live link of its page
// at the current version, nobody else); and the lifecycle (an old version
// keeps its asset; deleting the page deletes them). Every name is invented.

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

// assetPost sends data to agent a's page id as `ccd page-asset` does, typed
// ct; chunked sends it with no Content-Length (the length unknown until it
// ends).
func (f *pageFixture) assetPost(t *testing.T, a agents.Agent, id int64, name, ct string, data []byte, chunked bool) (int, map[string]any) {
	t.Helper()
	code, out, _ := f.assetPostCounted(t, a, id, name, ct, data, chunked)
	return code, out
}

// assetPostCounted is assetPost, and how many bytes of the body the server
// read.
func (f *pageFixture) assetPostCounted(t *testing.T, a agents.Agent, id int64, name, ct string, data []byte, chunked bool) (int, map[string]any, int) {
	t.Helper()
	body := &countingBody{r: bytes.NewReader(data)}
	r := httptest.NewRequest(http.MethodPost, "/agent/pages/"+strconv.FormatInt(id, 10)+"/assets?filename="+name, nil)
	r.Body = body
	r.ContentLength = int64(len(data))
	if chunked {
		r.ContentLength = -1
	}
	// The fixture's agents carry the token their name makes (newAgent).
	r.Header.Set("X-Muster-Token", a.Name+"-token")
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, body.n
}

// assetFixture is a page fixture whose upload store takes 25 MiB files (the
// asset cap, 15 MiB, is then the tighter one).
func newAssetFixture(t *testing.T) *pageFixture {
	t.Helper()
	f := newPageFixture(t)
	f.srv.ext.Uploads.MaxBytes = 25 << 20
	return f
}

func (f *pageFixture) assetRows(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT count(*) FROM page_assets WHERE page_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// objectsUnder counts the fake bucket's objects under page id's prefix.
func (f *pageFixture) objectsUnder(id int64) int {
	n := 0
	for _, k := range f.objects.Keys() {
		if strings.HasPrefix(k, "pages/"+strconv.FormatInt(id, 10)+"/") {
			n++
		}
	}
	return n
}

// seedAssetBytes records n assets of size bytes on page id as agent a's —
// rows only, through the store (the page cap counts rows).
func (f *pageFixture) seedAssetBytes(t *testing.T, a agents.Agent, id int64, n int, size int64) {
	t.Helper()
	for i := 0; i < n; i++ {
		pid, _ := uploads.NewPublicID()
		if _, err := f.pages.AddAsset(f.as(a.OwnerID, authz.ViaAgent), pages.Creator{AgentID: a.ID, Label: a.Name}, id,
			pages.Asset{PublicID: pid, ContentType: "video/mp4", Size: size}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAnAssetOverItsCapIsRefusedByItsOwnGuard: a 16 MiB file (15 MiB + 1)
// onto an EMPTY page — so the page's 100 MiB has room for it — is refused by
// the asset's own cap: 413 asset_too_large, before a byte is read when its
// length is known, and while it streams when it is not; nothing is stored.
// Control: 2 MiB is taken.
func TestAnAssetOverItsCapIsRefusedByItsOwnGuard(t *testing.T) {
	f := newAssetFixture(t)
	id := f.create(t, f.agent, "Reels", `[{"type":"markdown","text":"soon"}]`)
	big := sized(assetMP4, 16<<20)
	for _, chunked := range []bool{false, true} {
		code, out, read := f.assetPostCounted(t, f.agent, id, "reel.mp4", "video/mp4", big, chunked)
		if code != http.StatusRequestEntityTooLarge || out["code"] != pageAssetTooLargeCode || out["error"] != "the file is over 15 MiB, the most one page asset may be" {
			t.Fatalf("16 MiB (chunked %v): %d %v", chunked, code, out)
		}
		// Known length: refused before a byte is read (the length check).
		// Unknown: refused by the cap held while it streams, once past it.
		if (!chunked && read != 0) || (chunked && read <= int(pages.MaxAssetBytes)) {
			t.Fatalf("16 MiB (chunked %v) was refused after reading %d bytes", chunked, read)
		}
	}
	if n, o := f.assetRows(t, id), f.objectsUnder(id); n != 0 || o != 0 {
		t.Fatalf("a refused asset left %d row(s) and %d object(s)", n, o)
	}
	code, out := f.assetPost(t, f.agent, id, "reel.mp4", "video/mp4", sized(assetMP4, 2<<20), false)
	if code != http.StatusCreated || out["type"] != "video/mp4" || pnum(out["size"]) != 2<<20 || !strings.HasPrefix(out["ref"].(string), "asset:") {
		t.Fatalf("control: 2 MiB: %d %v", code, out)
	}
}

// TestAPageOverItsTotalIsRefusedByItsOwnGuard: a page already holding 90 MiB
// (six 15 MiB assets) refuses an 11 MiB file — under the asset's own cap, so
// only the page's total can refuse it — with 413 page_assets_full, known
// length or streamed; nothing is stored. Control: a 9 MiB file fits.
func TestAPageOverItsTotalIsRefusedByItsOwnGuard(t *testing.T) {
	f := newAssetFixture(t)
	id := f.create(t, f.agent, "Reels", `[{"type":"markdown","text":"soon"}]`)
	f.seedAssetBytes(t, f.agent, id, 6, pages.MaxAssetBytes)
	eleven := sized(assetMP4, 11<<20)
	for _, chunked := range []bool{false, true} {
		code, out, read := f.assetPostCounted(t, f.agent, id, "reel.mp4", "video/mp4", eleven, chunked)
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
	if n, o := f.assetRows(t, id), f.objectsUnder(id); n != 6 || o != 0 {
		t.Fatalf("a refused asset left %d row(s) (want the 6 seeded) and %d object(s)", n, o)
	}
	if code, out := f.assetPost(t, f.agent, id, "reel.mp4", "video/mp4", sized(assetMP4, 9<<20), false); code != http.StatusCreated {
		t.Fatalf("control: 9 MiB onto 90: %d %v", code, out)
	}
}

// TestAnAssetIsTheTypeItsBytesAre: a type that is not one of the six — named,
// or only in the bytes — and a declared type the bytes contradict are refused
// (415), and nothing is stored; the six, declared rightly or not at all, are
// taken.
func TestAnAssetIsTheTypeItsBytesAre(t *testing.T) {
	f := newAssetFixture(t)
	id := f.create(t, f.agent, "Types", `[{"type":"markdown","text":"soon"}]`)
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
		code, out := f.assetPost(t, f.agent, id, c.name, c.ct, c.data, false)
		if code != http.StatusUnsupportedMediaType || out["code"] != "unsupported_media_type" || out["error"] != c.want {
			t.Errorf("%s as %q: %d %v, want 415 %q", c.name, c.ct, code, out, c.want)
		}
	}
	if n, o := f.assetRows(t, id), f.objectsUnder(id); n != 0 || o != 0 {
		t.Fatalf("refused files left %d row(s) and %d object(s)", n, o)
	}
	for _, c := range []struct{ name, ct string }{{"shot.png", "image/png"}, {"shot2.png", "application/octet-stream"}, {"shot3.png", ""}} {
		if code, out := f.assetPost(t, f.agent, id, c.name, c.ct, askMediaPNG, false); code != http.StatusCreated || out["type"] != "image/png" {
			t.Errorf("control: a PNG as %q: %d %v", c.ct, code, out)
		}
	}
	if code, out := f.assetPost(t, f.agent, id, "clip.mp4", "video/mp4", assetMP4, false); code != http.StatusCreated {
		t.Errorf("control: an MP4: %d %v", code, out)
	}
}

// TestOnlyThePagesAuthorAddsAnAsset: the owner's other agent (another creator)
// and another owner's agent are answered 404 for the page; nothing is stored.
func TestOnlyThePagesAuthorAddsAnAsset(t *testing.T) {
	f := newAssetFixture(t)
	id := f.create(t, f.agent, "Mine", `[{"type":"markdown","text":"soon"}]`)
	for _, a := range []agents.Agent{f.agent2, f.otherAgent} {
		if code, out := f.assetPost(t, a, id, "shot.png", "image/png", askMediaPNG, false); code != http.StatusNotFound || out["code"] != "not_found" {
			t.Errorf("agent %s added to another's page: %d %v", a.Name, code, out)
		}
	}
	if n, o := f.assetRows(t, id), f.objectsUnder(id); n != 0 || o != 0 {
		t.Fatalf("refused uploads left %d row(s) and %d object(s)", n, o)
	}
}

// publishWith uploads one PNG and one MP4 to agent a's page id and publishes a
// version using both; their ids and the version.
func (f *pageFixture) publishWith(t *testing.T, a agents.Agent, id int64) (img, vid string, version int) {
	t.Helper()
	code, out := f.assetPost(t, a, id, "shot.png", "image/png", askMediaPNG, false)
	if code != http.StatusCreated {
		t.Fatalf("upload the image: %d %v", code, out)
	}
	img = out["asset_id"].(string)
	code, out = f.assetPost(t, a, id, "clip.mp4", "video/mp4", assetMP4, false)
	if code != http.StatusCreated {
		t.Fatalf("upload the video: %d %v", code, out)
	}
	vid = out["asset_id"].(string)
	blocks, _ := json.Marshal([]map[string]any{
		{"type": "markdown", "text": "Look: ![the shot](asset:" + img + ")"},
		{"type": "html", "html": `<video id="v" src="asset:` + vid + `" controls></video>`},
	})
	up := f.tool(t, a, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(blocks)})
	if up["error"] != nil || pnum(up["version"]) < 2 {
		t.Fatalf("page_update with the assets: %v", up)
	}
	return img, vid, int(pnum(up["version"]))
}

// TestAnAssetIsReadByWhoCanReadItsPageAndNoOneElse: the owner's view of the
// version that uses it, and a live link of its page, read it — inert, the
// same bytes; the content names it for the SDK and draws the markdown image
// (its sandbox unchanged). Another owner's session, a link of ANOTHER page of
// the same owner, the owner's view of a version that does not use it, and a
// revoked link read nothing.
func TestAnAssetIsReadByWhoCanReadItsPageAndNoOneElse(t *testing.T) {
	f := newAssetFixture(t)
	id := f.create(t, f.agent, "Review", `[{"type":"markdown","text":"soon"}]`)
	other := f.create(t, f.agent, "Other", `[{"type":"markdown","text":"elsewhere"}]`)
	img, vid, v := f.publishWith(t, f.agent, id)
	sess := map[string]string{"Cookie": sessionCookieFor(f.srv)}
	vs := strconv.Itoa(v)
	pid := strconv.FormatInt(id, 10)

	// The content: both ids for the SDK, the markdown image as an <img>, and
	// the sandbox exactly as it was.
	cw := f.call(t, http.MethodGet, "/pages/"+pid+"/versions/"+vs+"/content", nil, merge(sess, map[string]string{"Sec-Fetch-Dest": "iframe"}))
	assertContentHeaders(t, cw, "the owner's content")
	if body := cw.Body.String(); !strings.Contains(body, `ASSETS=["`+img+`","`+vid+`"]`) && !strings.Contains(body, `ASSETS=["`+vid+`","`+img+`"]`) {
		t.Errorf("the content does not name its two assets for the SDK: %s", body)
	} else if blocks := pageContentBlocks(t, body); !strings.Contains(blocks, `<img src="asset:`+img+`" alt="the shot">`) || !strings.Contains(blocks, `<video id="v" src="asset:`+vid+`" controls></video>`) {
		t.Errorf("the blocks do not draw the two assets: %s", blocks)
	}

	// The owner's view of the version: both, inert, the stored bytes.
	for _, c := range []struct {
		id, ct string
		data   []byte
	}{{img, "image/png", askMediaPNG}, {vid, "video/mp4", assetMP4}} {
		w := f.call(t, http.MethodGet, "/pages/"+pid+"/versions/"+vs+"/assets/"+c.id, nil, sess)
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), c.data) || w.Header().Get("Content-Type") != c.ct ||
			w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" ||
			!strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Errorf("the owner reads %s: %d %v", c.ct, w.Code, w.Header())
		}
	}
	// Version 1 does not use them.
	if w := f.call(t, http.MethodGet, "/pages/"+pid+"/versions/1/assets/"+img, nil, sess); w.Code != http.StatusNotFound {
		t.Errorf("the owner's view of version 1 reads an asset it does not use: %d", w.Code)
	}
	// Another owner's session: nothing.
	f.session = f.other
	if w := f.call(t, http.MethodGet, "/pages/"+pid+"/versions/"+vs+"/assets/"+img, nil, sess); w.Code != http.StatusNotFound {
		t.Errorf("another owner read the asset: %d %s", w.Code, w.Body.String())
	}
	f.session = f.owner

	// A live link of the page reads both; a link of the OTHER page reads
	// neither (its page's version does not use them).
	l := f.liveLink(t, f.owner, id)
	lo := f.liveLink(t, f.owner, other)
	for _, a := range []string{img, vid} {
		if w := f.get(t, pages.LinkPath(l.Token)+"/assets/"+a, nil); w.Code != http.StatusOK || w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Errorf("the page's link reads %s: %d", a, w.Code)
		}
		if w := f.get(t, pages.LinkPath(lo.Token)+"/assets/"+a, nil); w.Code != http.StatusNotFound || w.Body.String() != pageLinkGone {
			t.Errorf("another page's link read %s: %d %s", a, w.Code, w.Body.String())
		}
	}
	// Revoked: nothing.
	if _, err := f.pages.RevokeLink(f.as(f.owner, authz.ViaSession), id, l.ID); err != nil {
		t.Fatal(err)
	}
	if w := f.get(t, pages.LinkPath(l.Token)+"/assets/"+img, nil); w.Code != http.StatusNotFound || w.Body.String() != pageLinkGone {
		t.Errorf("a revoked link read the asset: %d %s", w.Code, w.Body.String())
	}
}

// TestAnOldVersionKeepsItsAssetsAndDeletingThePageTakesThem: once a version
// that does not use the image is current, the owner's view of the old version
// still reads it while a link (the current version) does not, and
// page_asset_delete refuses it; page_get lists both assets; deleting the page
// (the owner's Delete) removes every object under its prefix, revokes their
// rows, and nothing reads them after.
func TestAnOldVersionKeepsItsAssetsAndDeletingThePageTakesThem(t *testing.T) {
	f := newAssetFixture(t)
	id := f.create(t, f.agent, "Review", `[{"type":"markdown","text":"soon"}]`)
	img, vid, v := f.publishWith(t, f.agent, id)
	if up := f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"markdown","text":"no media now"}]`)}); up["error"] != nil {
		t.Fatalf("page_update without the assets: %v", up)
	}
	sess := map[string]string{"Cookie": sessionCookieFor(f.srv)}
	pid, vs := strconv.FormatInt(id, 10), strconv.Itoa(v)
	if w := f.call(t, http.MethodGet, "/pages/"+pid+"/versions/"+vs+"/assets/"+img, nil, sess); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), askMediaPNG) {
		t.Fatalf("version %d's image after a newer version: %d", v, w.Code)
	}
	l := f.liveLink(t, f.owner, id)
	if w := f.get(t, pages.LinkPath(l.Token)+"/assets/"+img, nil); w.Code != http.StatusNotFound {
		t.Errorf("a link (the current version, which does not use it) read the old version's image: %d", w.Code)
	}
	del := f.tool(t, f.agent, pageAssetDeleteTool, map[string]any{"page_id": id, "asset_id": img})
	if msg, _ := del["error"].(string); !strings.Contains(msg, "an asset is kept while any version refers to it") {
		t.Fatalf("page_asset_delete of an asset in use: %v", del)
	}
	got := f.tool(t, f.agent, pageGetTool, map[string]any{"page_id": id})
	if as, _ := got["assets"].([]any); len(as) != 2 || pnum(got["asset_bytes"]) != int64(len(askMediaPNG)+len(assetMP4)) {
		t.Fatalf("page_get's assets: %v", got)
	}
	if n := f.objectsUnder(id); n != 2 {
		t.Fatalf("the bucket holds %d of the page's objects, want its 2 assets", n)
	}

	// The owner deletes the page.
	if w := f.call(t, http.MethodPost, "/pages/"+pid+"/delete", nil, f.ownerHdr()); w.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if n, o := f.assetRows(t, id), f.objectsUnder(id); n != 0 || o != 0 {
		t.Fatalf("deleting the page left %d asset row(s) and %d object(s)", n, o)
	}
	for _, a := range []string{img, vid} {
		u, err := f.srv.ext.Uploads.Store.Get(f.as(f.owner, authz.ViaSession), a)
		if err != nil || !u.Revoked() {
			t.Errorf("asset %s's upload row after the page's delete: %+v %v", a, u, err)
		}
	}
	if w := f.call(t, http.MethodGet, "/pages/"+pid+"/versions/"+vs+"/assets/"+img, nil, sess); w.Code != http.StatusNotFound {
		t.Errorf("a deleted page's asset still reads: %d", w.Code)
	}
}

// TestPageAssetUploadFromAnAgentPointsAtCcd: a dispatched agent's
// page_asset_upload with a path (muster cannot read its workspace) is told
// the command that sends it; with an asset id of its page it is told the
// ref; with another page's asset id, nothing.
func TestPageAssetUploadFromAnAgentPointsAtCcd(t *testing.T) {
	f := newAssetFixture(t)
	f.agent.Kind = agents.KindClaudeCode
	id := f.create(t, f.agent, "Mine", `[{"type":"markdown","text":"soon"}]`)
	other := f.create(t, f.agent, "Other", `[{"type":"markdown","text":"soon"}]`)
	out := f.tool(t, f.agent, pageAssetUploadTool, map[string]any{"page_id": id, "path": "shots/a.png"})
	if msg, _ := out["error"].(string); !strings.Contains(msg, "run `ccd page-asset "+strconv.FormatInt(id, 10)+" shots/a.png`") {
		t.Fatalf("an agent's path: %v", out)
	}
	_, rec := f.assetPost(t, f.agent, id, "shot.png", "image/png", askMediaPNG, false)
	aid := rec["asset_id"].(string)
	out = f.tool(t, f.agent, pageAssetUploadTool, map[string]any{"page_id": id, "asset_id": aid})
	if out["ref"] != "asset:"+aid || out["type"] != "image/png" {
		t.Fatalf("its asset: %v", out)
	}
	if out := f.tool(t, f.agent, pageAssetUploadTool, map[string]any{"page_id": other, "asset_id": aid}); out["ref"] != nil {
		t.Fatalf("another page's asset through this one: %v", out)
	}
}
