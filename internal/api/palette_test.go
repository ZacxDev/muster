package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE PALETTE'S SERVER SEARCH without a database: what the handler hands the
// store, and how it answers a hit.

func paletteServe(t *testing.T, s *Server, q string) []paletteHit {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, ui.PaletteHistoryRoute+"?q="+url.QueryEscape(q), nil).WithContext(operatorCtx())
	w := httptest.NewRecorder()
	s.handlePaletteHistory(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("palette search: %d %s", w.Code, w.Body)
	}
	var out struct {
		Hits []paletteHit `json:"hits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Hits == nil {
		t.Fatalf("palette search answered %s (%v)", w.Body, err)
	}
	return out.Hits
}

// TestAnOverLengthPaletteQueryIsNeverRead: a query longer than any stored title
// (focus.MaxTitleQueryRunes) is answered with no hits WITHOUT reaching the
// store; CONTROL: one exactly that long does reach it (so the fake would see
// the long one if it got through). Red with the handler's cap removed.
func TestAnOverLengthPaletteQueryIsNeverRead(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: claudeWindows()}
	s, _ := focusMirrorServer(t, fm, store, nil)
	long := strings.Repeat("é", focus.MaxTitleQueryRunes+1) // runes, not bytes: 2 bytes each
	if hits := paletteServe(t, s, long); len(hits) != 0 {
		t.Fatalf("an over-length query answered %v", hits)
	}
	if len(store.titleQueries) != 0 {
		t.Fatalf("an over-length query reached the store (%d runes)", len([]rune(store.titleQueries[0])))
	}
	paletteServe(t, s, long[:len(long)-len("é")])
	if len(store.titleQueries) != 1 {
		t.Fatalf("CONTROL: a %d-rune query was not handed to the store: %d calls", focus.MaxTitleQueryRunes, len(store.titleQueries))
	}
}

// TestAnOverLengthTitleQueryIsNeverRead: the store's own cap — SearchTitles on
// a store with NO pool answers an over-length query with no hits (any read
// would dereference the nil pool); CONTROL: a query at the cap does try to
// read (and panics on the nil pool). Red with the store's cap removed.
func TestAnOverLengthTitleQueryIsNeverRead(t *testing.T) {
	st := focus.NewPG(nil)
	live := focus.Live{Agents: []int64{1}}
	since := time.Now().Add(-time.Hour)
	hits, err := st.SearchTitles(operatorCtx(), strings.Repeat("a", focus.MaxTitleQueryRunes+1), live, since, 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("an over-length query: %v %v", hits, err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("CONTROL: a query at the cap did not try to read the (nil) pool")
			}
		}()
		_, _ = st.SearchTitles(operatorCtx(), strings.Repeat("a", focus.MaxTitleQueryRunes), live, since, 5)
	}()
}

// TestAHitNamesEveryWindowOfItsSession: two mirrored windows showing ONE
// Claude session both carry its titles, so a hit on that session is answered
// as both windows' hrefs — not one of them. Red at the first cut, whose href
// map kept one href a subject.
func TestAHitNamesEveryWindowOfItsSession(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{},
		titleHits: []focus.HistoryHit{{Subject: focus.MirrorSubject(focusSID), Title: "Shared session title"}}}
	wins := []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: focusSID, Activity: 1_700_000_000},
		{Session: "main", ID: "@3", Index: 3, Claude: true, ClaudeSession: focusSID, Activity: 1_700_000_000},
		{Session: "main", ID: "@2", Index: 2, Claude: true, Activity: 1_700_000_000},
	}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: wins}
	s, _ := focusMirrorServer(t, fm, store, nil)
	var got []string
	for _, h := range paletteServe(t, s, "shared") {
		if h.Title != "Shared session title" {
			t.Fatalf("hit %+v", h)
		}
		got = append(got, h.Href)
	}
	sort.Strings(got)
	want := []string{ref("main", "@1").ViewPath(), ref("main", "@3").ViewPath()}
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("hrefs %q, want both windows of the session %q", got, want)
	}
}
