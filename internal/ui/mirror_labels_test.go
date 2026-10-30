package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// visibleText is markup's text nodes, concatenated: what a reader sees, with
// no attribute (a title= tooltip, a data-* value) in it.
func visibleText(t *testing.T, markup string) string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	return nodeText(doc)
}

// PRETTY NAMES on the mirrored surfaces: a host's label ("mango:1") is what a
// card and a page are TITLED by, and every link and socket keeps addressing the
// session by its NAME. A window's own PAGES keep the raw tmux session name
// visible beside the label; the Agents list's CARD does not (an operator ask:
// the resolved title only) — it keeps it as the name's tooltip and a search
// term.

func TestTheMirrorTerminalPageIsTitledByTheLabelWithTheRawName(t *testing.T) {
	html := renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "scratch2", DisplayName: "Gold:8", RawName: "scratch2",
		Badge: "Mirrored · laptop", BackHref: "/agents", WSPath: MirrorTerminalWSPath("laptop", "scratch2")}))
	for _, want := range []string{"<title>Terminal · Gold:8 · muster</title>", `>Gold:8`, `data-mirror-session="" class="ml-1.5 text-xs text-muted">scratch2</span>`, `title="tmux session scratch2"`} {
		if !strings.Contains(html, want) {
			t.Errorf("terminal page lacks %q", want)
		}
	}
	// Control: without a label (DisplayName/RawName empty) it is the raw name only.
	plain := renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "scratch2", Badge: "Mirrored · laptop", BackHref: "/agents",
		WSPath: MirrorTerminalWSPath("laptop", "scratch2")}))
	if !strings.Contains(plain, "<title>Terminal · scratch2 · muster</title>") || strings.Contains(plain, "data-mirror-session") {
		t.Errorf("an unlabelled terminal page is not titled by its raw name alone")
	}
}

func TestTheRenderedViewIsTitledByTheLabelAndAddressedByTheSession(t *testing.T) {
	html := renderNode(t, MirrorViewPage(MirrorViewView{Host: "laptop", Session: "scratch13", Label: "mango:1"}))
	for _, want := range []string{"<title>mango:1 · Mirrored · muster</title>", `<span data-mirror-label="">mango:1</span>`,
		`data-mirror-session="" class="ml-1.5 text-xs text-muted">scratch13</span>`, `data-ws="` + strings.ReplaceAll(MirrorViewWSPath("laptop", "scratch13"), "&", "&amp;") + `"`} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered view lacks %q", want)
		}
	}
	plain := renderNode(t, MirrorViewPage(MirrorViewView{Host: "laptop", Session: "scratch13"}))
	if !strings.Contains(plain, "<title>scratch13 · Mirrored · muster</title>") || strings.Contains(plain, "data-mirror-label") {
		t.Errorf("an unlabelled rendered view is not titled by its raw name alone")
	}
}

// THE HOST BADGE IS NOT THE GREY CHIP: on both pages it is the tinted, bold
// mirrorBadge (with its screen icon), so the host stands out on a phone among
// the kind chips. (The CARD carries an icon instead —
// TestTheMirroredMarkIsAnIconWithAnAccessibleTooltip.)
func TestTheMirroredHostBadgeIsDistinctFromTheGenericChip(t *testing.T) {
	const tint = "bg-st-progress-bg"
	grey := renderNode(t, chip("kind", "x"))
	if strings.Contains(grey, tint) {
		t.Fatal("CONTROL: the generic chip already carries the tint — this test cannot tell them apart")
	}
	badge := func(html, attr string) string {
		i := strings.Index(html, attr)
		if i < 0 {
			t.Fatalf("no %s in:\n%s", attr, html)
		}
		rest := html[i:]
		return rest[:strings.Index(rest, "</span></span>")]
	}
	for name, got := range map[string]string{
		"view": badge(renderNode(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "s"})), "data-term-badge"),
		"term": badge(renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "s", Badge: "Mirrored · tower", BackHref: "/agents"})), "data-term-badge"),
	} {
		for _, want := range []string{tint, "font-semibold", "<svg", "Mirrored · tower"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s badge lacks %q: %s", name, want, got)
			}
		}
		if strings.Contains(got, `<span class="text-muted">kind</span>`) {
			t.Errorf("%s badge is still the generic kind chip: %s", name, got)
		}
	}
}

// A mirrored window with no rendered view says why in ONE line under the
// header — its reason, escaped, after "Terminal only." — and a window that has
// a rendered view draws the Rendered | Terminal switch and no such line, even
// if a reason was set. Red at origin/main: the page had no line at all.
func TestTheTerminalViewSaysWhyItIsTerminalOnly(t *testing.T) {
	const why = "muster found no Claude Code conversation in <this> window."
	html := renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "work", Badge: "Mirrored · laptop", BackHref: "/agents",
		WSPath: MirrorTerminalWSPath("laptop", "work"), TerminalOnly: why}))
	want := `<p id="term-only" data-terminal-only="" class="border-b border-line bg-s1 px-3 py-1.5 text-xs text-fg2 break-words"><span class="font-semibold text-fg">Terminal only.</span> <span data-terminal-only-reason="">muster found no Claude Code conversation in &lt;this&gt; window.</span></p>`
	if !strings.Contains(html, want) {
		t.Errorf("terminal page lacks the terminal-only line %s", want)
	}
	if strings.Count(html, `data-terminal-only=""`) != 1 {
		t.Errorf("want exactly one terminal-only line")
	}
	both := renderNode(t, AgentTerminalPage(AgentTerminalView{Name: "work", Badge: "Mirrored · laptop", BackHref: "/agents",
		WSPath: MirrorTerminalWSPath("laptop", "work"), RenderedHref: MirrorViewPath("laptop", "work"), TerminalOnly: why}))
	if strings.Contains(both, "data-terminal-only") || !strings.Contains(both, `id="view-rendered"`) {
		t.Errorf("a window with a rendered view must draw the switch and no terminal-only line")
	}
}
