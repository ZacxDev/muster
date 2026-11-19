package pages_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// cssRule is one style rule: its selector list and its declarations.
type cssRule struct{ sel, body string }

// cssRules parses a stylesheet into its style rules, in order, descending into
// at-rule blocks (@media, @supports). Comments are dropped first. It is the
// small parser the assertions below need, not a CSS engine.
func cssRules(css string) []cssRule {
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	var out []cssRule
	var walk func(s string)
	walk = func(s string) {
		for {
			open := strings.IndexByte(s, '{')
			if open < 0 {
				return
			}
			sel := strings.TrimSpace(s[:open])
			if k := strings.LastIndexAny(sel, ";}"); k >= 0 {
				sel = strings.TrimSpace(sel[k+1:])
			}
			depth, end := 0, -1
			for i := open; i < len(s); i++ {
				if s[i] == '{' {
					depth++
				} else if s[i] == '}' {
					depth--
					if depth == 0 {
						end = i
						break
					}
				}
			}
			if end < 0 {
				return
			}
			body := s[open+1 : end]
			if strings.HasPrefix(sel, "@") {
				walk(body)
			} else {
				out = append(out, cssRule{sel, body})
			}
			s = s[end+1:]
		}
	}
	walk(css)
	return out
}

// contentDoc renders a markdown page the way internal/api does.
func contentDoc(t *testing.T, md string) string {
	t.Helper()
	return pages.Content(pages.Version{Title: "Theme", Version: 1, Blocks: []pages.Block{{Type: pages.BlockMarkdown, Text: md}}},
		pages.RenderOptions{Markdown: ui.MarkdownHTML})
}

// styleOf is the content document's one <style> element.
func styleOf(t *testing.T, doc string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindAllStringSubmatch(doc, -1)
	if len(m) != 1 {
		t.Fatalf("the content document has %d <style> elements, want 1", len(m))
	}
	return m[0][1]
}

// rootTokens resolves every --mu-* custom property on the document's root
// element with the `dark` class present or absent, by the cascade: the rules
// whose selector matches the root, the more specific one winning, then the
// later. It knows the three root selectors the palette is written with and
// fails on any other that declares a token, so a palette moved under a
// selector it cannot judge is a red test, not a silently skipped one.
func rootTokens(t *testing.T, css string, dark bool) map[string]string {
	t.Helper()
	type win struct {
		v         string
		spec, ord int
	}
	got := map[string]win{}
	decl := regexp.MustCompile(`(--mu-[a-z0-9-]+)\s*:\s*([^;]+)`)
	for ord, r := range cssRules(css) {
		if !strings.Contains(r.body, "--mu-") || !regexp.MustCompile(`(^|;)\s*--mu-`).MatchString(r.body) {
			continue
		}
		spec := -1
		for _, s := range strings.Split(r.sel, ",") {
			switch strings.TrimSpace(s) {
			case ":root":
				spec = max(spec, 10)
			case ".dark":
				if dark {
					spec = max(spec, 10)
				}
			case ":root:not(.dark)":
				if !dark {
					spec = max(spec, 20)
				}
			default:
				t.Fatalf("a rule declaring --mu-* tokens has a selector this test cannot resolve: %q", s)
			}
		}
		if spec < 0 {
			continue
		}
		for _, m := range decl.FindAllStringSubmatch(r.body, -1) {
			if w, ok := got[m[1]]; !ok || spec > w.spec || (spec == w.spec && ord >= w.ord) {
				got[m[1]] = win{strings.TrimSpace(m[2]), spec, ord}
			}
		}
	}
	out := map[string]string{}
	for k, w := range got {
		out[k] = w.v
	}
	return out
}

// TestTheContentDocumentIsTheAppsTheme: the page's content document is drawn
// in the app's palette and the app's theme, not a neutral one that follows
// the device.
//
// The expected values are the palette's own, copied by hand from
// web/css/tokens.css (the source web/static/app.css is built from), never
// read back from the document under test.
func TestTheContentDocumentIsTheAppsTheme(t *testing.T) {
	doc := contentDoc(t, "hello")
	css := styleOf(t, doc)

	// The app's one theme: every document it draws is <html class="dark">.
	if !strings.Contains(doc, `<html lang="en" class="dark">`) {
		t.Errorf("the content document's <html> does not carry the app's theme class: %.120q", doc)
	}
	// The device's own setting does not decide it.
	if strings.Contains(css, "prefers-color-scheme") {
		t.Error("the content document's colours follow the device's prefers-color-scheme, not the app's theme")
	}

	for _, c := range []struct {
		name string
		dark bool
		want map[string]string
	}{
		{"dark", true, map[string]string{"--mu-bg": "17 15 28", "--mu-text": "238 235 247", "--mu-accent": "228 181 86", "--mu-line": "47 42 72"}},
	} {
		got := rootTokens(t, css, c.dark)
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s selected: %s resolves to %q, want the app's %q", c.name, k, got[k], v)
			}
		}
	}

	// And the document's surfaces read those tokens: the body's ground and
	// text, the accent (buttons, links), a markdown link and a table's rule.
	for _, want := range []string{
		"--bg:rgb(var(--mu-bg))", "--fg:rgb(var(--mu-text))", "--accent:rgb(var(--mu-accent))",
		"html,body{margin:0;background:var(--bg);color:var(--fg);",
		":where(a){color:var(--accent)}",
		".mp-markdown .text-accent{--tw-text-opacity:1;color:rgb(var(--mu-accent)/var(--tw-text-opacity,1))}",
		".mp-markdown .border-line{--tw-border-opacity:1;border-color:rgb(var(--mu-line)/var(--tw-border-opacity,1))}",
		".mp-markdown .border{border-width:1px}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the content document's CSS lacks %q", want)
		}
	}
}

// markdownFixture exercises every construct ui.MarkdownHTML draws: each
// heading level, emphasis, inline code, both link shapes, an image's syntax
// (which it draws as text, not an <img>), both lists, a quote, a rule, a
// fenced block (plain and with a language) and a table.
const markdownFixture = "# One\n## Two\n### Three\n#### Four\n##### Five\n###### Six\n\n" +
	"Some **bold**, *italic*, _under_ and `inline code`.\n" +
	"A [link](https://example.com/a) and a bare https://example.com/b.\n" +
	"![an image](https://example.com/i.png)\n\n" +
	"- one\n- two\n\n1. first\n2. second\n\n> quoted\n> again\n\n---\n\n" +
	"```\nplain\n```\n\n```go\nx := 1\n```\n\n" +
	"| a | b |\n| --- | :-: |\n| `c` | [d](https://example.com/d) |\n"

// browserDefaultElements are the elements the renderer writes whose browser
// default IS their style (inline emphasis, line breaks, table row groups, a
// list item inside its styled list, a class-styled wrapper). Every OTHER
// element it writes must have a rule of its own. The set is asserted whole:
// an element added to it, or no longer written, fails.
var browserDefaultElements = []string{"br", "div", "em", "li", "span", "strong", "tbody", "thead", "tr"}

// cssIdent escapes a class name the way it is written in a selector (as
// Tailwind writes it): every character outside [A-Za-z0-9_-] backslashed.
func cssIdent(c string) string {
	var b strings.Builder
	for _, r := range c {
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteString(`\` + string(r))
		}
	}
	return b.String()
}

// classRuled reports whether some rule's selector scoped to a markdown block
// selects class c.
func classRuled(rules []cssRule, c string) bool {
	re := regexp.MustCompile(`\.` + regexp.QuoteMeta(cssIdent(c)) + `([^A-Za-z0-9_\\-]|$)`)
	for _, r := range rules {
		if strings.Contains(r.sel, ".mp-markdown") && re.MatchString(r.sel) {
			return true
		}
	}
	return false
}

// elementRuled reports whether some rule names element el as a type selector.
func elementRuled(rules []cssRule, el string) bool {
	re := regexp.MustCompile(`(^|[\s,>+~(])` + el + `($|[\s,>+~:.\[)])`)
	for _, r := range rules {
		if re.MatchString(r.sel) {
			return true
		}
	}
	return false
}

// TestEveryMarkdownClassHasARule: every class ui.MarkdownHTML writes has a
// rule in the content document's CSS, scoped to a markdown block, and every
// element it writes has a rule or is one of browserDefaultElements. A class
// added to the renderer without `make css` (which builds content_theme.css
// from markdown.go) fails here.
//
// It reads the classes twice: from a rendering of every construct
// (markdownFixture), and from markdown.go itself (every class="…" literal and
// every literal assigned to a variable a class attribute is built from), so a
// class in a branch the fixture does not reach is still checked.
func TestEveryMarkdownClassHasARule(t *testing.T) {
	out := ui.MarkdownHTML(markdownFixture)
	// The fixture still draws every construct: a fixture the renderer stopped
	// recognising would check nothing.
	for _, want := range []string{"<h1>", "<h2>", "<h3>", "<h4>", "<h5>", "<h6>", "<strong>", "<em>", "<code class=", "<a href=",
		"<ul class=", "<ol class=", "<blockquote class=", "<hr class=", "<details class=", "<summary class=", "<pre class=",
		"<table class=", "<th class=", "<td class=", "<br>", "<p class="} {
		if !strings.Contains(out, want) {
			t.Fatalf("control: the fixture's rendering lacks %q:\n%s", want, out)
		}
	}
	rules := cssRules(styleOf(t, contentDoc(t, markdownFixture)))

	classes := map[string]string{}
	for _, m := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(out, -1) {
		for _, c := range strings.Fields(m[1]) {
			classes[c] = "the rendered fixture"
		}
	}
	src, err := os.ReadFile("../ui/markdown.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile("class=\"([^\"`]*)").FindAllStringSubmatch(string(src), -1) {
		for _, c := range strings.Fields(m[1]) {
			if _, ok := classes[c]; !ok {
				classes[c] = "internal/ui/markdown.go"
			}
		}
	}
	// And a class written through a variable: a class="…` + v + `…" in the
	// source, and every string literal assigned to v (`size := "text-sm"`,
	// `size = "text-base"`). That one is the chat's heading branch, which
	// MarkdownHTML never takes (it draws <h1>…<h6>), so it cannot reach the
	// content document today; it is read anyway, so the day a branch that can
	// does the same, it is checked.
	viaVar := 0
	for _, m := range regexp.MustCompile("class=\"[^\"`]*` \\+ ([A-Za-z_][A-Za-z0-9_]*) \\+").FindAllStringSubmatch(string(src), -1) {
		for _, a := range regexp.MustCompile(`\b` + m[1] + `\s*:?=\s*"([^"]*)"`).FindAllStringSubmatch(string(src), -1) {
			for _, c := range strings.Fields(a[1]) {
				viaVar++
				if _, ok := classes[c]; !ok {
					classes[c] = "internal/ui/markdown.go, through `" + m[1] + "`"
				}
			}
		}
	}
	if viaVar == 0 {
		t.Fatal("control: no class written through a variable was found in markdown.go; the extraction is broken")
	}
	if len(classes) < 20 {
		t.Fatalf("control: only %d classes found; the extraction is broken: %v", len(classes), classes)
	}
	var names []string
	for c := range classes {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		if !classRuled(rules, c) {
			t.Errorf("markdown class %q (from %s) has no rule in the content document's CSS: run `make css` (content_theme.css is built from internal/ui/markdown.go)", c, classes[c])
		}
	}

	written := map[string]bool{}
	for _, m := range regexp.MustCompile(`<([a-z][a-z0-9]*)[\s>]`).FindAllStringSubmatch(out, -1) {
		written[m[1]] = true
	}
	def := map[string]bool{}
	for _, el := range browserDefaultElements {
		def[el] = true
		if !written[el] {
			t.Errorf("browserDefaultElements lists <%s>, which the renderer no longer writes: remove it", el)
		}
	}
	var els []string
	for el := range written {
		els = append(els, el)
	}
	sort.Strings(els)
	for _, el := range els {
		if !def[el] && !elementRuled(rules, el) {
			t.Errorf("markdown element <%s> has no rule in the content document's CSS (style it in contentCSS, or add it to browserDefaultElements if its browser default is right)", el)
		}
	}
}

// TestTheGuardsInstrumentsWork: the two predicates the drift guard rests on
// can say no, and can say yes.
func TestTheGuardsInstrumentsWork(t *testing.T) {
	rules := cssRules(`.mp-markdown .my-1{margin:0}.mp-markdown .text-\[0\.85em\]{font-size:.85em}.x .text-fg{color:red}` +
		`@media (min-width:1px){.mp-markdown :is(h1,h2){margin:0}}a{color:red}`)
	for _, c := range []struct {
		class string
		want  bool
	}{{"my-1", true}, {"my-10", false}, {"text-[0.85em]", true}, {"text-fg", false}, {"my", false}} {
		if got := classRuled(rules, c.class); got != c.want {
			t.Errorf("classRuled(%q) = %v, want %v", c.class, got, c.want)
		}
	}
	for _, c := range []struct {
		el   string
		want bool
	}{{"h1", true}, {"h2", true}, {"h3", false}, {"a", true}, {"td", false}} {
		if got := elementRuled(rules, c.el); got != c.want {
			t.Errorf("elementRuled(%q) = %v, want %v", c.el, got, c.want)
		}
	}
}

// resolveColour resolves expr's var() references against vars (a custom
// property map), repeatedly, and fails on one it cannot resolve.
func resolveColour(t *testing.T, vars map[string]string, expr string) string {
	t.Helper()
	re := regexp.MustCompile(`var\((--[a-z0-9-]+)\)`)
	for i := 0; i < 8 && re.MatchString(expr); i++ {
		expr = re.ReplaceAllStringFunc(expr, func(m string) string {
			v, ok := vars[re.FindStringSubmatch(m)[1]]
			if !ok {
				t.Fatalf("%s is not defined on the root", m)
			}
			return v
		})
	}
	return expr
}

// declOf is property prop's value in the first rule whose selector is sel.
func declOf(t *testing.T, rules []cssRule, sel, prop string) string {
	t.Helper()
	for _, r := range rules {
		if r.sel != sel {
			continue
		}
		for _, d := range strings.Split(r.body, ";") {
			if k, v, ok := strings.Cut(d, ":"); ok && strings.TrimSpace(k) == prop {
				return strings.TrimSpace(v)
			}
		}
	}
	t.Fatalf("no %s in a %q rule", prop, sel)
	return ""
}

// TestTheShellIsTheAppsTheme: the page's own top bar, its version menu and
// the owner's confirm bar are drawn in the app's palette (the same generated
// stylesheet the frame carries), not the device's system colours.
//
// The expected colours are the palette's, copied by hand from
// web/css/tokens.css, never read back from the shell.
func TestTheShellIsTheAppsTheme(t *testing.T) {
	out := pages.Shell(pages.ShellView{Title: "T", FrameSrc: "/c", Base: "/b", Nonce: "N",
		Owner: &pages.OwnerChrome{Version: 1, Versions: []pages.ShellVersion{{Version: 1, Current: true, Href: "/v1"}}, DetailHref: "/d"}})
	if !strings.Contains(out, `<html lang="en" class="dark">`) {
		t.Errorf("the shell's <html> does not carry the app's theme class: %.120q", out)
	}
	css := styleOf(t, out)
	for _, bad := range []string{"Canvas", "prefers-color-scheme", "color-scheme:light dark", "#2f6fde"} {
		if strings.Contains(css, bad) {
			t.Errorf("the shell's CSS still uses %q, not the app's palette", bad)
		}
	}
	rules := cssRules(css)
	for _, c := range []struct {
		name             string
		dark             bool
		bg, fg, line, s1 string
	}{
		{"dark", true, "rgb(17 15 28)", "rgb(238 235 247)", "rgb(47 42 72)", "rgb(25 22 42)"},
	} {
		vars := rootTokens(t, css, c.dark)
		for _, r := range rules {
			if r.sel == ":root" && strings.Contains(r.body, "--muted:") {
				for _, d := range strings.Split(r.body, ";") {
					if k, v, ok := strings.Cut(d, ":"); ok {
						vars[strings.TrimSpace(k)] = strings.TrimSpace(v)
					}
				}
			}
		}
		for _, chk := range []struct{ what, sel, prop, want string }{
			{"the page's ground", "html,body", "background", c.bg},
			{"the page's text", "html,body", "color", c.fg},
			{"the header's ground", "header", "background", c.bg},
			{"the header's rule", "header", "border-bottom", "1px solid " + c.line},
			{"the version menu's ground", ".pop", "background", c.s1},
			{"the confirm bar's ground", ".confirm", "background", c.s1},
			{"the confirm bar's rule", ".confirm", "border-top", "1px solid " + c.line},
		} {
			if got := resolveColour(t, vars, declOf(t, rules, chk.sel, chk.prop)); got != chk.want {
				t.Errorf("%s selected: %s (%s %s) = %q, want the app's %q", c.name, chk.what, chk.sel, chk.prop, got, chk.want)
			}
		}
	}
}
