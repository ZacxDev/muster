package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestBreadcrumbsMarkup: the trail is a nav named "Breadcrumb" holding an
// ordered list; every step but the last links back, the last is the current
// page (aria-current="page", no link), and the "›" between steps is hidden
// from a screen reader. Pinned whole: the markup is the contract.
func TestBreadcrumbsMarkup(t *testing.T) {
	got := nodeString(Breadcrumbs([]Breadcrumb{{Label: "Asks", Href: "/attention"}, {Label: "Ask #1001"}}))
	want := `<nav aria-label="Breadcrumb" data-breadcrumbs=""><ol class="flex min-w-0 flex-wrap items-center gap-1 text-sm text-fg2">` +
		`<li class="flex min-w-0 items-center gap-1"><a href="/attention" class="inline-flex min-h-[44px] items-center rounded px-1 font-semibold text-fg2 underline underline-offset-2 transition hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus" data-breadcrumb="">Asks</a></li>` +
		`<li class="flex min-w-0 items-center gap-1"><span aria-hidden="true">›</span><span aria-current="page" class="min-w-0 truncate px-1 text-fg">Ask #1001</span></li>` +
		`</ol></nav>`
	if got != want {
		t.Errorf("the breadcrumbs:\n got %s\nwant %s", got, want)
	}
	// The last step is the current page even when it is given a link.
	if c := nodeString(Breadcrumbs([]Breadcrumb{{Label: "Asks", Href: "/attention"}, {Label: "Here", Href: "/here"}})); strings.Contains(c, `href="/here"`) ||
		!strings.Contains(c, `<span aria-current="page" class="min-w-0 truncate px-1 text-fg">Here</span>`) {
		t.Errorf("the current page links to itself, or is not marked current:\n%s", c)
	}
	if Breadcrumbs(nil) != nil {
		t.Error("no steps drew a trail")
	}
}

// TestAnAppScreenDrawsItsBreadcrumbs: an app screen's breadcrumbs (the ask
// page's) are at the top of its content, before the page's own. RED at the
// base: a NavApp had no breadcrumbs (the component did not exist).
func TestAnAppScreenDrawsItsBreadcrumbs(t *testing.T) {
	v := sampleAppScreen()
	v.App.Crumbs = []Breadcrumb{{Label: "Asks", Href: "/attention"}, {Label: "Ask #7"}}
	var b bytes.Buffer
	if err := RenderNavDocument(&b, v); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	main := page[strings.Index(page, `id="app-main"`):]
	crumbs, content := strings.Index(main, `<nav aria-label="Breadcrumb"`), strings.Index(main, `data-testid="ask-page"`)
	if crumbs < 0 || content < crumbs {
		t.Errorf("the breadcrumbs are not at the top of the content (crumbs %d, content %d)", crumbs, content)
	}
	if !strings.Contains(main, `<a href="/attention" class="`+BreadcrumbLinkClass+`" data-breadcrumb="">Asks</a>`) ||
		!strings.Contains(main, `<span aria-current="page" class="`+BreadcrumbCurrentClass+`">Ask #7</span>`) {
		t.Errorf("the screen's breadcrumbs are not Asks › Ask #7:\n%s", main)
	}
	// Control: a screen with no crumbs draws no trail.
	b.Reset()
	if err := RenderNavDocument(&b, sampleAppScreen()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `aria-label="Breadcrumb"`) {
		t.Error("CONTROL: a screen with no crumbs drew a trail")
	}
}
