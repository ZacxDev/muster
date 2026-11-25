package pages_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/theme"
	"github.com/ZacxDev/muster/internal/ui"
)

// shortNames adds the document's own `:root{--fg:...}` short names (the rule
// that declares --accent) to vars, as the cascade would.
func shortNames(rules []cssRule, vars map[string]string) map[string]string {
	for _, r := range rules {
		if r.sel == ":root" && strings.Contains(r.body, "--accent:") {
			for _, d := range strings.Split(r.body, ";") {
				if k, v, ok := strings.Cut(d, ":"); ok {
					vars[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
		}
	}
	return vars
}

func themeBlockFor(t *testing.T, preset, accent string) string {
	t.Helper()
	tok, err := theme.Choice{Preset: preset, Accent: accent}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	return theme.CSS(tok)
}

// TestTheContentAndTheShellAreDrawnInTheInstancesTheme: with the instance's
// theme block inlined (RenderOptions.ThemeCSS, ShellView.ThemeCSS), the
// tokens a page's own HTML is told to use resolve to the INSTANCE's accent —
// two different accents, so no hard-coded value can pass both — while the
// ground stays the theme's dark one. With none (the default theme), the
// document is the app's own palette: the negative control.
//
// The expected values are written by hand: #5AA9F0 is 90 169 240, #7FD0FF
// is 127 208 255, brass (the default) is 228 181 86.
func TestTheContentAndTheShellAreDrawnInTheInstancesTheme(t *testing.T) {
	v := pages.Version{Title: "Theme", Version: 1, Blocks: []pages.Block{
		{Type: pages.BlockMarkdown, Text: "hello"}, {Type: pages.BlockHTML, HTML: `<div class="card">x</div>`}}}
	docs := map[string]string{}
	for _, c := range []struct {
		name, css, accent string
	}{
		{"harbor #5aa9f0", themeBlockFor(t, "harbor", "#5aa9f0"), "rgb(90 169 240)"},
		{"ember #7fd0ff", themeBlockFor(t, "ember", "#7fd0ff"), "rgb(127 208 255)"},
		{"the default theme", "", "rgb(228 181 86)"},
	} {
		doc := pages.Content(v, pages.RenderOptions{Markdown: ui.MarkdownHTML, ThemeCSS: c.css})
		docs[c.name] = doc
		css := styleOf(t, doc)
		if c.css != "" && !strings.Contains(css, c.css) {
			t.Errorf("%s: the content's <style> does not inline the theme block", c.name)
		}
		rules := cssRules(css)
		vars := shortNames(rules, rootTokens(t, css, true))
		for _, chk := range []struct{ what, expr, want string }{
			{"--accent", "var(--accent)", c.accent},
			{"a button", declOf(t, rules, "button", "background"), c.accent},
			{"--bg", "var(--bg)", "rgb(17 15 28)"},
			{"the page's ground", declOf(t, rules, "html,body", "background"), "rgb(17 15 28)"},
			{"--s1", "var(--s1)", "rgb(25 22 42)"},
		} {
			if got := resolveColour(t, vars, chk.expr); got != chk.want {
				t.Errorf("content, %s: %s = %q, want %q", c.name, chk.what, got, chk.want)
			}
		}

		sh := styleOf(t, pages.Shell(pages.ShellView{Title: "T", FrameSrc: "/c", Base: "/b", Nonce: "N", ThemeCSS: c.css}))
		svars := shortNames(cssRules(sh), rootTokens(t, sh, true))
		if got := resolveColour(t, svars, "var(--accent)"); got != c.accent {
			t.Errorf("shell, %s: --accent = %q, want %q", c.name, got, c.accent)
		}
	}
	if docs["harbor #5aa9f0"] == docs["the default theme"] || docs["ember #7fd0ff"] == docs["harbor #5aa9f0"] {
		t.Error("a themed content document is byte-identical to another theme's")
	}
}

// TestTheContentIsFullWidth: .mp, which holds every block, has no max-width:
// it is the frame's width less its 16px side gutters, with room for the
// shell's header at the top.
func TestTheContentIsFullWidth(t *testing.T) {
	rules := cssRules(styleOf(t, contentDoc(t, "hello")))
	for _, r := range rules {
		if strings.Contains(r.body, "max-width") && strings.Contains(r.sel, ".mp") && !strings.Contains(r.sel, "mp-") {
			t.Errorf("%s{%s} caps the content's width", r.sel, r.body)
		}
	}
	if got := declOf(t, rules, ".mp", "padding"); got != "56px 16px 16px" {
		t.Errorf(".mp padding = %q, want 56px 16px 16px", got)
	}
}
