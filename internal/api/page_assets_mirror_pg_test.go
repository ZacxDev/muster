package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/askattach"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

// THE SEAM, FOR A PAGE'S IMAGE AND VIDEO: a mirrored Claude Code session's
// page_asset_upload naming a file by PATH, through the host's real MCP proxy
// (internal/mirrorhost) to a real muster (this package's mux, Postgres, the
// upload service). The proxy reads the file from the session's working
// directory and sends it to POST /mirror/pages/{id}/assets with the host's
// token — never inside the MCP body; muster stores it as an asset of the page
// that host made; the forwarded call carries its id and answers its ref; a
// version using the ref is published through the same proxy; and a live link
// of the page reads the bytes.
func TestAMirroredSessionsImageAndVideoReachItsPageThroughItsHost(t *testing.T) {
	objs := &uploadstest.Objects{}
	f, ws, _, members := newMirrorMCPFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		sg, err := uploads.NewSigner("page-asset-seam-signing-key-0123456789abcdef")
		if err != nil {
			t.Fatal(err)
		}
		ext.Uploads = &uploads.Service{Store: uploads.NewPG(pool), Objects: objs, Signer: sg, PublicURL: "https://muster.example.test",
			MaxBytes: 25 << 20, LinkTTL: time.Hour}
		ext.Pages = pages.NewPG(pool)
	})
	store := f.srv.ext.Pages
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `DELETE FROM uploads WHERE owner_id = 1 AND object_key LIKE 'pages/%' AND filename LIKE 'seam-%'`)
		_, _ = f.pool.Exec(sys, `DELETE FROM pages WHERE owner_id = 1 AND mirror_host = 'ws' AND title LIKE 'Seam %'`)
	})
	muster := httptest.NewServer(f.h)
	t.Cleanup(muster.Close)
	cwd := t.TempDir()
	clip := sized(assetMP4, 2<<20)
	for name, data := range map[string][]byte{"seam-shot.png": askMediaPNG, "seam-clip.mp4": clip,
		"seam-page.png": []byte("<!DOCTYPE html><html><script>alert(1)</script></html>")} {
		if err := os.WriteFile(filepath.Join(cwd, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "seam-outside.png")
	if err := os.WriteFile(outside, askMediaPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := &mirrorhost.MCPProxy{URL: muster.URL, Token: ws, AllowHTTP: true, Secret: "local-secret", Windows: seamWindows{},
		Cwd: func(pid int) (string, bool) { return cwd, pid == seamPID }}
	call := func(name string, args map[string]any) map[string]any {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(raw)))
		r.Header.Set("Authorization", "Bearer local-secret")
		r.Header.Set(mirrorhost.MCPPaneHeader, "%7")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		var reply struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
				IsError bool                    `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || len(reply.Result.Content) == 0 {
			t.Fatalf("the proxy answered %d %s", w.Code, w.Body)
		}
		out := map[string]any{}
		if json.Unmarshal([]byte(reply.Result.Content[0].Text), &out) != nil {
			out["text"] = reply.Result.Content[0].Text
		}
		out["isError"] = reply.Result.IsError
		return out
	}

	created := call(pageCreateTool, map[string]any{"title": "Seam review", "blocks": []map[string]any{{"type": "markdown", "text": "soon"}}})
	id := pnum(created["page_id"])
	if created["isError"] == true || id <= 0 {
		t.Fatalf("page_create through the proxy: %v", created)
	}
	ids := map[string]string{}
	for _, c := range []struct{ file, ct string }{{"seam-shot.png", "image/png"}, {"seam-clip.mp4", "video/mp4"}} {
		out := call(askattach.PageAssetTool, map[string]any{"page_id": id, "path": c.file})
		ref, _ := out["ref"].(string)
		if out["isError"] == true || out["type"] != c.ct || !strings.HasPrefix(ref, pages.AssetScheme) {
			t.Fatalf("page_asset_upload %s: %v", c.file, out)
		}
		ids[c.file] = strings.TrimPrefix(ref, pages.AssetScheme)
	}
	owner := asUser(1)
	list, err := store.Assets(owner, id)
	if err != nil || len(list) != 2 {
		t.Fatalf("the page's assets: %+v %v", list, err)
	}
	// The bytes are in the store under the page's prefix: the MP4 whole.
	body, size, err := objs.Get(owner, uploads.PageObjectKeyFor(id, ids["seam-clip.mp4"]))
	if err != nil || size != int64(len(clip)) {
		t.Fatalf("the video's object: %d %v", size, err)
	}
	_ = body.Close()

	// Refused, not forwarded: a path out of the working directory, and a file
	// whose bytes are not what its name says.
	for _, c := range []struct{ path, want string }{{outside, "outside"}, {"seam-page.png", "the file is text/html"}} {
		out := call(askattach.PageAssetTool, map[string]any{"page_id": id, "path": c.path})
		msg, _ := out["error"].(string)
		if out["isError"] != true || !strings.Contains(strings.ToLower(msg), strings.ToLower(c.want)) {
			t.Errorf("page_asset_upload %s: %v, want a refusal naming %q", c.path, out, c.want)
		}
	}
	if list, _ := store.Assets(owner, id); len(list) != 2 {
		t.Fatalf("a refused file was stored: %d assets", len(list))
	}
	// Another owner's host of the same name cannot attach to this page.
	r := httptest.NewRequest(http.MethodPost, askattach.PageAssetMirrorPath(id)+"?filename=x.png", strings.NewReader(string(askMediaPNG)))
	for k, v := range mirrorAs(members, focusSID) {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code < 400 {
		t.Fatalf("another owner's host attached to the page: %d %s", w.Code, w.Body)
	}

	// A version using both, through the same proxy; a live link reads them.
	upd := call(pageUpdateTool, map[string]any{"page_id": id, "blocks": []map[string]any{
		{"type": "markdown", "text": "![the shot](asset:" + ids["seam-shot.png"] + ")"},
		{"type": "html", "html": `<video src="asset:` + ids["seam-clip.mp4"] + `" controls></video>`}}})
	if upd["isError"] == true || pnum(upd["version"]) != 2 {
		t.Fatalf("page_update with the refs: %v", upd)
	}
	l, err := store.CreateLink(owner, id, pages.LinkLive, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	for file, aid := range ids {
		lw := f.call(t, http.MethodGet, pages.LinkPath(l.Token)+"/assets/"+aid, nil, nil)
		if lw.Code != http.StatusOK || lw.Body.Len() == 0 {
			t.Errorf("the link reads %s: %d", file, lw.Code)
		}
	}
	if lw := f.call(t, http.MethodGet, pages.LinkPath(l.Token)+"/assets/"+ids["seam-clip.mp4"], nil, nil); lw.Body.Len() != len(clip) ||
		lw.Header().Get("Content-Length") != strconv.Itoa(len(clip)) {
		t.Errorf("the link's video is %d bytes, want %d", lw.Body.Len(), len(clip))
	}
}
