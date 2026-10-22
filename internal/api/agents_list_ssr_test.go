package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/recents"
	"github.com/ZacxDev/muster/internal/ui"
)

// agentsListOpen is #agents-list's opening tag in a document.
var agentsListOpen = regexp.MustCompile(`<div id="agents-list"[^>]*>`)

// listServer is the pins fixture (three agents, two mirrored windows, agent 6
// pinned, a recent entry) with a provisioner that counts its reads.
func listServer(t *testing.T) (*Server, http.Handler, *countingInstances) {
	t.Helper()
	mem := &memRecents{}
	mem.put(recents.Entry{AgentID: 5})
	mp := &memPins{}
	s, _ := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, mp)
	mp.pinFor(1).Agents[6] = true
	prov := &countingInstances{Provisioner: newFakeTerminal()}
	s.ext.Recents = mem
	s.ext.Provisioner = prov
	return s, s.Handler(), prov
}

// 🔴 ONE RULE, ONE RENDERER: the /agents document's #agents-list holds EXACTLY
// the /ui/agents partial's cards — byte for byte — for one state. The partial
// is that same body followed by the out-of-band "Recently viewed" strip, so the
// document's list is a prefix of it that ends where the strip begins.
//
// The fixture's times are fixed and months old, so the relative "active … ago"
// both requests print cannot tick between them.
func TestTheAgentsDocumentDrawsTheListPartial(t *testing.T) {
	s, h, _ := listServer(t)
	doc := getOwed(t, s, h, "/agents").Body.String()
	part := getOwed(t, s, h, "/ui/agents").Body.String()

	strip := strings.Index(part, `<nav id="agents-recent"`)
	if strip < 0 {
		t.Fatalf("control: the partial carries no out-of-band strip:\n%s", part)
	}
	cards := part[:strip]
	if n := strings.Count(cards, "<article "); n != 5 {
		t.Fatalf("control: the partial drew %d cards, want the fixture's 5:\n%s", n, cards)
	}
	open := agentsListOpen.FindStringIndex(doc)
	if open == nil {
		t.Fatalf("no #agents-list in the /agents document")
	}
	got := doc[open[1]:]
	if !strings.HasPrefix(got, cards+"</div>") {
		t.Errorf("the /agents document's #agents-list is not the /ui/agents partial's cards.\n--- document list (first 600 bytes):\n%.600s\n--- partial cards (first 600 bytes):\n%.600s", got, cards)
	}
}

// TestTheAgentsFirstPaintBuildsTheListOnce: the Agents tab's first paint is
// ONE list build. The document draws the cards (one provisioner read, for their
// live status) and its list issues no `load` request of its own; the document's
// first live refresh, carrying the version the document stamped, is answered
// 204 — no markup, nothing to swap. A refresh after the state moved gets the
// list, and the new version, again.
//
// On the base tree the document read no instances and drew no cards; every
// paint of the tab was empty until /ui/agents answered.
func TestTheAgentsFirstPaintBuildsTheListOnce(t *testing.T) {
	s, h, prov := listServer(t)
	doc := getOwed(t, s, h, "/agents").Body.String()
	if prov.n != 1 {
		t.Errorf("GET /agents read the provisioner's instances %d time(s), want exactly 1 (the list's one build)", prov.n)
	}
	open := agentsListOpen.FindString(doc)
	if open == "" {
		t.Fatal("no #agents-list in the /agents document")
	}
	if n := strings.Count(doc[strings.Index(doc, open):], "<article "); n != 5 {
		t.Errorf("the /agents document drew %d cards in its list, want the fixture's 5", n)
	}
	trig := regexp.MustCompile(`hx-trigger="([^"]*)"`).FindStringSubmatch(open)
	if trig == nil {
		t.Fatalf("the list has no hx-trigger: %s", open)
	}
	if strings.HasPrefix(trig[1], "load") || strings.Contains(trig[1], " load,") {
		t.Errorf("the server-rendered list still loads itself: hx-trigger=%q", trig[1])
	}
	for _, live := range []string{"sse:agent.changed from:body", "sse:chat.reply from:body", "muster:resync from:body", "agents:changed from:body"} {
		if !strings.Contains(trig[1], live) {
			t.Errorf("the list lost its live trigger %q: %q", live, trig[1])
		}
	}
	v := regexp.MustCompile(ui.AgentsListVersionAttr + `="([0-9a-f]+)"`).FindStringSubmatch(open)
	if v == nil {
		t.Fatalf("the list carries no %s: %s", ui.AgentsListVersionAttr, open)
	}

	refresh := func(version string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/ui/agents", nil)
		admit(s, req)
		if version != "" {
			req.Header.Set(ui.AgentsListVersionHeader, version)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := refresh(v[1]); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("the first live refresh with the document's version = %d (%d bytes), want 204 with nothing", rec.Code, rec.Body.Len())
	}
	// Control: an older version (or none) gets the list and the current version.
	for _, stale := range []string{"", "0123456789abcdef01234567"} {
		rec := refresh(stale)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<article ") {
			t.Errorf("a refresh with version %q = %d, want 200 with the cards", stale, rec.Code)
		}
		if got := rec.Header().Get(ui.AgentsListVersionHeader); got != v[1] {
			t.Errorf("a refresh with version %q answered version %q, want the current %q", stale, got, v[1])
		}
	}
	// The state moves — on EACH input the list is drawn from: the version
	// moves, and the version the client holds gets the list again.
	fm := s.ext.Mirror.(*fakeMirror)
	held := v[1]
	for _, step := range []struct {
		what string
		move func()
	}{
		{"an agent pinned", func() { s.ext.Pins.(*memPins).pinFor(1).Agents[5] = true }},
		{"a mirrored window active again", func() { fm.mu.Lock(); fm.windows[1].Activity += 600; fm.mu.Unlock() }},
		{"an entry viewed (the recents strip)", func() { s.ext.Recents.(*memRecents).put(recents.Entry{AgentID: 7}) }},
	} {
		step.move()
		rec := refresh(held)
		got := rec.Header().Get(ui.AgentsListVersionHeader)
		if rec.Code != http.StatusOK || got == held {
			t.Errorf("after %s, the held version answered %d with version %q — the change would never reach the list", step.what, rec.Code, got)
		}
		held = got
	}
}

// TestOtherTabsStillLoadTheListThemselves: a tab that is not Agents draws the
// panel hidden and EMPTY, and fills it with the list's own load — it does not
// pay for the list build on every page of the app.
func TestOtherTabsStillLoadTheListThemselves(t *testing.T) {
	s, h, prov := listServer(t)
	doc := getOwed(t, s, h, "/tasks").Body.String()
	if prov.n != 0 {
		t.Errorf("GET /tasks read the provisioner's instances %d time(s), want none", prov.n)
	}
	open := agentsListOpen.FindString(doc)
	if !strings.Contains(open, `hx-trigger="load, `) {
		t.Errorf("the /tasks document's hidden list does not load itself: %s", open)
	}
	if strings.Contains(open, ui.AgentsListVersionAttr) {
		t.Errorf("an empty list carries a version: %s", open)
	}
	rest := doc[strings.Index(doc, open)+len(open):]
	if strings.HasPrefix(rest, "<article") {
		t.Error("the /tasks document drew the list's cards")
	}
}
