package api

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/suggest"
	"github.com/ZacxDev/muster/internal/ui"
)

// EVERY "HOW TO TURN IT ON" LINK OPENS ITS FEATURE'S ⓘ SHEET (the seam between
// the hints beside a control that is off — the muted mic and paperclip, the
// nav's muted Connectors, the Account page's suggested-replies note, the
// Credentials page — and the Features page that answers them). Each side was
// tested alone; what breaks is the pair: a hint whose anchor the page does
// not draw lands on the page and opens nothing.

// featureLinkLedger is every place a "how to turn it on" link is built, as
// "<package dir>/<file> <function> <feature argument>": a call to
// ui.FeatureHref, or to featureOffNavLink (which passes its feature argument
// on to it). Both directions fail: a new caller, or one that went away.
var featureLinkLedger = map[string]string{
	"ui/attach.go attachOffControl features.Attachments":                     features.Attachments,
	"ui/dictate.go dictateOffControl features.Dictation":                     features.Dictation,
	"ui/components.go featureOffNavLink feature":                             "", // its callers are ledgered below
	"ui/components.go sidebarIn featureOffNavLink features.Connectors":       features.Connectors,
	"api/suggest_account.go accountSuggestSection features.SuggestedReplies": features.SuggestedReplies,
	"api/credentials.go renderCredentials features.Connectors":               features.Connectors,
}

// featureLinkSites reads the ledger's facts from the source: every call to
// FeatureHref / ui.FeatureHref and to featureOffNavLink in the non-test files
// of internal/ui and internal/api, and every string literal that spells a
// Features page anchor by hand (which no ledger row can cover).
func featureLinkSites(t *testing.T) (sites []string, handSpelled []string) {
	t.Helper()
	fset := token.NewFileSet()
	for _, dir := range []string{"../ui", "."} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		pkg := map[string]string{"../ui": "ui", ".": "api"}[dir]
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			base := pkg + "/" + filepath.Base(path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CallExpr:
						name, argAt := "", 0
						switch fun := x.Fun.(type) {
						case *ast.Ident:
							name = fun.Name
						case *ast.SelectorExpr:
							name = fun.Sel.Name
						}
						switch name {
						case "FeatureHref":
						case "featureOffNavLink":
							argAt = 1
						default:
							return true
						}
						if name == "FeatureHref" && fn.Name.Name == "FeatureHref" {
							return true
						}
						var arg strings.Builder
						if len(x.Args) > argAt {
							_ = printer.Fprint(&arg, fset, x.Args[argAt])
						}
						site := base + " " + fn.Name.Name + " " + arg.String()
						if name == "featureOffNavLink" {
							site = base + " " + fn.Name.Name + " featureOffNavLink " + arg.String()
						}
						sites = append(sites, site)
					case *ast.BasicLit:
						if x.Kind == token.STRING {
							if v, err := strconv.Unquote(x.Value); err == nil && strings.Contains(v, ui.FeaturesPagePath+"#") {
								handSpelled = append(handSpelled, base+" "+fn.Name.Name+" "+x.Value)
							}
						}
					}
					return true
				})
			}
		}
	}
	sort.Strings(sites)
	return sites, handSpelled
}

// sheetIDs is every element id on page that is an ⓘ sheet, and every id.
func sheetIDs(t *testing.T, page string) (sheets, all map[string]bool) {
	t.Helper()
	sheets, all = map[string]bool{}, map[string]bool{}
	z := html.NewTokenizer(strings.NewReader(page))
	for tt := z.Next(); tt != html.ErrorToken; tt = z.Next() {
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		id, sheet := "", false
		for _, a := range tok.Attr {
			switch a.Key {
			case "id":
				id = a.Val
			case "data-info-sheet":
				sheet = true
			}
		}
		if id != "" {
			all[id] = true
			sheets[id] = sheets[id] || sheet
		}
	}
	return sheets, all
}

var featuresAnchorHref = regexp.MustCompile(`href="` + regexp.QuoteMeta(FeaturesPagePath) + `#([^"]*)"`)

// TestEveryHowToTurnItOnLinkOpensAFeatureSheet is the seam guard, three ways:
//
//   - STRUCTURE: the ledger above is exactly the set of places a link is
//     built (a new hint, or one that stopped going through FeatureHref, fails),
//     and no source spells a Features anchor by hand;
//   - RESOLUTION: for every ledgered feature, the anchor FeatureHref writes
//     is an ⓘ sheet on the Features page — an admin's and a member's;
//   - BEHAVIOUR: the hints the api package draws (the ask page's and the
//     Asks list's muted mic, the Account page's suggested-replies note, the
//     Credentials page and its nav's muted Connectors entry), rendered, link
//     only to anchors that are sheets on that page.
//
// Red at origin/main, whose hints linked a row (#dictation), not a sheet.
func TestEveryHowToTurnItOnLinkOpensAFeatureSheet(t *testing.T) {
	sites, hand := featureLinkSites(t)
	if len(sites) == 0 {
		t.Fatal("CONTROL: no FeatureHref call found — the source scan reads nothing")
	}
	var want []string
	for k := range featureLinkLedger {
		want = append(want, k)
	}
	sort.Strings(want)
	if strings.Join(sites, "\n") != strings.Join(want, "\n") {
		t.Errorf("the places that build a \"how to turn it on\" link changed — ledger each one (and its anchor):\n got:\n  %s\nwant:\n  %s",
			strings.Join(sites, "\n  "), strings.Join(want, "\n  "))
	}
	if len(hand) > 0 {
		t.Errorf("a Features page anchor spelled by hand, not through ui.FeatureHref: %q", hand)
	}

	s, h := featureServer(t, Extensions{Suggest: &suggest.Engine{}}, plainFeatureSet(t))
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleMember} {
		sheets, all := sheetIDs(t, featuresPageAs(t, s, role))
		if len(all) == 0 {
			t.Fatalf("%s: CONTROL: no element id parsed from the Features page", role)
		}
		for site, id := range featureLinkLedger {
			if id == "" {
				continue
			}
			href := ui.FeatureHref(id)
			frag := href[strings.Index(href, "#")+1:]
			if !sheets[frag] {
				t.Errorf("%s: %s links %s, which is not an ⓘ sheet on the Features page (an element: %v)", role, site, href, all[frag])
			}
		}
	}

	// The hints as the api package draws them, against the admin's page (the
	// member's has the same sheet ids, checked above).
	sheets, _ := sheetIDs(t, featuresPageAs(t, s, authz.RoleAdmin))
	r := httptest.NewRequest(http.MethodGet, "/attention/asks/9", nil)
	surfaces := map[string]string{
		"ask page composer": askComposerHTML(r, attention.Ask{ID: 9}, "", true),
		"Asks answer box":   attentionItemsHTMLTo([]attentionItem{{Kind: attentionKindAsk, AskID: 12, dictateOff: true}}, time.Now(), ""),
		"Account page":      sessionGet(h, s, "/account", true).Body.String(),
		"Credentials page":  sessionGet(h, s, "/account/credentials", true).Body.String(),
	}
	seen := map[string]bool{}
	for name, doc := range surfaces {
		links := featuresAnchorHref.FindAllStringSubmatch(doc, -1)
		if len(links) == 0 {
			t.Errorf("CONTROL: the %s draws no \"how to turn it on\" link", name)
		}
		for _, m := range links {
			seen[m[1]] = true
			if !sheets[m[1]] {
				t.Errorf("the %s links %s#%s, which is not an ⓘ sheet on the Features page", name, FeaturesPagePath, m[1])
			}
		}
	}
	// The rendered hints reach three features: dictation, suggested replies
	// and connectors (the Credentials page's link and its nav's entry).
	if len(seen) < 3 {
		t.Errorf("CONTROL: the rendered hints link %d distinct anchor(s), want 3: %v", len(seen), seen)
	}
}
