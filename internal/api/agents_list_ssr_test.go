package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/recents"
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
// live status) and its list issues no `load` request of its own — every live
// trigger is kept.
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
	rest := doc[strings.Index(doc, open)+len(open):]
	if strings.HasPrefix(rest, "<article") {
		t.Error("the /tasks document drew the list's cards")
	}
}
