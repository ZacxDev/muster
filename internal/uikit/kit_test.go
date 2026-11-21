package uikit

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/theme"
	"github.com/ZacxDev/muster/web"
)

// ---------------------------------------------------------------------------
// THE CONTRACT LEDGER (docs/ui-kit.md). Each list is what the kit promises:
// the kit must carry every entry (TestKitCSSCarriesTheContract,
// TestIconsSVGCarriesTheContract, TestKitJSCarriesTheContract), the ledger
// equals what the contract document lists, both ways
// (TestTheLedgerIsTheContract), and — v1 being additive only — nothing in the
// frozen v1 baseline (testdata/v1-baseline.txt) may leave the ledger
// (TestNothingInV1WasRemoved). Adding to v1: add it to the ledger, the doc
// and the kit; the baseline is not touched.
// ---------------------------------------------------------------------------

var ledgerTokens = func() []string {
	out := theme.ColourNames()
	for _, f := range theme.Fixed {
		out = append(out, f.Name)
	}
	return out
}()

var ledgerClasses = []string{
	"mu-root", "mu-icon", "mu-visually-hidden",
	"mu-btn", "mu-btn--primary", "mu-btn--secondary", "mu-btn--ghost", "mu-btn--danger", "mu-btn--sm", "mu-btn--block",
	"mu-icon-btn", "mu-icon-btn--danger", "mu-icon-btn--lg", "mu-toggle", "mu-toggle--alert", "mu-toolbar",
	"mu-chip", "mu-chip--open", "mu-chip--progress", "mu-chip--review", "mu-chip--complete", "mu-chip--error", "mu-chip--warning", "mu-chip--running",
	"mu-badge", "mu-badge--accent", "mu-badge--danger",
	"mu-card", "mu-card--raised", "mu-card__title", "mu-card__body",
	"mu-backdrop", "mu-sheet", "mu-sheet__header", "mu-sheet__body", "mu-sheet__footer",
	"mu-menu", "mu-menu__item", "mu-menu__item--danger", "mu-menu__sep",
	"mu-avatar", "mu-avatar--sm", "mu-avatar--lg", "mu-tile", "mu-tile--speaking", "mu-tile__label", "mu-tile__status", "mu-tile-grid",
	"mu-toast-region", "mu-toast", "mu-toast--success", "mu-toast--warning", "mu-toast--error",
	"mu-spinner", "mu-spinner--lg",
	"mu-field", "mu-label", "mu-input", "mu-select", "mu-textarea", "mu-check", "mu-switch", "mu-help", "mu-error",
	"mu-tooltip",
}

var ledgerIcons = []string{
	"mic", "mic-off", "camera", "camera-off", "screen-share", "screen-share-stop", "leave", "phone",
	"people", "chat", "transcript", "captions", "settings", "more", "more-vertical", "copy", "link", "share",
	"hand", "check", "close", "warning", "info", "error",
	"signal-0", "signal-1", "signal-2", "signal-3", "signal-4",
	"record", "stop", "pin", "fullscreen", "fullscreen-exit", "volume", "volume-off", "send", "plus", "minus", "search",
	"chevron-up", "chevron-down", "chevron-left", "chevron-right", "external",
}

var ledgerJS = []string{"version", "ready", "icon", "tooltip", "toggle", "trapFocus", "openSheet", "closeSheet", "toast", "scan"}

var ledgerAttrs = []string{"data-mu-toggle", "data-mu-tooltip", "data-mu-menu", "data-mu-sheet", "data-mu-close"}

// ledgerEntries is the ledger as "<kind> <name>" lines, the baseline's form.
func ledgerEntries() map[string]bool {
	out := map[string]bool{}
	for kind, l := range map[string][]string{"token": ledgerTokens, "class": ledgerClasses, "icon": ledgerIcons, "js": ledgerJS, "attr": ledgerAttrs} {
		for _, e := range l {
			out[kind+" "+e] = true
		}
	}
	return out
}

// TestNothingInV1WasRemoved: every token, class, icon, kit.js member and
// attribute the kit shipped as v1 is still in the ledger (and so, by the
// tests above, still in the kit and the doc). An addition needs no edit here;
// a removal or a rename fails, which is the v1 promise.
func TestNothingInV1WasRemoved(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "v1-baseline.txt"))
	if err != nil {
		t.Fatal(err)
	}
	now := ledgerEntries()
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
		if !now[line] {
			t.Errorf("%q was in v1 and is gone: v1 is additive only (a breaking change is /ui-kit/v2/)", line)
		}
	}
	if n < 150 {
		t.Fatalf("read only %d baseline entries; the file is not the v1 baseline", n)
	}
	for _, l := range [][]string{ledgerTokens, ledgerClasses, ledgerIcons} {
		seen := map[string]bool{}
		for _, e := range l {
			if seen[e] {
				t.Errorf("the ledger lists %q twice", e)
			}
			seen[e] = true
		}
	}
}

// --- the contract document -------------------------------------------------

func contractDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "ui-kit.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func section(t *testing.T, doc, from, to string) string {
	t.Helper()
	i := strings.Index(doc, "\n"+from+"\n")
	j := strings.Index(doc, "\n"+to+"\n")
	if i < 0 || j < i {
		t.Fatalf("docs/ui-kit.md has no %q … %q section", from, to)
	}
	return doc[i:j]
}

var backtick = regexp.MustCompile("`([^`]+)`")

func expandStatus(s string) []string {
	if !strings.Contains(s, "<status>") {
		return []string{s}
	}
	var out []string
	for _, st := range theme.Statuses {
		out = append(out, strings.ReplaceAll(s, "<status>", st))
	}
	return out
}

func set(l []string) map[string]bool {
	m := map[string]bool{}
	for _, e := range l {
		m[e] = true
	}
	return m
}

func compareSets(t *testing.T, what string, ledger []string, doc map[string]bool) {
	t.Helper()
	if len(doc) == 0 {
		t.Fatalf("read no %s from docs/ui-kit.md: the parser no longer matches the document", what)
	}
	l := set(ledger)
	var missing, extra []string
	for e := range l {
		if !doc[e] {
			missing = append(missing, e)
		}
	}
	for e := range doc {
		if !l[e] {
			extra = append(extra, e)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s in the ledger but not in docs/ui-kit.md: %v", what, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s in docs/ui-kit.md but not in the ledger: %v", what, extra)
	}
}

// TestTheLedgerIsTheContract reads docs/ui-kit.md's tables and compares each
// set with the ledger, both ways: the document cannot promise what the kit
// is not checked for, and the kit cannot be checked for what is not promised.
func TestTheLedgerIsTheContract(t *testing.T) {
	doc := contractDoc(t)

	toks := map[string]bool{}
	for _, m := range regexp.MustCompile("`--mu-([a-z0-9<>-]+)`").FindAllStringSubmatch(section(t, doc, "## Tokens", "## Components"), -1) {
		// `--mu-st-<status>-fg` expands; the prose's `--mu-dur-*` does not match.
		for _, e := range expandStatus(m[1]) {
			toks[e] = true
		}
	}
	compareSets(t, "tokens", ledgerTokens, toks)

	classes := map[string]bool{}
	comp := section(t, doc, "## Components", "## Icons")
	for _, line := range strings.Split(comp, "\n") {
		if !strings.HasPrefix(line, "| `mu-") {
			continue
		}
		first := strings.SplitN(line[1:], "|", 2)[0]
		for _, m := range backtick.FindAllStringSubmatch(first, -1) {
			for _, e := range expandStatus(m[1]) {
				classes[e] = true
			}
		}
	}
	compareSets(t, "classes", ledgerClasses, classes)

	icons := map[string]bool{}
	for _, line := range strings.Split(section(t, doc, "## Icons", "## kit.js"), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		first := strings.SplitN(line[1:], "|", 2)[0]
		ms := backtick.FindAllStringSubmatch(first, -1)
		if strings.Contains(first, "…") && len(ms) == 2 {
			// `signal-0` … `signal-4`
			p := strings.TrimSuffix(ms[0][1], "0")
			for i := 0; i <= 4; i++ {
				icons[p+string(rune('0'+i))] = true
			}
			continue
		}
		for _, m := range ms {
			icons[m[1]] = true
		}
	}
	compareSets(t, "icons", ledgerIcons, icons)

	js := map[string]bool{}
	attrs := map[string]bool{}
	for _, line := range strings.Split(section(t, doc, "## kit.js", "## The theme"), "\n") {
		if m := regexp.MustCompile("^\\| `MusterKit\\.([A-Za-z]+)").FindStringSubmatch(line); m != nil {
			js[m[1]] = true
		}
		if m := regexp.MustCompile("^\\| `(data-mu-[a-z]+)").FindStringSubmatch(line); m != nil {
			attrs[m[1]] = true
		}
	}
	compareSets(t, "kit.js members", ledgerJS, js)
	compareSets(t, "data attributes", ledgerAttrs, attrs)
}

// --- the kit itself ------------------------------------------------------------

func defaultTokens(t *testing.T) theme.Tokens {
	t.Helper()
	tok, err := theme.Choice{}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// classDefined reports whether css has a rule selecting .<class> exactly (not
// a longer class it is a prefix of).
func classDefined(css, class string) bool {
	return regexp.MustCompile(`\.` + regexp.QuoteMeta(class) + `(?:[^a-zA-Z0-9_-]|$)`).MatchString(css)
}

// TestKitCSSCarriesTheContract: the served kit.css declares every ledger
// token in its theme block and selects every ledger class. A positive
// control first: the matcher must not count `.mu-btn--primary` as `.mu-btn--p`.
func TestKitCSSCarriesTheContract(t *testing.T) {
	if classDefined(".mu-btn--primary{}", "mu-btn--p") || !classDefined(".mu-btn--primary{}", "mu-btn--primary") {
		t.Fatal("control: the class matcher matches prefixes or misses exact classes")
	}
	css := string(KitCSS(defaultTokens(t)).Body)
	block := css[:strings.Index(css, web.KitCSS)]
	for _, tok := range ledgerTokens {
		if !strings.Contains(block, "--mu-"+tok+": ") {
			t.Errorf("kit.css's theme block does not declare --mu-%s", tok)
		}
	}
	for _, c := range ledgerClasses {
		if !classDefined(css, c) {
			t.Errorf("kit.css defines no .%s", c)
		}
	}
	if !strings.HasPrefix(css, kitHeader+theme.CSS(defaultTokens(t))) || !strings.HasSuffix(css, web.KitCSS) {
		t.Error("kit.css is not the theme block followed by web/css/kit.css")
	}
}

// TestKitCSSIsTheStylesheetAppCSSImports: the components the kit serves are
// the file input.css imports into app.css — one source.
func TestKitCSSIsTheStylesheetAppCSSImports(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("..", "..", "web", "css", "input.css"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(in), `@import "./kit.css";`) {
		t.Fatal("web/css/input.css does not import kit.css: muster's own stylesheet and the kit have forked")
	}
	disk, err := os.ReadFile(filepath.Join("..", "..", "web", "css", "kit.css"))
	if err != nil || string(disk) != web.KitCSS {
		t.Fatalf("web.KitCSS is not web/css/kit.css (%v)", err)
	}
	app, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"mu-btn--primary", "mu-icon-btn", "mu-tile--speaking", "mu-toast"} {
		if !classDefined(string(app), c) {
			t.Errorf("the committed app.css has no .%s: rebuild it (make css)", c)
		}
	}
}

// TestFixedTokensAreKitCSSDefaults: web/css/kit.css's own :root block (what
// app.css carries) declares exactly theme.Fixed, value for value.
func TestFixedTokensAreKitCSSDefaults(t *testing.T) {
	i := strings.Index(web.KitCSS, "\n:root {")
	if i < 0 {
		t.Fatal("web/css/kit.css has no :root block")
	}
	body := web.KitCSS[i+len("\n:root {"):]
	body = body[:strings.Index(body, "}")]
	got := map[string]string{}
	for _, m := range regexp.MustCompile(`--mu-([a-z-]+):\s*([^;]+);`).FindAllStringSubmatch(body, -1) {
		got[m[1]] = strings.TrimSpace(m[2])
	}
	if len(got) != len(theme.Fixed) {
		t.Errorf("kit.css's :root declares %d tokens, theme.Fixed has %d", len(got), len(theme.Fixed))
	}
	for _, f := range theme.Fixed {
		if got[f.Name] != f.Value {
			t.Errorf("--mu-%s: kit.css %q, theme.Fixed %q", f.Name, got[f.Name], f.Value)
		}
	}
	if !strings.Contains(web.KitCSS, "@media (prefers-reduced-motion: reduce) {\n  :root { --mu-dur-fast: 0ms; --mu-dur-base: 0ms; --mu-dur-slow: 0ms; }\n}") {
		t.Error("kit.css's reduced-motion rule no longer zeroes the three durations")
	}
}

// TestIconsSVGCarriesTheContract: the sprite parses as XML, holds one symbol
// per Icons entry, and every ledger id is there.
func TestIconsSVGCarriesTheContract(t *testing.T) {
	body := IconsSVG()
	var doc struct {
		Symbols []struct {
			ID      string `xml:"id,attr"`
			ViewBox string `xml:"viewBox,attr"`
			Inner   string `xml:",innerxml"`
		} `xml:"symbol"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("icons.svg is not well-formed XML: %v", err)
	}
	ids := map[string]bool{}
	for _, s := range doc.Symbols {
		if s.ViewBox != "0 0 24 24" || strings.TrimSpace(s.Inner) == "" {
			t.Errorf("symbol %s: viewBox %q, %d bytes of drawing", s.ID, s.ViewBox, len(s.Inner))
		}
		ids[s.ID] = true
	}
	if len(doc.Symbols) != len(Icons) {
		t.Errorf("%d symbols for %d icons", len(doc.Symbols), len(Icons))
	}
	for _, n := range ledgerIcons {
		if !ids["mu-"+n] {
			t.Errorf("icons.svg has no id mu-%s", n)
		}
	}
	// Each signal level is a different drawing (a shape, not a colour).
	seen := map[string]string{}
	for i := 0; i <= 4; i++ {
		n := "signal-" + string(rune('0'+i))
		if prev, dup := seen[Icons[n]]; dup {
			t.Errorf("%s draws the same as %s", n, prev)
		}
		seen[Icons[n]] = n
	}
}

// TestKitJSCarriesTheContract: kit.js's version is Version, and its frozen
// API object has every ledger member (and only those), and it reads every
// ledger attribute.
func TestKitJSCarriesTheContract(t *testing.T) {
	js := string(KitJS().Body)
	if !strings.Contains(js, "var VERSION = '"+Version+"';") {
		t.Errorf("kit.js does not report version %s", Version)
	}
	i := strings.Index(js, "window.MusterKit = Object.freeze({")
	if i < 0 {
		t.Fatal("kit.js does not define a frozen window.MusterKit")
	}
	obj := js[i:]
	obj = obj[:strings.Index(obj, "});")]
	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+([A-Za-z]+):`).FindAllStringSubmatch(obj, -1) {
		got[m[1]] = true
	}
	compareSets(t, "MusterKit members", ledgerJS, got)
	for _, a := range ledgerAttrs {
		if !strings.Contains(js, a) {
			t.Errorf("kit.js never reads %s", a)
		}
	}
	// 🔴 A toast's message is text, never HTML.
	if strings.Contains(js, "innerHTML") || strings.Contains(js, "insertAdjacentHTML") {
		t.Error("kit.js writes HTML from a string; the contract says messages are drawn as text")
	}
}

// TestKitJSCarriesTheSprite: the served kit.js holds the whole icon sprite —
// every ledger id, as the very sprite icons.svg serves — and makes no request
// for it, so a plugin on any origin draws the icons with no CORS. A control
// first: the source file itself names no icon (the sprite is put in when
// served), so finding the ids in the source would prove nothing.
func TestKitJSCarriesTheSprite(t *testing.T) {
	if strings.Contains(kitJSSource, "mu-mic-off") || strings.Count(kitJSSource, spritePlaceholder) != 1 {
		t.Fatal("control: kit.js's source should carry the placeholder once and no icon")
	}
	js := string(KitJS().Body)
	lit, err := json.Marshal(string(IconsSVG()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js, "var SPRITE = "+string(lit)+";") {
		t.Error("kit.js does not carry icons.svg's sprite as its SPRITE")
	}
	for _, n := range ledgerIcons {
		if !strings.Contains(js, `id=\"mu-`+n+`\"`) {
			t.Errorf("kit.js's sprite has no mu-%s", n)
		}
	}
	for _, req := range []string{"fetch(", "XMLHttpRequest", "icons.svg"} {
		if strings.Contains(js, req) {
			t.Errorf("kit.js still reaches for the sprite (%q)", req)
		}
	}
}

// TestTheETagMovesWithTheThemeAndOnlyWithIt: two themes, two ETags for
// kit.css and theme.css; the same theme twice, one ETag; icons.svg and kit.js
// do not depend on the theme at all.
func TestTheETagMovesWithTheThemeAndOnlyWithIt(t *testing.T) {
	a := defaultTokens(t)
	b, err := theme.Choice{Preset: "harbor"}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if KitCSS(a).ETag == KitCSS(b).ETag || ThemeCSS(a).ETag == ThemeCSS(b).ETag {
		t.Error("a different theme did not change the ETag")
	}
	if KitCSS(a).ETag != KitCSS(defaultTokens(t)).ETag {
		t.Error("the same theme gave two ETags")
	}
	if KitCSS(a).ETag != theme.ETag(KitCSS(a).Body) {
		t.Error("the ETag is not the hash of the body")
	}
}
