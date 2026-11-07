package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
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
