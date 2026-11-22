package ui

import (
	"regexp"
	"strings"
	"testing"
)

// THE ONE NAV LIST (nav_pages.go), read by the sidebar and the palette. These
// tests read only the rendered document (sidebarIn) — the sidebar's More links
// and the palette's template of screens — so they compile on the commit before
// the list, and are RED there by assertion: the palette had no screens.

// sidebarMoreHrefs is the hrefs of the links in the sidebar's More section, in
// order, less a feature-off hint (it links to the Features page's sheet, not
// to a page of the app).
func sidebarMoreHrefs(t *testing.T, h string) []string {
	t.Helper()
	i := strings.Index(h, `id="`+navMoreID+`-body"`)
	j := strings.Index(h, `</details>`)
	if i < 0 || j < i {
		t.Fatal("the sidebar draws no More section")
	}
	var out []string
	for _, tag := range regexp.MustCompile(`<a [^>]*>`).FindAllString(h[i:j], -1) {
		if strings.Contains(tag, "data-feature-off=") {
			continue
		}
		out = append(out, regexp.MustCompile(`href="([^"]*)"`).FindStringSubmatch(tag)[1])
	}
	return out
}

// paletteScreenHrefs is the hrefs of the screens the palette searches, in
// order, and the words each is found by ("label|words").
func paletteScreenHrefs(h string) (hrefs, found []string) {
	i := strings.Index(h, `<template data-palette-pages="">`)
	if i < 0 {
		return nil, nil
	}
	j := strings.Index(h[i:], `</template>`)
	for _, m := range regexp.MustCompile(`<a href="([^"]*)" data-palette-page="[^"]*" data-palette-icon="[^"]*" data-palette-words="([^"]*)">([^<]*)</a>`).FindAllStringSubmatch(h[i:i+j], -1) {
		hrefs = append(hrefs, m[1])
		found = append(found, m[3]+"|"+m[2])
	}
	return hrefs, found
}

// TestThePaletteSearchesExactlyTheSidebarsPages: for every feature set the
// sidebar golden covers, the screens the palette searches are the sidebar's
// More links — the same set in the same order, so a page added to (or gated
// out of) one is added to (or gated out of) the other. Fails when either side
// GROWS or SHRINKS.
func TestThePaletteSearchesExactlyTheSidebarsPages(t *testing.T) {
	for _, c := range sidebarGoldenCases {
		h := renderString(t, sidebarIn(c.active, c.current, c.hasPanels, c.feat, c.overlay))
		side := sidebarMoreHrefs(t, h)
		pal, _ := paletteScreenHrefs(h)
		if len(side) < 3 {
			t.Fatalf("%s: the sidebar lists %v (instrument: too few to compare)", c.name, side)
		}
		if strings.Join(pal, " ") != strings.Join(side, " ") {
			t.Errorf("%s: the palette searches %v, the sidebar draws %v", c.name, pal, side)
		}
	}
}

// TestAGatedPageIsNeitherDrawnNorFound: a member with every feature off is
// drawn — and the palette searches — only the tasks and their account pages;
// an admin with every feature on, every page, Admin among them. Literal lists.
func TestAGatedPageIsNeitherDrawnNorFound(t *testing.T) {
	for _, c := range []struct {
		name string
		feat Features
		want string
	}{
		{"member, everything off", Features{Setup: false}, "/tasks /account /account/features"},
		{"member, everything on", Features{Connectors: true, Attention: true, Pages: true, Officers: true, Apps: []AppTab{{ID: 4, Label: "Board", Icon: "B"}}},
			"/tasks /connectors /attention /pages /officers /apps/4 /account /account/features"},
		{"admin, everything on", Features{Admin: true, Setup: true, Connectors: true, Attention: true, Pages: true, Officers: true},
			"/tasks /connectors /attention /pages /officers /admin/users /account /account/features"},
	} {
		h := renderString(t, sidebarIn("", "", false, c.feat, false))
		side := strings.Join(sidebarMoreHrefs(t, h), " ")
		hrefs, _ := paletteScreenHrefs(h)
		pal := strings.Join(hrefs, " ")
		if side != c.want {
			t.Errorf("%s: the sidebar draws %q, want %q", c.name, side, c.want)
		}
		if pal != c.want {
			t.Errorf("%s: the palette searches %q, want %q", c.name, pal, c.want)
		}
	}
}

// TestTheScreensAreFoundByTheirRouteWords: what each screen is found by — its
// label (the template's text) and its own words — as the palette reads them.
func TestTheScreensAreFoundByTheirRouteWords(t *testing.T) {
	h := renderString(t, sidebarIn("", "", false, Features{Admin: true, Setup: true, Connectors: true, Attention: true, Pages: true, Officers: true}, false))
	_, found := paletteScreenHrefs(h)
	want := []string{
		"Tasks|board todo",
		"Connectors|integrations credentials",
		"Asks|attention questions answers drafts",
		"Pages|docs documents shared links",
		"Officers|schedules",
		"Admin|users settings",
		"Account|settings profile sign out",
		"Features|settings",
	}
	if strings.Join(found, "\n") != strings.Join(want, "\n") {
		t.Errorf("the screens are found by:\n%s\nwant:\n%s", strings.Join(found, "\n"), strings.Join(want, "\n"))
	}
}
