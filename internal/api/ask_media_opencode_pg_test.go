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

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

// A MIRRORED OPENCODE SESSION'S ASK CARRIES FILES AND A WHY, as a Claude Code
// session's does (ask_media.go; internal/mirrorhost mcpproxy_attach.go), over
// a real Postgres, the real upload service and this package's real mux.

// ocSeamWindows lists the host's opencode window @5: its TUI on ses_launch,
// bound by its launcher record (as ocMirrorWindows' @5 is on muster's side).
type ocSeamWindows struct{}

func (ocSeamWindows) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return []mirrorhost.WindowInfo{{Session: "oc", ID: "@5", Pane: "%5", Active: true,
		Agent: &mirrorhost.AgentPane{Runtime: mirror.RuntimeOpenCode, Pane: "%5", Gen: "ses_launch", Send: true}}}, nil
}

// newOpenCodeAskMediaFixture is newOpenCodeMirrorFixture with the upload
// store on (objs holds its objects).
func newOpenCodeAskMediaFixture(t *testing.T) (*attnFixture, string, string, *uploadstest.Objects) {
	t.Helper()
	objs := &uploadstest.Objects{}
	f, ws, _, members := newMirrorMCPFixture(t, func(ext *Extensions, pool *pgxpool.Pool) {
		sg, err := uploads.NewSigner("ask-media-oc-seam-signing-key-0123456789abcdef")
		if err != nil {
			t.Fatal(err)
		}
		ext.Uploads = &uploads.Service{Store: uploads.NewPG(pool), Objects: objs, Signer: sg, PublicURL: "https://muster.example.test",
			MaxBytes: 25 << 20, LinkTTL: time.Hour}
	})
	f.mirror.mu.Lock()
	f.mirror.windows = append(f.mirror.windows, ocMirrorWindows()...)
	f.mirror.mu.Unlock()
	prev := mirrorBindWait
	mirrorBindWait = 100 * time.Millisecond
	t.Cleanup(func() { mirrorBindWait = prev })
	return f, ws, members, objs
}

// TestAMirroredOpenCodeSessionsFileGoesFromItsHostIntoItsAsk: opencode
// session ses_launch's attention_ask names a file by a path relative to the
// directory its host's opencode server records for it; the host's real proxy
// uploads it as that session (its key, the opencode marker), and the ask is
// filed as ses_launch's carrying it — by its literal name, type and size — and
// the owner fetches it through the ask. A path outside files no ask and stores
// nothing; an ask muster refuses after the upload has the upload taken back,
// which the upload route accepts from the opencode session too.
//
// 🔴 REGRESSION TEST: at the base the host refused an opencode session's
// attachments outright, so no ask was filed.
func TestAMirroredOpenCodeSessionsFileGoesFromItsHostIntoItsAsk(t *testing.T) {
	f, ws, _, objs := newOpenCodeAskMediaFixture(t)
	muster := httptest.NewServer(f.h)
	t.Cleanup(muster.Close)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "shots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shots", "screen.png"), askMediaPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := &mirrorhost.MCPProxy{URL: muster.URL, Token: ws, AllowHTTP: true, Secret: "local-secret", Windows: ocSeamWindows{},
		OpenCodeDirectory: func(id string) (string, bool) { return dir, id == "ses_launch" },
		// The TUI's cwd is the PARENT: a root read from it would reach outside.
		Cwd: func(int) (string, bool) { return filepath.Dir(dir), true }}
	call := func(args map[string]any) map[string]any {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": attentionAskTool, "arguments": args, "_meta": map[string]any{mirror.OpenCodeSessionMeta: "ses_launch"}}})
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(raw)))
		r.Header.Set("Authorization", "Bearer local-secret")
		r.Header.Set(mirrorhost.MCPClientHeader, mirrorhost.MCPClientOpenCode)
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

	out := call(map[string]any{"question": "Is this the right screen?", "context": "checking the header before I ship it", "attachments": []string{"shots/screen.png"}})
	if out["isError"] == true || out["attachments"] != float64(1) {
		t.Fatalf("the ask: %v", out)
	}
	ask, err := attention.NewPG(f.pool).Get(asUser(1), int64(out["id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if ask.Asker.MirrorHost != "ws" || ask.Asker.MirrorSession != "opencode:ses_launch" || len(ask.Attachments) != 1 ||
		ask.Attachments[0].Name != "screen.png" || ask.Attachments[0].Type != "image/png" || ask.Attachments[0].Size != int64(len(askMediaPNG)) {
		t.Fatalf("the ask is %+v with %+v", ask.Asker, ask.Attachments)
	}
	if ask.Why == nil || ask.Why.Text != "checking the header before I ship it" {
		t.Fatalf("the ask's why is %+v", ask.Why)
	}
	up, err := f.srv.ext.Uploads.Store.Get(asUser(1), ask.Attachments[0].ID)
	if err != nil || up.Size != int64(len(askMediaPNG)) {
		t.Fatalf("the upload row: %+v %v", up, err)
	}
	if rec := f.call(t, http.MethodGet, askAttachmentPath(ask.ID, up.PublicID), nil, f.operator()); rec.Code != http.StatusOK || rec.Body.String() != string(askMediaPNG) {
		t.Fatalf("the owner's fetch through the ask: %d", rec.Code)
	}

	// Outside the session's directory — the TUI's cwd, a sibling: refused,
	// nothing stored, no ask.
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "outside.png"), askMediaPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(objs.Keys())
	if out := call(map[string]any{"question": "Escape?", "attachments": []string{"../outside.png"}}); out["isError"] != true ||
		out["error"] != `attachment "../outside.png": the path is outside your workspace` {
		t.Fatalf("an escape: %v", out)
	}
	if len(objs.Keys()) != before {
		t.Fatal("a refused path stored a file")
	}

	// muster refuses the ask (an empty question) after the upload: taken back
	// as the opencode session, so the row is revoked.
	revokedRows := func() int {
		var n int
		if err := f.pool.QueryRow(authz.System(context.Background()),
			`SELECT count(*) FROM uploads WHERE owner_id = 1 AND revoked_at IS NOT NULL AND filename = 'screen.png'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	revokedBefore := revokedRows()
	if out := call(map[string]any{"question": "   ", "attachments": []string{"shots/screen.png"}}); out["isError"] != true {
		t.Fatalf("an empty question with a file was filed: %v", out)
	}
	if revoked := revokedRows() - revokedBefore; revoked != 1 {
		t.Fatalf("the refused ask's upload: %d row(s) revoked; want 1", revoked)
	}
}

// TestTheAskUploadRouteNamesAnOpenCodeSessionByTheDoorsRule: POST and DELETE
// /mirror/ask-attachments take an opencode session as the MCP door does — its
// key in X-Muster-Agent-Session (with or without the marker), for a window
// the host lists running it and sends to — and refuse its key in the Claude
// header, a key beside a Claude session, the marker with no key, a key no
// window binds, and one two windows show. A Claude Code session is named as
// before.
func TestTheAskUploadRouteNamesAnOpenCodeSessionByTheDoorsRule(t *testing.T) {
	f, ws, _, _ := newOpenCodeAskMediaFixture(t)
	auth := "Bearer " + ws
	post := func(h map[string]string) int {
		hdr := map[string]string{"Authorization": auth}
		for k, v := range h {
			hdr[k] = v
		}
		return f.call(t, http.MethodPost, "/mirror/ask-attachments?filename=x.png", strings.NewReader(string(askMediaPNG)), hdr).Code
	}
	for name, c := range map[string]struct {
		h    map[string]string
		want int
	}{
		"opencode key and marker":       {map[string]string{mirrorAgentSessionHeader: "opencode:ses_launch", mirrorClientHeader: mirrorClientOpenCode}, http.StatusCreated},
		"opencode key, no marker":       {map[string]string{mirrorAgentSessionHeader: "opencode:ses_title"}, http.StatusCreated},
		"a Claude session":              {map[string]string{mirrorSessionHeader: focusSID}, http.StatusCreated},
		"opencode key in Claude header": {map[string]string{mirrorSessionHeader: "opencode:ses_launch"}, http.StatusForbidden},
		"key beside a Claude session":   {map[string]string{mirrorAgentSessionHeader: "opencode:ses_launch", mirrorSessionHeader: focusSID}, http.StatusForbidden},
		"marker with a Claude session":  {map[string]string{mirrorClientHeader: mirrorClientOpenCode, mirrorSessionHeader: focusSID}, http.StatusForbidden},
		"marker, no key":                {map[string]string{mirrorClientHeader: mirrorClientOpenCode}, http.StatusForbidden},
		"a Claude id as the agent key":  {map[string]string{mirrorAgentSessionHeader: focusSID, mirrorClientHeader: mirrorClientOpenCode}, http.StatusForbidden},
		"a key no window sends to":      {map[string]string{mirrorAgentSessionHeader: "opencode:ses_view", mirrorClientHeader: mirrorClientOpenCode}, http.StatusForbidden},
		"a key two windows show":        {map[string]string{mirrorAgentSessionHeader: "opencode:ses_twice", mirrorClientHeader: mirrorClientOpenCode}, http.StatusForbidden},
		"a key no window shows":         {map[string]string{mirrorAgentSessionHeader: "opencode:ses_gone", mirrorClientHeader: mirrorClientOpenCode}, http.StatusForbidden},
	} {
		if got := post(c.h); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	// The take-back, by the same rule: refused for the key in the Claude
	// header (the upload stays live), done for the key in its own header.
	rec := f.call(t, http.MethodPost, "/mirror/ask-attachments?filename=y.png", strings.NewReader(string(askMediaPNG)),
		map[string]string{"Authorization": auth, mirrorAgentSessionHeader: "opencode:ses_launch", mirrorClientHeader: mirrorClientOpenCode})
	var r struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &r) != nil || r.ID == "" {
		t.Fatalf("the upload: %d %s", rec.Code, rec.Body)
	}
	if rec := f.call(t, http.MethodDelete, "/mirror/ask-attachments/"+r.ID, nil, map[string]string{"Authorization": auth, mirrorSessionHeader: "opencode:ses_launch"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a take-back naming the key in the Claude header: %d", rec.Code)
	}
	if u, err := f.srv.ext.Uploads.Store.Get(asUser(1), r.ID); err != nil || u.Revoked() {
		t.Fatalf("after a refused take-back: %+v %v", u, err)
	}
	if rec := f.call(t, http.MethodDelete, "/mirror/ask-attachments/"+r.ID, nil,
		map[string]string{"Authorization": auth, mirrorAgentSessionHeader: "opencode:ses_launch", mirrorClientHeader: mirrorClientOpenCode}); rec.Code != http.StatusNoContent {
		t.Fatalf("the opencode session's take-back: %d %s", rec.Code, rec.Body)
	}
	if u, err := f.srv.ext.Uploads.Store.Get(asUser(1), r.ID); err != nil || !u.Revoked() {
		t.Fatalf("after the take-back: %+v %v", u, err)
	}
}

// TestAMirroredOpenCodeSessionsWhyIsItsContextOrItsFocusAndLastWords: an
// opencode session's ask stores its context as given (the fallback not read)
// and, without one, its focus title and its last assistant message — read
// from its OWN window's backfill as an opencode session (the read names the
// opencode runtime and the session's key), secrets redacted — as a Claude
// Code session's does.
//
// 🔴 REGRESSION TEST (the fallback half): at the base an opencode session's
// ask without a context stored no why at all.
func TestAMirroredOpenCodeSessionsWhyIsItsContextOrItsFocusAndLastWords(t *testing.T) {
	f, ws := newOpenCodeMirrorFixture(t)
	const key = "opencode:ses_launch"
	if _, err := focus.NewPG(f.pool).Upsert(asUser(1), focus.MirrorSubject(key), focus.Write{Title: "Ship the opencode header", Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	var reads []chiefnudge.Session
	f.srv.nudgeReadHook = func(_ context.Context, s chiefnudge.Session) (nudgeRead, error) {
		reads = append(reads, s)
		if s.Runtime != mirror.RuntimeOpenCode || s.Key != key {
			return nudgeRead{}, errNoSession
		}
		return nudgeRead{Msgs: []chiefnudge.Msg{{Role: "assistant", Text: "Pushed with token ghp_" + strings.Repeat("b", 36) + " and waiting."}, {Role: "user", Text: "?"}}}, nil
	}
	ask := func(args map[string]any) attention.Ask {
		t.Helper()
		_, r, body := f.mirrorRPC(t, mirrorAsOpenCode(ws, key), mcpToolCall(attentionAskTool, args))
		if r == nil || r.Error != nil {
			t.Fatalf("attention_ask: %s", body)
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		}
		_ = json.Unmarshal(r.Result, &res)
		if res.IsError || len(res.Content) == 0 {
			t.Fatalf("attention_ask refused: %s", body)
		}
		a, err := attention.NewPG(f.pool).Get(asUser(1), mediaAskID(t, res.Content[0].Text))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if a := ask(map[string]any{"question": "Ship?", "context": "CI is green but the canary is red"}); a.Why == nil || a.Why.Text != "CI is green but the canary is red" ||
		a.Why.Focus != "" || a.Why.Last != "" || len(reads) != 0 {
		t.Fatalf("with a context the why is %+v (%d reads)", a.Why, len(reads))
	}
	a := ask(map[string]any{"question": "Ship now?"})
	if a.Asker.MirrorSession != key || a.Why == nil || a.Why.Text != "" || a.Why.Focus != "Ship the opencode header" ||
		!strings.HasPrefix(a.Why.Last, "Pushed with token ") || !strings.HasSuffix(a.Why.Last, " and waiting.") || strings.Contains(a.Why.Last, "ghp_bbbb") || len(reads) != 1 {
		t.Fatalf("without a context the why is %+v (asker %+v, reads %+v)", a.Why, a.Asker, reads)
	}
	if s := reads[0]; s.Window != "@5" || s.TmuxSession != "oc" || s.Host != "ws" {
		t.Fatalf("the backfill read named %+v, want ws's oc:@5", s)
	}
}
