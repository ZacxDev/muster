package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/web"
)

// The favicon and the touch icon on every document (docs/branding.md).
//
// 🔴 THE DEFECT THESE PIN: in 0.187.0 the brand icon links were drawn by two
// of the ten document heads (the shell and NavDocument) while the manifest
// link was drawn by all ten, so the landing page — the chief's agent page —
// served no rel=icon at all. Both are now emitted by ONE helper, docHead, and
// the tests below hold that from two sides: every document, rendered, carries
// the links (behavioural), and the old per-shell spellings have exactly one
// caller (structural), so a new document head cannot be written with the
// manifest and without the icons.

// testBrand is a logo with an ETag no other string in a page can produce, so
// a match on it can only come from the brand icon links.
var testBrand = Brand{Name: "Branded Instance", ETag: "zq81-brand-etag"}

// brandedDocuments renders every standalone document (keyed by the function
// that emits its Doctype, as doctypeEmitters names it) with the given Brand.
//
// 🔴 ITS KEY SET IS CHECKED AGAINST THE SOURCE: a document added to the package
// without a row here fails TestEveryDocumentLinksTheBrandIcon, rather than
// going unswept.
func brandedDocuments(b Brand) map[string]func(io.Writer) error {
	feat := featuresAllOn
	feat.Brand = b
	return map[string]func(io.Writer) error{
		"PageWith": func(w io.Writer) error { return RenderPage(w, defaultTab, feat) },
		"ComposePage": func(w io.Writer) error {
			return RenderComposePage(w, feat, "Shared title")
		},
		"AgentDetailPage": func(w io.Writer) error { return RenderAgentDetail(w, sampleAgentDetailView(), feat) },
		"TaskDetailPage":  func(w io.Writer) error { return RenderTaskDetail(w, sampleTaskDetailView(), feat) },
		"TaskNotFoundPage": func(w io.Writer) error {
			return RenderTaskNotFound(w, taskNotFoundFixtureID, feat)
		},
		"AgentTerminalPage": func(w io.Writer) error {
			return AgentTerminalPage(AgentTerminalView{Name: "keen-wren", Nav: NavChrome{Feat: feat}}).Render(w)
		},
		"MirrorViewPage": func(w io.Writer) error {
			return MirrorViewPage(MirrorViewView{Host: "laptop", Session: "main", Nav: NavChrome{Feat: feat}}).Render(w)
		},
		"StepUpFormPage": func(w io.Writer) error {
			return StepUpFormPage(StepUpFormView{Action: "/mirror/confirm-send", BackHref: "/agents", Window: "12 hours", Brand: b}).Render(w)
		},
		"SendQueueRetryConfirmPage": func(w io.Writer) error {
			return SendQueueRetryConfirmPage("/send-queue/7/retry", "confirm", "Again?", "/agents", b).Render(w)
		},
		"SendAgainPage": func(w io.Writer) error {
			return SendAgainPage(SendAgainView{Action: "/send-queue/7/send-now", Button: "Send it now anyway", BackHref: "/agents", Brand: b}).Render(w)
		},
		"NavDocument": func(w io.Writer) error {
			return RenderNavDocument(w, NavDocumentView{Title: "Plugins", Feat: feat, Main: `<section><h1>Plugins</h1></section>`})
		},
	}
}

var (
	linkTag     = regexp.MustCompile(`<link [^>]*>`)
	relAttr     = regexp.MustCompile(` rel="([^"]*)"`)
	hrefAttr    = regexp.MustCompile(` href="([^"]*)"`)
	defaultIcon = "/static/icons/icon-192.png"
)

// headLinks is each rel's hrefs, in document order.
func headLinks(page string) map[string][]string {
	out := map[string][]string{}
	for _, tag := range linkTag.FindAllString(page, -1) {
		rel := relAttr.FindStringSubmatch(tag)
		href := hrefAttr.FindStringSubmatch(tag)
		if rel == nil || href == nil {
			continue
		}
		out[rel[1]] = append(out[rel[1]], strings.ReplaceAll(href[1], "&amp;", "&"))
	}
	return out
}

// checkIconLinks asserts page carries exactly one manifest link, and exactly
// one favicon and one touch icon, both at want.
func checkIconLinks(t *testing.T, label, page, want string) {
	t.Helper()
	links := headLinks(page)
	if got := links["manifest"]; len(got) != 1 {
		t.Errorf("%s: %d rel=manifest links, want 1: %v", label, len(got), got)
	}
	for _, rel := range []string{"icon", "apple-touch-icon"} {
		if got := links[rel]; len(got) != 1 || got[0] != want {
			t.Errorf("%s: rel=%s links %v, want exactly [%s]", label, rel, got, want)
		}
	}
}

// TestEveryDocumentLinksTheBrandIcon: every document muster serves carries the
// favicon and touch icon — the logo's when one is set, muster's own when not —
// beside its manifest link.
func TestEveryDocumentLinksTheBrandIcon(t *testing.T) {
	// The ledger: the rows here are exactly the package's Doctype emitters, so
	// a document added or removed without this table following fails here.
	derived := doctypeEmitters(t)
	if len(derived) < 2 {
		t.Fatalf("the source scan found %d Doctype emitters — it is not observing this package", len(derived))
	}
	var rows []string
	for fn := range brandedDocuments(Brand{}) {
		rows = append(rows, fn)
	}
	sort.Strings(rows)
	sort.Strings(derived)
	if strings.Join(rows, ",") != strings.Join(derived, ",") {
		t.Fatalf("brandedDocuments covers %v, the package emits a Doctype from %v: add the new document's row", rows, derived)
	}

	branded := testBrand.Icon192()
	if !strings.Contains(branded, BrandIcon192Path+"?v="+testBrand.ETag) {
		t.Fatalf("the branded icon is %q — the fixture does not reach the logo's path", branded)
	}
	unbranded := Brand{}.Icon192()
	// 🔴 THE ZERO BRAND'S ICON SHIPS: an unbranded server must not gain a link
	// to a file it does not serve. AssetHash is "" for a path not in the embed.
	if !strings.HasPrefix(unbranded, defaultIcon) || web.AssetHash(defaultIcon) == "" {
		t.Fatalf("the default icon is %q and %s is not in the embed", unbranded, defaultIcon)
	}
	for fn, render := range brandedDocuments(testBrand) {
		checkIconLinks(t, fn+" (branded)", renderString(t, g.NodeFunc(render)), branded)
	}
	for fn, render := range brandedDocuments(Brand{}) {
		page := renderString(t, g.NodeFunc(render))
		checkIconLinks(t, fn+" (no logo)", page, unbranded)
		if strings.Contains(page, "/brand/icon") {
			t.Errorf("%s (no logo) links a brand icon", fn)
		}
	}
}

// TestTheAgentPageLinksTheBrandIcon is the measured case: the chief's agent
// page, where `/` lands, served no rel=icon in 0.187.0.
func TestTheAgentPageLinksTheBrandIcon(t *testing.T) {
	feat := featuresAllOn
	feat.Brand = testBrand
	page := renderString(t, AgentDetailPage(AgentDetailView{ID: 1, Name: "chief", DisplayName: "chief", Status: "running",
		Sessions: []SessionView{{ID: 92, Title: "Today", Active: true}}, ActiveSessionID: 92}, feat))
	want := BrandIcon192Path + "?v=" + testBrand.ETag
	for _, tag := range []string{
		`<link rel="icon" type="image/png" sizes="192x192" href="` + want + `">`,
		`<link rel="apple-touch-icon" href="` + want + `">`,
	} {
		if !strings.Contains(page, tag) {
			t.Errorf("the agent page lacks %s", tag)
		}
	}
}

// TestDocumentHeadsHaveOneSpelling is the structural half: the manifest link
// and the brand icon links are emitted only by docHead, and docHead is called
// by exactly the document heads listed here. A head that spells manifestLink()
// on its own — the 0.187.0 shape — fails, and so does a document head added
// or removed without this list following.
func TestDocumentHeadsHaveOneSpelling(t *testing.T) {
	// Positive control: ThemeHead is in every document head, so a scan that
	// cannot see it is not reading this package.
	if got := callersOf(t, ".", "ThemeHead"); len(got) < 10 {
		t.Fatalf("the scan found ThemeHead called from only %v — it is not observing this package", got)
	}
	for _, helper := range []string{"manifestLink", "brandIconLinks"} {
		if got := callersOf(t, ".", helper); strings.Join(got, ",") != "docHead" {
			t.Errorf("%s is called from %v, want only docHead: a document head must use docHead, "+
				"which draws the manifest and the brand icons together", helper, got)
		}
	}
	want := []string{
		// DocHeadHTML: internal/api's hand-written documents (writeAuthDocument),
		// held from that side by TestEveryHandWrittenDocumentDrawsDocHead.
		"AgentDetailPage", "AgentTerminalPage", "DocHeadHTML", "MirrorViewPage", "NavDocument", "SendAgainPage",
		"SendQueueRetryConfirmPage", "StepUpFormPage", "TaskDetailPage", "TaskNotFoundPage", "shell",
	}
	if got := callersOf(t, ".", "docHead"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("docHead is called from %v, want %v", got, want)
	}
}

// callersOf is the sorted, de-duplicated names of the top-level declarations
// in dir's non-test files that call the bare identifier name.
func callersOf(t *testing.T, dir, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, dir+"/"+n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Both declaration shapes, as doctypeEmittersIn walks them: `func X()`
		// and `var X = func() …` — a call in the second must not go unseen.
		calls := func(n ast.Node) bool {
			found := false
			ast.Inspect(n, func(x ast.Node) bool {
				if c, ok := x.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == name {
						found = true
					}
				}
				return !found
			})
			return found
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil && calls(d.Body) {
					seen[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					vs, ok := sp.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, v := range vs.Values {
						if calls(v) && i < len(vs.Names) {
							seen[vs.Names[i].Name] = true
						}
					}
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
