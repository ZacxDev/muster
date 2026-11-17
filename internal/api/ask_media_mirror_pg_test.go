package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/askattach"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

// THE SEAM: a mirrored Claude Code session's attention_ask with attachment
// PATHS, through the host's real MCP proxy (internal/mirrorhost) to a real
// muster (this package's mux, Postgres, the upload service). The proxy reads
// the file from the session's working directory and sends it to
// POST /mirror/ask-attachments with the host's token; muster stores it as the
// host owner's upload; the forwarded call carries its id; the ask is filed
// with it; and the owner's session fetches it through the ask.

// seamWindows lists the host's one Claude pane: %7 running focusSID, process
// seamPID.
type seamWindows struct{}

const seamPID = 7777

func (seamWindows) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return []mirrorhost.WindowInfo{{Session: "main", ID: "@1", Pane: "%7", Active: true,
		Claude: &mirrorhost.ClaudePane{Pane: "%7", Gen: focusSID, PID: seamPID}}}, nil
}

func TestAMirroredSessionsFileGoesFromItsHostIntoTheUploadStoreAndItsAskShowsIt(t *testing.T) {
	objs := &uploadstest.Objects{}
	f, ws, _, members := newMirrorMCPFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		sg, err := uploads.NewSigner("ask-media-seam-signing-key-0123456789abcdef")
		if err != nil {
			t.Fatal(err)
		}
		ext.Uploads = &uploads.Service{Store: uploads.NewPG(pool), Objects: objs, Signer: sg, PublicURL: "https://muster.example.test",
			MaxBytes: 25 << 20, LinkTTL: time.Hour}
	})
	muster := httptest.NewServer(f.h)
	t.Cleanup(muster.Close)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "screen.png"), askMediaPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := &mirrorhost.MCPProxy{URL: muster.URL, Token: ws, AllowHTTP: true, Secret: "local-secret", Windows: seamWindows{},
		Cwd: func(pid int) (string, bool) { return cwd, pid == seamPID }}
	call := func(args map[string]any) map[string]any {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": attentionAskTool, "arguments": args}})
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
		var out map[string]any
		_ = json.Unmarshal([]byte(reply.Result.Content[0].Text), &out)
		out["isError"] = reply.Result.IsError
		return out
	}

	out := call(map[string]any{"question": "Is this the right screen?", "attachments": []string{"screen.png"}})
	if out["isError"] == true || out["attachments"] != float64(1) {
		t.Fatalf("the ask: %v", out)
	}
	ask, err := attention.NewPG(f.pool).Get(asUser(1), int64(out["id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if ask.Asker.MirrorHost != "ws" || ask.Asker.MirrorSession != focusSID || len(ask.Attachments) != 1 || ask.Attachments[0].Name != "screen.png" ||
		ask.Attachments[0].Type != "image/png" {
		t.Fatalf("the ask is %+v with %+v", ask.Asker, ask.Attachments)
	}
	// The bytes are in the upload store, as the host owner's upload.
	up, err := f.srv.ext.Uploads.Store.Get(asUser(1), ask.Attachments[0].ID)
	if err != nil || up.Size != int64(len(askMediaPNG)) {
		t.Fatalf("the upload row: %+v %v", up, err)
	}
	// Another owner's host cannot take it back: no 2xx, and it stays live.
	if rec := f.call(t, http.MethodDelete, "/mirror/ask-attachments/"+up.PublicID, nil, mirrorAs(members, focusSID)); rec.Code < 300 {
		t.Fatalf("another owner's host revoked the operator's upload: %d", rec.Code)
	}
	if u, err := f.srv.ext.Uploads.Store.Get(asUser(1), up.PublicID); err != nil || u.Revoked() {
		t.Fatalf("after another owner's take-back: %+v %v", u, err)
	}
	if rec := f.call(t, http.MethodGet, askAttachmentPath(ask.ID, up.PublicID), nil, f.operator()); rec.Code != http.StatusOK || rec.Body.String() != string(askMediaPNG) {
		t.Fatalf("the owner's fetch through the ask: %d", rec.Code)
	}
	// A path outside the session's working directory files no ask and
	// stores nothing.
	before := len(objs.Keys())
	if out := call(map[string]any{"question": "Escape?", "attachments": []string{"../../etc/passwd"}}); out["isError"] != true ||
		!strings.Contains(out["error"].(string), "outside your workspace") {
		t.Fatalf("an escape: %v", out)
	}
	if len(objs.Keys()) != before {
		t.Fatal("a refused path stored a file")
	}
	// An ask muster refuses (an empty question) AFTER its file was uploaded:
	// the proxy takes the upload back (round 1 G2), so the row is revoked and
	// its object gone.
	keysBefore := map[string]bool{}
	for _, k := range objs.Keys() {
		keysBefore[k] = true
	}
	revokedRows := func() int {
		var n int
		if err := f.pool.QueryRow(authz.System(context.Background()),
			`SELECT count(*) FROM uploads WHERE owner_id = 1 AND revoked_at IS NOT NULL AND filename = 'screen.png'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	revokedBefore := revokedRows()
	if out := call(map[string]any{"question": "   ", "attachments": []string{"screen.png"}}); out["isError"] != true {
		t.Fatalf("an empty question with a file was filed: %v", out)
	}
	var orphan string
	for _, k := range objs.Keys() {
		if !keysBefore[k] {
			orphan = k
		}
	}
	if revoked := revokedRows() - revokedBefore; orphan != "" || revoked != 1 {
		t.Fatalf("the refused ask's upload: object left %q, %d row(s) revoked; want none left and 1", orphan, revoked)
	}
	// The ask's own cap, below the store's (25 MiB here): one byte over
	// askattach.MaxBytes is a 413 and stores nothing; exactly at it is stored.
	big := make([]byte, askattach.MaxBytes+1)
	copy(big, askMediaPNG)
	if rec := f.call(t, http.MethodPost, "/mirror/ask-attachments?filename=big.png", strings.NewReader(string(big)), mirrorAs(ws, focusSID)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a file one byte over the ask's cap: %d", rec.Code)
	}
	if rec := f.call(t, http.MethodPost, "/mirror/ask-attachments?filename=max.png", strings.NewReader(string(big[:askattach.MaxBytes])), mirrorAs(ws, focusSID)); rec.Code != http.StatusCreated {
		t.Fatalf("a file exactly at the ask's cap: %d %s", rec.Code, rec.Body)
	}
	// The upload route itself refuses a session this host does not list.
	rec := f.call(t, http.MethodPost, "/mirror/ask-attachments?filename=x.png", strings.NewReader(string(askMediaPNG)), mirrorAs(ws, "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an unlisted session's upload: %d %s", rec.Code, rec.Body)
	}
}
