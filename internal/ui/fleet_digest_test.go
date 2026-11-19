package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/fleet"
)

// TestTheSkeletonNamesTheFleetPackagesGroups: the shell's skeleton draws the
// groups internal/fleet builds, in its order and with its labels — the card's
// first paint and the fetched card cannot name different groups.
func TestTheSkeletonNamesTheFleetPackagesGroups(t *testing.T) {
	if !slices.Equal(fleetGroupKeys, fleet.GroupOrder) {
		t.Fatalf("skeleton groups %v; fleet.GroupOrder %v", fleetGroupKeys, fleet.GroupOrder)
	}
	for _, k := range fleet.GroupOrder {
		if fleetGroupLabels[k] != fleet.GroupLabel[k] || fleetGroupLabels[k] == "" {
			t.Errorf("group %s: skeleton %q, fleet %q", k, fleetGroupLabels[k], fleet.GroupLabel[k])
		}
	}
}

// TestThePanelPinsTheDigestAboveTheChat: the shell's panel holds the card's
// slot (the skeleton, state loading) BEFORE the chat body, outside it — a
// thread switch swaps the body and leaves the card.
func TestThePanelPinsTheDigestAboveTheChat(t *testing.T) {
	if off := renderString(t, ChiefPanel(false)); strings.Contains(off, `id="`+fleetDigestSlotID+`"`) || strings.Contains(off, "data-fleet-") {
		t.Fatalf("MUSTER_FLEET_DIGEST off: the panel draws the digest slot:\n%s", off)
	}
	html := renderString(t, ChiefPanel(true))
	slot := strings.Index(html, `id="`+fleetDigestSlotID+`"`)
	body := strings.Index(html, `id="`+chiefPanelBodyID+`"`)
	if slot < 0 || body < 0 || slot > body {
		t.Fatalf("the digest slot (%d) must come before the chat body (%d):\n%s", slot, body, html)
	}
	if !strings.Contains(html[slot:body], fleetDigestStateAttr+`="`+FleetStateLoading+`"`) {
		t.Errorf("the slot does not hold the skeleton")
	}
	// The slot closes before the body opens: it is a sibling, not a parent.
	if strings.Count(html[slot:body], "<div") != strings.Count(html[slot:body], "</div>") {
		t.Errorf("the digest slot is not closed before the chat body: the body would be inside it")
	}
}

// TestTheCardsStatesAndAnUnavailableCard: a card with rows names its state
// ready and its rows by fleet id; an unavailable one says why and draws no
// group; a panel body with a draft carries it for the script, and one without
// carries none.
func TestTheCardsStatesAndAnUnavailableCard(t *testing.T) {
	v := FleetDigestView{Total: 2, Since: "Since you last looked (5m ago): 1 new stop", Groups: []FleetGroupView{
		{Key: "needs_you", Label: "Needs you", Asks: []FleetAskView{{ID: 9, Question: "Ship it?", Href: "/attention/asks/9"}}},
		{Key: "waiting_ci", Label: "Waiting on checks"}, {Key: "working", Label: "Working"},
		{Key: "done", Label: "Done", Rows: []FleetRowView{{ID: "mirror:abc", Name: "Fix the loader", State: "stalled", Detail: "stalled 2h"}}},
		{Key: "stalled", Label: "Stalled"},
	}}
	var b strings.Builder
	if err := RenderFleetDigest(&b, v); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{`data-fleet-state="ready"`, `data-fleet-row="mirror:abc"`, `data-fleet-ask="9"`, `data-fleet-chip="done"`,
		`data-fleet-sheet="done"`, `hx-get="/ui/chief/fleet/session?id=mirror%3Aabc"`, `Since you last looked (5m ago): 1 new stop`, ">2 sessions<"} {
		if !strings.Contains(html, want) {
			t.Errorf("the card lacks %s", want)
		}
	}
	b.Reset()
	if err := RenderFleetDigest(&b, FleetDigestView{Unavailable: "no states here"}); err != nil {
		t.Fatal(err)
	}
	if u := b.String(); !strings.Contains(u, `data-fleet-state="unavailable"`) || !strings.Contains(u, "no states here") || strings.Contains(u, "data-fleet-chip") {
		t.Errorf("unavailable card: %s", u)
	}
	withDraft := renderString(t, chiefPanelBody(ChiefPanelView{Configured: true, Prefill: "About mirror:abc: "}))
	if !strings.Contains(withDraft, ChiefPrefillAttr+`="About mirror:abc: "`) {
		t.Errorf("the body does not carry the draft")
	}
	if none := renderString(t, chiefPanelBody(ChiefPanelView{Configured: true})); strings.Contains(none, ChiefPrefillAttr) {
		t.Errorf("a body with no draft carries the mark")
	}
}

// TestTheFleetCardLoadsOnItsOwnEvent: the card's slot loads on fleetOpenEvent
// — never on the body's muster:chief-open, which restore() re-dispatches on
// every settle while the body is unloaded (audit round 1 🟡2) — and the panel
// script dispatches exactly that name. The browser fact (one fetch on a
// restored-open load) is e2e/tests/chief-fleet-digest.spec.ts.
func TestTheFleetCardLoadsOnItsOwnEvent(t *testing.T) {
	html := renderString(t, fleetDigestSlot())
	if !strings.Contains(html, `hx-trigger="`+fleetOpenEvent+` from:document,`) {
		t.Errorf("the slot does not load on %s:\n%s", fleetOpenEvent, html)
	}
	if strings.Contains(html, chiefPanelOpenEvent) {
		t.Errorf("the slot listens to the body's %s", chiefPanelOpenEvent)
	}
	if src := jsSource(t, chiefPanelScript()); !strings.Contains(src, `var FLEET_OPEN_EVENT = '`+fleetOpenEvent+`'`) {
		t.Errorf("the panel script does not dispatch %s", fleetOpenEvent)
	}
}
