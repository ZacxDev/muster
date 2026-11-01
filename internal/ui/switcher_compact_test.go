package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The compact bar (operator ask: "replace the labels for each view with an
// icon … Move the pin icon button into a three dots button context menu.
// Reduce padding …"). These tests name every id, class and string literally
// (never through the code's own constants), so they compile, and fail, against
// the bar before the change.

// cpParse is page as an HTML tree.
func cpParse(t *testing.T, page string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// cpFind is every element under n for which ok holds, in document order.
func cpFind(n *html.Node, ok func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.ElementNode && ok(c) {
			out = append(out, c)
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return out
}

// cpByID is the one element of id under n; nil when there is none.
func cpByID(t *testing.T, n *html.Node, id string) *html.Node {
	t.Helper()
	got := cpFind(n, func(e *html.Node) bool { v, _ := svAttr(e, "id"); return v == id })
	if len(got) > 1 {
		t.Fatalf("%d elements of id %q", len(got), id)
	}
	if len(got) == 0 {
		return nil
	}
	return got[0]
}

// cpText is n's text, its children's included.
func cpText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// cpClasses is n's class list as a set.
func cpClasses(n *html.Node) map[string]bool {
	v, _ := svAttr(n, "class")
	out := map[string]bool{}
	for _, c := range strings.Fields(v) {
		out[c] = true
	}
	return out
}

// cpInside reports whether n is a descendant of an element of id.
func cpInside(n *html.Node, id string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if v, _ := svAttr(p, "id"); v == id {
			return true
		}
	}
	return false
}

// TestTheViewButtonIsAnIconNamedForEachView: the view button draws NO text,
// only an icon per view — the shown one (Recent) visible, the others hidden —
// each a role=img named by the view's name (aria-label and title); the button
// itself is named for the shown view (aria-label and title). The four icons
// are four DIFFERENT drawings. The script swaps the icon, not a label. And
// the menu keeps each view's words, with that view's icon beside them. Red
// before: the button drew the word "Recent" and no per-view icon.
func TestTheViewButtonIsAnIconNamedForEachView(t *testing.T) {
	list, mirrors := switcherFixture()
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{AgentID: 1}), true))
	doc := cpParse(t, page)
	btn := cpByID(t, doc, "switcher-toggle")
	if btn == nil {
		t.Fatal("no view button")
	}
	if got := cpText(btn); got != "" {
		t.Errorf("the view button draws text %q; want an icon only", got)
	}
	for k, want := range map[string]string{"aria-label": "Showing Recent sessions; choose a view", "title": "Showing Recent sessions; choose a view"} {
		if v, _ := svAttr(btn, k); v != want {
			t.Errorf("the view button's %s = %q, want %q", k, v, want)
		}
	}
	icons := cpFind(btn, func(e *html.Node) bool { _, ok := svAttr(e, "data-switcher-icon"); return ok })
	want := []struct{ key, name string }{{"recent", "Recent"}, {"waiting", "Waiting"}, {"pins", "Pins"}, {"all", "All"}}
	if len(icons) != len(want) {
		t.Fatalf("the view button holds %d view icons, want %d", len(icons), len(want))
	}
	drawn := map[string]string{}
	for i, w := range want {
		ic := icons[i]
		key, _ := svAttr(ic, "data-switcher-icon")
		role, _ := svAttr(ic, "role")
		label, _ := svAttr(ic, "aria-label")
		title, _ := svAttr(ic, "title")
		_, hidden := svAttr(ic, "hidden")
		if key != w.key || role != "img" || label != w.name || title != w.name {
			t.Errorf("icon %d = key %q role %q aria-label %q title %q; want %s, img, %s, %s", i, key, role, label, title, w.key, w.name, w.name)
		}
		if hidden != (w.key != "recent") {
			t.Errorf("icon %s hidden = %v; only Recent's is shown at first", w.key, hidden)
		}
		svg := cpFind(ic, func(e *html.Node) bool { return e.Data == "svg" })
		if len(svg) != 1 {
			t.Fatalf("icon %s holds %d svgs", w.key, len(svg))
		}
		var b strings.Builder
		if err := html.Render(&b, svg[0]); err != nil {
			t.Fatal(err)
		}
		if other, dup := drawn[b.String()]; dup {
			t.Errorf("views %s and %s draw the same icon", other, w.key)
		}
		drawn[b.String()] = w.key
	}
	// The script swaps the icon by data-switcher-icon (before paint, with the
	// stored view) and names the button; it reads no label span.
	js := page[strings.Index(page, "<script>"):]
	if !strings.Contains(js, "[data-switcher-icon]") || strings.Contains(js, "data-switcher-label") {
		t.Error("the bar's script does not swap the icon (or still writes a text label)")
	}
	if strings.Contains(page, "data-switcher-label") {
		t.Error("the bar still draws a text label")
	}
	// The menu: each item its view's words AND its icon (the same drawing).
	menu := cpByID(t, doc, "switcher-views")
	for _, w := range []struct{ key, words string }{{"recent", "Recent"}, {"waiting", "Waiting on you"}, {"pins", "Pins"}, {"all", "All"}} {
		item := cpFind(menu, func(e *html.Node) bool { v, _ := svAttr(e, "data-switcher-pick"); return v == w.key })
		if len(item) != 1 {
			t.Fatalf("%d menu items for %s", len(item), w.key)
		}
		if !strings.HasPrefix(cpText(item[0]), w.words) {
			t.Errorf("menu item %s reads %q, want it to start %q", w.key, cpText(item[0]), w.words)
		}
		ic := cpFind(item[0], func(e *html.Node) bool { v, _ := svAttr(e, "data-switcher-menu-icon"); return v == w.key })
		if len(ic) != 1 {
			t.Errorf("menu item %s has no icon of its own", w.key)
			continue
		}
		var b strings.Builder
		_ = html.Render(&b, cpFind(ic[0], func(e *html.Node) bool { return e.Data == "svg" })[0])
		if drawn[b.String()] != w.key {
			t.Errorf("menu item %s's icon is not the one the button draws for it", w.key)
		}
	}
}

// TestThePinIsInTheMoreMenu: on a session's page the bar draws a ⋯ button — a
// menu button (popovertarget, aria-haspopup=menu, aria-expanded=false) — at
// its right end, AFTER the search, opening a popover role=menu that holds the
// pin's form: posting the subject's fields to pin (unpinned) or unpin
// (pinned), with view=switcher and the page to come back to, its button a
// menuitem with the one #switcher-pin id. Red before: no ⋯, the pin a button
// of its own in the bar.
func TestThePinIsInTheMoreMenu(t *testing.T) {
	list, mirrors := switcherFixture() // ag-two and mango:2 (@2) pinned
	for _, c := range []struct {
		name, action, pinned, words string
		cur                         SwitcherCurrent
		fields                      map[string]string
	}{
		{"an unpinned agent", PinPath, "false", "Pin this session", SwitcherCurrent{AgentID: 1},
			map[string]string{"kind": "agent", "agent_id": "1", "view": "switcher", "back": "/agents/ag-one"}},
		{"a pinned agent", UnpinPath, "true", "Unpin this session", SwitcherCurrent{AgentID: 2},
			map[string]string{"kind": "agent", "agent_id": "2", "view": "switcher", "back": "/agents/ag-two"}},
		{"a pinned window", UnpinPath, "true", "Unpin this session", SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@2"}},
			map[string]string{"kind": "mirror", "host": "tower", "session": "w", "window": "@2", "view": "switcher"}},
		{"an unpinned window on its Terminal view", PinPath, "false", "Pin this session", SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@1"}, Back: "/mirror/terminal?host=tower"},
			map[string]string{"kind": "mirror", "host": "tower", "session": "w", "window": "@1", "back": "/mirror/terminal?host=tower"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, c.cur), true))
			doc := cpParse(t, page)
			btn := cpByID(t, doc, "switcher-more-btn")
			if btn == nil {
				t.Fatal("no ⋯ button")
			}
			for k, want := range map[string]string{"type": "button", "popovertarget": "switcher-more", "aria-haspopup": "menu", "aria-expanded": "false", "aria-controls": "switcher-more", "aria-label": "More for this session"} {
				if v, _ := svAttr(btn, k); v != want {
					t.Errorf("the ⋯ button's %s = %q, want %q", k, v, want)
				}
			}
			if s, m := strings.Index(page, `id="switcher-search-open"`), strings.Index(page, `id="switcher-more-btn"`); s < 0 || m < s {
				t.Errorf("the ⋯ is not after the search (search %d, ⋯ %d)", s, m)
			}
			menu := cpByID(t, doc, "switcher-more")
			if menu == nil {
				t.Fatal("no ⋯ menu")
			}
			for k, want := range map[string]string{"popover": "", "role": "menu"} {
				if v, ok := svAttr(menu, k); !ok || v != want {
					t.Errorf("the ⋯ menu's %s = %q (%v), want %q", k, v, ok, want)
				}
			}
			forms := cpFind(doc, func(e *html.Node) bool { _, ok := svAttr(e, "data-switcher-pin"); return ok })
			if len(forms) != 1 || !cpInside(forms[0], "switcher-more") {
				t.Fatalf("%d pin forms, want one inside the ⋯ menu", len(forms))
			}
			f := forms[0]
			for k, want := range map[string]string{"method": "post", "action": c.action, "hx-post": c.action, "hx-target": "this", "hx-swap": "outerHTML"} {
				if v, _ := svAttr(f, k); v != want {
					t.Errorf("the pin form's %s = %q, want %q", k, v, want)
				}
			}
			got := map[string]string{}
			for _, in := range cpFind(f, func(e *html.Node) bool { return e.Data == "input" }) {
				n, _ := svAttr(in, "name")
				v, _ := svAttr(in, "value")
				got[n] = v
			}
			for k, v := range c.fields {
				if got[k] != v {
					t.Errorf("field %s = %q, want %q", k, got[k], v)
				}
			}
			item := cpByID(t, doc, "switcher-pin")
			if item == nil || !cpInside(item, "switcher-more") {
				t.Fatal("no #switcher-pin inside the ⋯ menu")
			}
			role, _ := svAttr(item, "role")
			typ, _ := svAttr(item, "type")
			pinned, _ := svAttr(item, "data-pinned")
			if role != "menuitem" || typ != "submit" || pinned != c.pinned || cpText(item) != c.words {
				t.Errorf("the pin item = role %q type %q data-pinned %q %q; want menuitem submit %s %q", role, typ, pinned, cpText(item), c.pinned, c.words)
			}
			if _, bad := svAttr(item, "aria-pressed"); bad {
				t.Error("a menuitem carries aria-pressed (not allowed on role=menuitem)")
			}
		})
	}
}

// TestNoStandalonePinInTheBar: the bar's row — what is drawn in the bar
// itself, outside its popovers — holds no pin form and no pin button; and a
// bar with no page session (or one not among the entries) draws no ⋯ and no
// pin at all. Red before: the pin form sat in the row, beside the search.
func TestNoStandalonePinInTheBar(t *testing.T) {
	list, mirrors := switcherFixture()
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{AgentID: 1}), true))
	doc := cpParse(t, page)
	inRow := cpFind(doc, func(e *html.Node) bool {
		_, form := svAttr(e, "data-switcher-pin")
		id, _ := svAttr(e, "id")
		return (form || id == "switcher-pin") && !cpInside(e, "switcher-more")
	})
	if len(inRow) != 0 {
		t.Errorf("%d pin elements drawn in the bar outside the ⋯ menu", len(inRow))
	}
	if cpByID(t, doc, "switcher-pin") == nil {
		t.Fatal("CONTROL: the page's session has no pin at all")
	}
	for name, cur := range map[string]SwitcherCurrent{"no page session": {}, "a session not listed": {AgentID: 99}} {
		none := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, cur), true))
		for _, banned := range []string{"data-switcher-pin", `id="switcher-pin"`, `id="switcher-more"`, `id="switcher-more-btn"`} {
			if strings.Contains(none, banned) {
				t.Errorf("%s: the bar draws %s", name, banned)
			}
		}
	}
}

// TestTheBarIsTight: every icon button in the bar — the view button, the
// search, the ⋯ — is a 44px-tall target (h-11) 40px wide (w-10) around a 32px
// face; the pinned bar is inset 4px a side, never less than the safe-area
// inset, with the safe-area inset at its foot; the row's gaps and each pill's
// padding are the small ones. Red before: a 4.5rem view button, 44px-wide
// buttons, a px-3 inset with no side insets, gap-2 / gap-1.5 / px-3.
func TestTheBarIsTight(t *testing.T) {
	list, mirrors := switcherFixture()
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{AgentID: 1}), true))
	doc := cpParse(t, page)
	for _, id := range []string{"switcher-toggle", "switcher-search-open", "switcher-more-btn"} {
		b := cpByID(t, doc, id)
		if b == nil {
			t.Errorf("no #%s", id)
			continue
		}
		cl := cpClasses(b)
		if !cl["h-11"] || !cl["w-10"] || !cl["shrink-0"] {
			t.Errorf("#%s is not a 44px-tall, 40px-wide target: %v", id, cl)
		}
		face := cpFind(b, func(e *html.Node) bool { return e.Data == "span" && cpClasses(e)["h-8"] && cpClasses(e)["w-8"] })
		if len(face) != 1 {
			t.Errorf("#%s has no 32px face", id)
		}
	}
	nav := cpByID(t, doc, "session-switcher")
	cl := cpClasses(nav)
	for _, want := range []string{"pl-[max(0.25rem,env(safe-area-inset-left))]", "pr-[max(0.25rem,env(safe-area-inset-right))]", "pb-[env(safe-area-inset-bottom)]", "h-[calc(2.8125rem+env(safe-area-inset-bottom))]"} {
		if !cl[want] {
			t.Errorf("the pinned bar lacks %s: %v", want, cl)
		}
	}
	if cl["px-3"] {
		t.Error("the pinned bar is still inset px-3")
	}
	row := cpFind(nav, func(e *html.Node) bool {
		c := cpClasses(e)
		return c["flex"] && c["h-11"] && c["min-w-0"] && c["items-center"]
	})
	if len(row) == 0 || !cpClasses(row[0])["gap-1"] {
		t.Error("the bar's row is not gap-1")
	}
	for _, r := range cpFind(nav, func(e *html.Node) bool { _, ok := svAttr(e, "data-switcher-list"); return ok }) {
		if c := cpClasses(r); !c["gap-1"] || !c["overflow-x-auto"] || !c["min-w-0"] {
			t.Errorf("a pill row is not a gap-1 sideways scroller: %v", c)
		}
	}
	pills := cpFind(nav, func(e *html.Node) bool { c := cpClasses(e); return e.Data == "span" && c["rounded-full"] && c["h-8"] })
	if len(pills) == 0 {
		t.Fatal("CONTROL: no pills drawn")
	}
	for _, p := range pills {
		if c := cpClasses(p); !c["px-2"] || c["px-3"] {
			t.Errorf("a pill is not px-2: %v", c)
		}
	}
	// The pills still never cut a label (the header's 🔴 rule).
	if !strings.Contains(page, `<span class="whitespace-nowrap">mango:1</span>`) {
		t.Error("a pill's label is no longer whole")
	}
}
