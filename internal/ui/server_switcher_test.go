package ui

import (
	"os"
	"strings"
	"testing"
)

// TestTheSignInPageKeepsTheHandoffUnderTheSwitchersKey: the sign-in page's
// keep script (ServerHandoffKeepHTML) writes the sessionStorage key that
// web/static/servers.js reads (HANDOFF_KEY), in the "<ms>|<hash>" form its
// kept() parses — the two halves of one hand-off, in two languages.
func TestTheSignInPageKeepsTheHandoffUnderTheSwitchersKey(t *testing.T) {
	src, err := os.ReadFile("../../web/static/servers.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "var HANDOFF_KEY = '"+ServerHandoffKey+"';") {
		t.Fatalf("servers.js does not read the key the sign-in page writes (%q)", ServerHandoffKey)
	}
	keep := ServerHandoffKeepHTML()
	for _, want := range []string{"sessionStorage.setItem('" + ServerHandoffKey + "',", "Date.now()+'|'+location.hash", "/^#muster-from=/"} {
		if !strings.Contains(keep, want) {
			t.Errorf("the keep script lacks %q: %s", want, keep)
		}
	}
}

// TestTheServerRowIsAnnouncedOnce: the row's button is named by its own text
// ("Server in view:" and the label) and carries no aria-describedby pointing
// back into itself, which read the hint twice.
func TestTheServerRowIsAnnouncedOnce(t *testing.T) {
	page := renderString(t, serverSwitcher(Brand{Name: "Team Example"}))
	start := strings.Index(page, `<button id="server-switcher-toggle"`)
	if start < 0 {
		t.Fatal("no row button")
	}
	tag := page[start : start+strings.Index(page[start:], ">")]
	if strings.Contains(tag, "aria-describedby") {
		t.Errorf("the row button has a description: %s", tag)
	}
	if strings.Count(page, "Server in view:") != 1 {
		t.Errorf("the hint appears %d times", strings.Count(page, "Server in view:"))
	}
}

// TestTheOfferClaimsNoOrigin: the hand-off offer says a LINK asked, never "you
// came from", and starts its focus on Not now.
func TestTheOfferClaimsNoOrigin(t *testing.T) {
	page := renderString(t, serverSwitcherDialogs())
	i := strings.Index(page, `id="server-offer"`)
	if i < 0 {
		t.Fatal("no offer dialog")
	}
	offer := page[i:]
	offer = offer[:strings.Index(offer, "</dialog>")]
	for _, banned := range []string{"came from", "switched here from", "switch back"} {
		if strings.Contains(strings.ToLower(offer), banned) {
			t.Errorf("the offer claims where you came from (%q)", banned)
		}
	}
	if !strings.Contains(offer, "A link asked") || !strings.Contains(offer, "data-server-origin-text") {
		t.Errorf("the offer lacks the neutral copy or the origin slot: %s", offer)
	}
	if !strings.Contains(offer, `id="server-offer-dismiss"`) || !strings.Contains(offer[strings.Index(offer, `id="server-offer-dismiss"`):], "autofocus") {
		t.Errorf("Not now does not take the focus: %s", offer)
	}
}

// TestTheServerRowsShowANameAndNoAddressLine (jargon audit F028): neither the
// switcher's menu row nor a Manage servers row has a second line for the
// server's address ([data-server-host], which servers.js filled with
// host:port); a Manage row carries the address ⓘ instead — the shared
// InfoSheet, with a Copy and an (empty, script-filled) code row — and the
// menu row, a link, carries none (a button inside a link is invalid).
func TestTheServerRowsShowANameAndNoAddressLine(t *testing.T) {
	menu := renderString(t, serverSwitcher(Brand{Name: "Team Example"}))
	dialogs := renderString(t, serverSwitcherDialogs())
	for what, page := range map[string]string{"menu": menu, "dialogs": dialogs} {
		if strings.Contains(page, "data-server-host") {
			t.Errorf("%s: a row still has an address line (data-server-host)", what)
		}
	}
	i := strings.Index(dialogs, `id="server-manage-row-tpl"`)
	if i < 0 {
		t.Fatal("no Manage row template")
	}
	row := dialogs[i:]
	row = row[:strings.Index(row, "</template>")]
	want := InfoSheetHTML(serverAddressSheet())
	if !strings.Contains(row, want) {
		t.Errorf("the Manage row lacks the address sheet:\n%s", row)
	}
	for _, part := range []string{`data-info-code=""`, `data-info-copy=""`, `popovertarget="` + ServerAddressSheetTpl + `"`} {
		if !strings.Contains(row, part) {
			t.Errorf("the Manage row's sheet lacks %s", part)
		}
	}
	j := strings.Index(menu, `id="server-item-tpl"`)
	item := menu[j:]
	item = item[:strings.Index(item, "</template>")]
	if strings.Contains(item, "data-info-open") {
		t.Errorf("the menu row (a link) carries an ⓘ button: %s", item)
	}
}

// TestTheManageRowsAddressSheetIsTheOneServersJSRenames: servers.js renames
// the row template's sheet by the id the template draws it with.
func TestTheManageRowsAddressSheetIsTheOneServersJSRenames(t *testing.T) {
	src, err := os.ReadFile("../../web/static/servers.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "var ADDRESS_TPL = '"+ServerAddressSheetTpl+"';") {
		t.Fatalf("servers.js does not rename the sheet the template draws (%q)", ServerAddressSheetTpl)
	}
}
