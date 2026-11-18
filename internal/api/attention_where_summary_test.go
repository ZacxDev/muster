package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheSummaryNamesAMirroredAsksWindowFromItsOneHubRead: the seam between
// the summary's ONE hub read (TestTheSummaryReadsTheHubOnce) and an ask that
// names its window (askSource). With a mirrored session's ask open, the
// summary still reads the hub once, and the ask's and the waiting window's
// titles and whys are the exact tmux targets the bar shows.
func TestTheSummaryNamesAMirroredAsksWindowFromItsOneHubRead(t *testing.T) {
	f := newFakeAPITokens()
	tok := f.add(t, 1)
	m := &hostReadCounter{fakeMirror: &fakeMirror{online: true, statusReport: true, windows: []mirror.Window{
		{Session: "scratch3", ID: "@37", Index: 5, Active: true, Label: "mango:1", Claude: true, ClaudeSession: whereLabelledSID, Status: mirror.StatusIdle, StatusSince: attnSince},
		{Session: "build", ID: "@4", Index: 7, Active: true, Claude: true, ClaudeSession: whereUnlabelledSID, Status: mirror.StatusWaiting, StatusSince: attnSince},
	}}}
	s := New(nil, AuthConfig{HookToken: tokTestHook}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: f, Users: stubUsers{}, Mirror: m, Attention: openAsks{asks: []attention.Ask{
		{ID: 61, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: whereLabelledSID, Label: "mango:1 @ ws"}, Question: "Which branch?", CreatedAt: time.Now()},
	}}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	r := httptest.NewRequest(http.MethodGet, attentionSummaryPath, nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	var body attentionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != 200 || err != nil {
		t.Fatalf("the summary %d %s (%v)", rec.Code, rec.Body, err)
	}
	want := map[string][2]string{
		"ask-61":                        {"mango:1 · tmux scratch3:5 @ ws", "Which branch?"},
		sessionKey("ws", "build", "@4"): {"tmux build:7 @ ws", "Waiting: tmux build:7 @ ws"},
	}
	if len(body.Items) != len(want) {
		t.Fatalf("%d items, want %d: %+v", len(body.Items), len(want), body.Items)
	}
	for _, it := range body.Items {
		if w := want[it.Key]; it.Title != w[0] || it.Why != w[1] {
			t.Errorf("%s: title %q why %q, want %q %q", it.Key, it.Title, it.Why, w[0], w[1])
		}
	}
	if m.reads != 1 {
		t.Fatalf("one summary with a mirrored ask read the hub %d times; want 1", m.reads)
	}
}
