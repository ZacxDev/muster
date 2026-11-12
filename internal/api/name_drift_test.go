package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/sessionname"
	"github.com/ZacxDev/muster/internal/ui"
)

// driftedStore is window @1's conversation named "Billing migration" (auto),
// whose newest three focus changes are about the Stripe webhook, the oldest
// of them an hour old: a drift focus.DriftOffer offers.
func driftedStore(now time.Time) *memFocus {
	sub := focus.MirrorSubject(focusSID)
	return &memFocus{
		rows: map[focus.Subject]focus.Focus{sub: {Subject: sub, Title: "Fixing the Stripe webhook retry", UpdatedAt: now,
			Name: focus.Name{Auto: "Billing migration"}}},
		history: map[focus.Subject][]focus.HistoryEntry{sub: {
			{ID: 4, Title: "Fixing the Stripe webhook retry", CreatedAt: now.Add(-time.Minute)},
			{ID: 3, Title: "Reading the Stripe webhook docs", CreatedAt: now.Add(-30 * time.Minute)},
			{ID: 2, Title: "Tracing the webhook retry queue", CreatedAt: now.Add(-time.Hour)},
			{ID: 1, Title: "Billing migration", CreatedAt: now.Add(-2 * time.Hour)},
		}},
	}
}

// TestADriftedSessionIsOfferedARenameNeverGivenOne: the Rendered view of a
// session whose focus has moved on draws the offer (the current title), and
// the name is untouched; "Keep the name" (the real route) hides it until the
// title changes; a NEW title offers again; accepting it is an ordinary rename,
// after which the name is manual and nothing is offered.
func TestADriftedSessionIsOfferedARenameNeverGivenOne(t *testing.T) {
	now := time.Now()
	store := driftedStore(now)
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, ts := focusMirrorServer(t, fm, store, nil)
	view := ref("main", "@1").ViewPath()
	offerOn := func(page string) string {
		const attr = `data-session-drift="`
		i := strings.Index(page, attr)
		if i < 0 {
			return ""
		}
		rest := page[i+len(attr):]
		return rest[:strings.Index(rest, `"`)]
	}
	_, page := mirrorGet(t, s, ts, view)
	if got := offerOn(page); got != "Fixing the Stripe webhook retry" {
		t.Fatalf("offer %q, want the current focus title", got)
	}
	if !strings.Contains(page, "<title>Billing migration") {
		t.Fatal("the offer renamed the session")
	}
	h := s.Handler()
	rec := pinPost(t, s, h, ui.SessionDriftDismissPath, url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"main"},
		"window": {"@1"}, "title": {"Fixing the Stripe webhook retry"}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d %s", rec.Code, rec.Body)
	}
	if _, page = mirrorGet(t, s, ts, view); offerOn(page) != "" {
		t.Fatal("a dismissed offer is still drawn")
	}
	// The focus moves on again: the dismissal no longer holds.
	sub := focus.MirrorSubject(focusSID)
	store.mu.Lock()
	store.history[sub] = append([]focus.HistoryEntry{{ID: 5, Title: "Wiring the Stripe refund flow", CreatedAt: now}}, store.history[sub]...)
	f := store.rows[sub]
	f.Title = "Wiring the Stripe refund flow"
	store.rows[sub] = f
	store.mu.Unlock()
	if _, page = mirrorGet(t, s, ts, view); offerOn(page) != "Wiring the Stripe refund flow" {
		t.Fatalf("after a new title the offer is %q", offerOn(page))
	}
	// Accept: the offer's own form, a rename.
	rec = pinPost(t, s, h, ui.SessionNamePath, url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"},
		"name": {"Wiring the Stripe refund flow"}}, true)
	if rec.Code != http.StatusOK || store.rows[sub].Name.Manual != "Wiring the Stripe refund flow" {
		t.Fatalf("accept = %d, manual name %q", rec.Code, store.rows[sub].Name.Manual)
	}
	if _, page = mirrorGet(t, s, ts, view); offerOn(page) != "" || !strings.Contains(page, `data-session-name="manual"`) {
		t.Fatal("after accepting, the offer is still drawn or the name is not manual")
	}
}

// TestNoOfferWithoutSustainedDrift: one diverged change, or three within the
// last half hour, offer nothing; a fallback-named window never drifts.
func TestNoOfferWithoutSustainedDrift(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(*memFocus){
		"two of three back on the name": func(m *memFocus) {
			h := m.history[focus.MirrorSubject(focusSID)]
			h[1].Title = "Back to the billing migration"
		},
		"too recent": func(m *memFocus) {
			h := m.history[focus.MirrorSubject(focusSID)]
			h[2].CreatedAt = now.Add(-10 * time.Minute)
		},
		"no name to drift from": func(m *memFocus) {
			f := m.rows[focus.MirrorSubject(focusSID)]
			f.Name = focus.Name{}
			m.rows[focus.MirrorSubject(focusSID)] = f
		},
	} {
		store := driftedStore(now)
		mutate(store)
		fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
		s, ts := focusMirrorServer(t, fm, store, nil)
		if _, page := mirrorGet(t, s, ts, ref("main", "@1").ViewPath()); strings.Contains(page, "data-session-drift=") {
			t.Errorf("%s: an offer is drawn", name)
		}
	}
}

// TestATaskNameDriftsLikeAnyStableName: a task-dispatched name is the agent's
// stable name (operator, #504 Q1), so it is offered a rename on drift like an
// auto or manual one; a system name (the chief, an officer) and a technical
// fallback never are.
func TestATaskNameDriftsLikeAnyStableName(t *testing.T) {
	now := time.Now()
	sub := focus.AgentSubject(7)
	store := driftedStore(now)
	store.history[sub] = store.history[focus.MirrorSubject(focusSID)]
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, store, nil)
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(operatorCtx())
	for src, want := range map[sessionname.Source]string{
		sessionname.SourceDispatch: "Fixing the Stripe webhook retry",
		sessionname.SourceAuto:     "Fixing the Stripe webhook retry",
		sessionname.SourceManual:   "Fixing the Stripe webhook retry",
		sessionname.SourceSystem:   "",
		sessionname.SourceFallback: "",
	} {
		n := sessionname.Name{Text: "#42 · Billing migration", Source: src}
		if got := s.driftOffer(r, sub, n, time.Time{}); got != want {
			t.Errorf("a %s name: offer %q, want %q", src, got, want)
		}
	}
}

// TestARenameRestartsTheDriftClock: right after a person renames a drifted
// session, nothing is offered — the history it was renamed over does not
// count; three drifted changes AFTER the rename, the oldest DriftMinAge old,
// are offered again.
func TestARenameRestartsTheDriftClock(t *testing.T) {
	now := time.Now()
	store := driftedStore(now)
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, ts := focusMirrorServer(t, fm, store, nil)
	view := ref("main", "@1").ViewPath()
	if _, page := mirrorGet(t, s, ts, view); !strings.Contains(page, "data-session-drift=") {
		t.Fatal("control: the drifted session is offered a rename before it is renamed")
	}
	if rec := pinPost(t, s, s.Handler(), ui.SessionNamePath, url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"},
		"name": {"Payments refactor"}}, true); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d", rec.Code)
	}
	if _, page := mirrorGet(t, s, ts, view); strings.Contains(page, "data-session-drift=") {
		t.Fatal("a session renamed a moment ago is offered a rename on the history it was renamed over")
	}
	// Three drifted changes after the rename, the oldest 40 minutes before
	// "now" (the server's clock moved on).
	sub := focus.MirrorSubject(focusSID)
	store.mu.Lock()
	renamed := store.rows[sub].Name.ManualAt
	store.history[sub] = append([]focus.HistoryEntry{
		{ID: 7, Title: "Wiring the Stripe refund flow", CreatedAt: renamed.Add(90 * time.Minute)},
		{ID: 6, Title: "Reading the Stripe refund docs", CreatedAt: renamed.Add(70 * time.Minute)},
		{ID: 5, Title: "Tracing the Stripe refund queue", CreatedAt: renamed.Add(60 * time.Minute)},
	}, store.history[sub]...)
	store.mu.Unlock()
	s.now = func() time.Time { return renamed.Add(100 * time.Minute) }
	if _, page := mirrorGet(t, s, ts, view); !strings.Contains(page, `data-session-drift="Wiring the Stripe refund flow"`) {
		t.Fatal("three drifted changes after the rename, the oldest 40 minutes old, offer nothing")
	}
}

// renameForm is a rename as the page posts it — the drift offer's Rename
// and the rename dialog's Save post the same fields; nothing tells them apart.
func renameForm(name string) url.Values {
	return url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"main"}, "window": {"@1"}, "back": {ref("main", "@1").ViewPath()},
		"name": {name}}
}

// postFrom is pinPost from the page at current (HX-Current-URL, which htmx
// sends with every request: the address of the document that posted).
func postFrom(t *testing.T, s *Server, path string, form url.Values, current string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.Header.Set("HX-Request", "true")
	if current != "" {
		req.Header.Set("HX-Current-URL", "http://"+req.Host+current)
	}
	admit(s, req)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// expectNoReload fails when rec could reload or navigate the page: an
// HX-Refresh, an HX-Redirect or HX-Location, or an HTTP redirect.
func expectNoReload(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d, want 200: %s", what, rec.Code, rec.Body)
	}
	for _, h := range []string{"HX-Refresh", "HX-Redirect", "HX-Location", "Location"} {
		if v := rec.Header().Get(h); v != "" {
			t.Errorf("%s answers %s: %q — the page would reload or navigate", what, h, v)
		}
	}
}

// settled is the muster:renamed event an answer fires once it settles
// (HX-Trigger-After-Settle): the tab's new title and what takes focus.
func settled(t *testing.T, rec *httptest.ResponseRecorder) (title, focusOn string) {
	t.Helper()
	raw := rec.Header().Get("HX-Trigger-After-Settle")
	var ev map[string]map[string]string
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("HX-Trigger-After-Settle %q: %v", raw, err)
	}
	d, ok := ev["muster:renamed"]
	if !ok {
		t.Fatalf("HX-Trigger-After-Settle %q fires no muster:renamed", raw)
	}
	return d["title"], d["focus"]
}

// elementOf is the element whose opening tag starts with open, through its
// first closing tag close — out of html, "" when there is none. With the
// answer's hx-swap-oob removed (attrs follow the id), an answer's element and
// the page's compare equal.
func elementOf(html, open, close string) string {
	i := strings.Index(html, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(html[i:], close)
	if j < 0 {
		return ""
	}
	return html[i : i+j+len(close)]
}

func withoutOOBAttr(s string) string { return strings.ReplaceAll(s, ` hx-swap-oob="outerHTML"`, "") }

// TestTheOfferIsAnsweredInPlace: through the real routes, the drift offer's
// two buttons as htmx posts them from the Rendered view are answered WITHOUT a
// reload (operator: a tap reloaded the whole page; both answered HX-Refresh).
// "Keep" deletes the banner out of band, records the dismissal and moves
// focus to the title; "Rename" stores the manual name (an ordinary rename)
// and answers with the title button and full title redrawn, the tab's title
// and focus in the settle event, plus agents:changed for the sidebar.
func TestTheOfferIsAnsweredInPlace(t *testing.T) {
	now := time.Now()
	store := driftedStore(now)
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, store, nil)
	sub := focus.MirrorSubject(focusSID)
	view := ref("main", "@1").ViewPath()

	keep := postFrom(t, s, ui.SessionDriftDismissPath, url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"main"},
		"window": {"@1"}, "title": {"Fixing the Stripe webhook retry"}, "back": {view}}, view)
	expectNoReload(t, "keep", keep)
	if body := keep.Body.String(); !strings.Contains(body, `<div id="session-drift" hx-swap-oob="delete"></div>`) ||
		!strings.Contains(body, `id="session-drift-status" hx-swap-oob="innerHTML">Kept the name.`) {
		t.Errorf("keep's answer does not remove the banner in place:\n%s", body)
	}
	if title, focusOn := settled(t, keep); title != "" || focusOn != "mv-heading" {
		t.Errorf("keep settles title %q focus %q, want no title and focus on mv-heading", title, focusOn)
	}
	if store.rows[sub].Name.DriftDismissed != "Fixing the Stripe webhook retry" {
		t.Errorf("keep recorded %q", store.rows[sub].Name.DriftDismissed)
	}

	const offer = "Wiring the Stripe refund flow"
	rec := postFrom(t, s, ui.SessionNamePath, renameForm(offer), view)
	expectNoReload(t, "rename", rec)
	if got := store.rows[sub].Name.Manual; got != offer {
		t.Fatalf("the offer's Rename stored manual name %q, want %q", got, offer)
	}
	if got := rec.Header().Get("HX-Trigger"); got != "agents:changed" {
		t.Errorf("HX-Trigger %q, want agents:changed (the sidebar's refresh)", got)
	}
	if title, focusOn := settled(t, rec); title != offer+" · Mirrored · muster" || focusOn != "mv-heading" {
		t.Errorf("rename settles title %q focus %q", title, focusOn)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<div id="session-drift" hx-swap-oob="delete"></div>`,
		`hx-swap-oob="innerHTML">Renamed to “` + offer + `”.</p>`,
		`<button id="mv-title" hx-swap-oob="outerHTML"`,
		`data-session-name="manual"`,
		`id="session-rename" hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the rename's answer lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Billing migration") || strings.Contains(body, "<title") {
		t.Errorf("the answer names the old name, or carries a <title> htmx would decode:\n%s", body)
	}
}

// TestADialogRenameIsAnsweredOnThePageThatPostedIt: the ⋯ menu's Rename…
// dialog (the same route, the same fields) is answered in place too — and on
// the page that POSTED it. From the Rendered view the title button is
// swapped; from the window's Terminal page (which has no #mv-title) its own
// heading, #term-title, with its own tab title "Terminal · … · muster". Each
// swapped element is EXACTLY what a reload of that page draws (a GET of it),
// for a name given and for a name CLEARED back to the label.
func TestADialogRenameIsAnsweredOnThePageThatPostedIt(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{}}
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, ts := focusMirrorServer(t, fm, store, nil)
	view, term := ref("main", "@1").ViewPath(), ref("main", "@1").TerminalPath()
	for _, tc := range []struct {
		name, page, open, close, focusOn, titlePrefix, shown string
	}{
		{"Payments", view, `<button id="mv-title"`, `</button>`, "mv-heading", "", "Payments"},
		{"Ledger sync", term, `<h1 id="term-title"`, `</h1>`, "term-title", "Terminal · ", "Ledger sync"},
		// Cleared: no human name is left — the window's label heads it again.
		{"", view, `<button id="mv-title"`, `</button>`, "mv-heading", "", "mango:1"},
		{"", term, `<h1 id="term-title"`, `</h1>`, "term-title", "Terminal · ", "mango:1"},
	} {
		what := tc.page + " " + strconv.Quote(tc.name)
		rec := postFrom(t, s, ui.SessionNamePath, renameForm(tc.name), tc.page)
		expectNoReload(t, what, rec)
		title, focusOn := settled(t, rec)
		if focusOn != tc.focusOn || !strings.HasPrefix(title, tc.titlePrefix+tc.shown) {
			t.Errorf("%s: settles title %q focus %q, want %q… on %s", what, title, focusOn, tc.titlePrefix+tc.shown, tc.focusOn)
		}
		_, page := mirrorGet(t, s, ts, tc.page)
		got := withoutOOBAttr(elementOf(rec.Body.String(), tc.open+` hx-swap-oob="outerHTML"`, tc.close))
		want := elementOf(page, tc.open, tc.close)
		if got == "" || want == "" || got != want {
			t.Errorf("%s: the answer's element is not the page's:\n answer %s\n page   %s", what, got, want)
		}
		if !strings.Contains(want, ">"+tc.shown+"<") {
			t.Errorf("%s: control: the page's element does not show %q:\n%s", what, tc.shown, want)
		}
		if !strings.Contains(page, "<title>"+strings.ReplaceAll(title, "&", "&amp;")+"</title>") {
			t.Errorf("%s: the settled title %q is not the page's <title>", what, title)
		}
	}
}

// TestTheSettledTitleIsASCIIAndExact: a header is read by the browser byte
// for byte, so the settle event is ASCII — every other rune escaped — and
// decodes to the title exactly, a literal "&amp;" included.
func TestTheSettledTitleIsASCIIAndExact(t *testing.T) {
	const title = "R&amp;D “sync” 🚀 · Mirrored · muster"
	raw := renameEvent(title, "mv-title")
	for i := 0; i < len(raw); i++ {
		if raw[i] >= 0x80 || raw[i] < 0x20 {
			t.Fatalf("byte %d of %q is not printable ASCII", i, raw)
		}
	}
	var ev map[string]map[string]string
	if err := json.Unmarshal([]byte(raw), &ev); err != nil || ev["muster:renamed"]["title"] != title {
		t.Fatalf("%q decodes to %v (%v), want %q", raw, ev, err, title)
	}
}

// TestTheOfferFallsBackWithoutScript: the forms posted WITHOUT htmx (no
// script) are answered with a 303 back to the session's page, as before.
func TestTheOfferFallsBackWithoutScript(t *testing.T) {
	store := driftedStore(time.Now())
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, store, nil)
	h := s.Handler()
	back := ref("main", "@1").ViewPath()
	keep := pinPost(t, s, h, ui.SessionDriftDismissPath, url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"main"},
		"window": {"@1"}, "title": {"Fixing the Stripe webhook retry"}, "back": {back}}, false)
	if keep.Code != http.StatusSeeOther || keep.Header().Get("Location") != back {
		t.Fatalf("keep without script = %d to %q, want 303 to %q", keep.Code, keep.Header().Get("Location"), back)
	}
	rename := pinPost(t, s, h, ui.SessionNamePath, renameForm("Wiring the Stripe refund flow"), false)
	if rename.Code != http.StatusSeeOther || rename.Header().Get("Location") != back {
		t.Fatalf("rename without script = %d to %q, want 303 to %q", rename.Code, rename.Header().Get("Location"), back)
	}
}
