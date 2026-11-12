package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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

// inPlaceForm is the drift offer's Rename as htmx posts it: the form's hidden
// fields plus the hx-vals marker ("drift", ui.SessionDriftParam — spelled
// literally: the contract the banner's markup carries).
func inPlaceForm(name string) url.Values {
	return url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"main"}, "window": {"@1"}, "back": {ref("main", "@1").ViewPath()},
		"name": {name}, "drift": {"1"}}
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

// TestTheOfferIsAnsweredInPlace: through the real routes, the drift offer's
// two buttons as htmx posts them are answered WITHOUT a reload (operator: a
// tap reloaded the whole page; both answered HX-Refresh). "Keep" deletes the
// banner out of band and records the dismissal; "Rename" stores the manual
// name (an ordinary rename) and answers with the header's name, its full
// title, the <title> and the switcher redrawn with the NEW name, plus
// agents:changed for the sidebar.
func TestTheOfferIsAnsweredInPlace(t *testing.T) {
	now := time.Now()
	store := driftedStore(now)
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, store, nil)
	h := s.Handler()
	sub := focus.MirrorSubject(focusSID)

	keep := pinPost(t, s, h, ui.SessionDriftDismissPath, url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"main"},
		"window": {"@1"}, "title": {"Fixing the Stripe webhook retry"}, "back": {ref("main", "@1").ViewPath()}}, true)
	expectNoReload(t, "keep", keep)
	if body := keep.Body.String(); !strings.Contains(body, `<div id="session-drift" hx-swap-oob="delete"></div>`) ||
		!strings.Contains(body, `id="session-drift-status" hx-swap-oob="innerHTML">Kept the name.`) {
		t.Fatalf("keep's answer does not remove the banner in place:\n%s", body)
	}
	if store.rows[sub].Name.DriftDismissed != "Fixing the Stripe webhook retry" {
		t.Fatalf("keep recorded %q", store.rows[sub].Name.DriftDismissed)
	}

	const offer = "Wiring the Stripe refund flow"
	rec := pinPost(t, s, h, ui.SessionNamePath, inPlaceForm(offer), true)
	expectNoReload(t, "rename", rec)
	if got := store.rows[sub].Name.Manual; got != offer {
		t.Fatalf("the offer's Rename stored manual name %q, want %q", got, offer)
	}
	if got := rec.Header().Get("HX-Trigger"); got != "agents:changed" {
		t.Errorf("HX-Trigger %q, want agents:changed (the sidebar's refresh)", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<div id="session-drift" hx-swap-oob="delete"></div>`,
		`hx-swap-oob="innerHTML">Renamed to “` + offer + `”.</p>`,
		`<span id="mv-name" hx-swap-oob="outerHTML" data-mirror-name="" data-session-name="manual"`,
		`>` + offer + `</span>`,
		`<title>` + offer + ` · Mirrored · muster</title>`,
		`id="session-rename" hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the rename's answer lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Billing migration") {
		t.Errorf("the answer still names the session by its old name:\n%s", body)
	}
}

// TestTheOfferFallsBackWithoutScript: the same two forms posted WITHOUT htmx
// (no script) are answered with a 303 back to the session's page, as before
// — the marker changes nothing for them; and the rename DIALOG's Save (htmx,
// no marker) still reloads the page, as it did.
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
	rename := pinPost(t, s, h, ui.SessionNamePath, inPlaceForm("Wiring the Stripe refund flow"), false)
	if rename.Code != http.StatusSeeOther || rename.Header().Get("Location") != back {
		t.Fatalf("rename without script = %d to %q, want 303 to %q", rename.Code, rename.Header().Get("Location"), back)
	}
	dialog := inPlaceForm("Payments")
	dialog.Del("drift")
	if rec := pinPost(t, s, h, ui.SessionNamePath, dialog, true); rec.Code != http.StatusOK || rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("the dialog's Save = %d (HX-Refresh %q), want the reload it has always had", rec.Code, rec.Header().Get("HX-Refresh"))
	}
}
