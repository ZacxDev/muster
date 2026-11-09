package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// "SHOW TMUX SESSIONS" ON THE SIDEBAR'S ROWS AND THE COMMAND PALETTE'S
// (internal/api notify_prefs.go). internal/api decides each entry's TmuxAt by
// its one rule (names.go tmuxPlace); the ui draws it — the sidebar row as a
// muted line under its name (navRowName), and both kinds of entry element (a
// switcher pill, a sidebar row) carry it for the palette's row as
// data-switcher-tmux (entryTmuxAttr).

// navRowOf is the sidebar list holding exactly card c.
func navRowOf(t *testing.T, c MirrorCardView) string {
	t.Helper()
	return renderRaw(t, NavAgentsList(nil, []MirrorCardView{c}, "", navNow))
}

// withNavFocus is c with a focus title set 50 hours before navNow (old enough
// that its age, "2d", is drawn).
func withNavFocus(c MirrorCardView) MirrorCardView {
	c.Focus = &FocusView{Title: "Fixing the Stripe webhook retry", UpdatedAt: navNow.Add(-50 * time.Hour)}
	return c
}

// navRowOffGolden and navRowFocusOffGolden are a named window's sidebar list
// with the switch OFF — with no focus title, and with one — as main drew them
// before this change (captured at 5549f9ab, the merge-base, from this very
// fixture; at the merge-base a card carrying TmuxAt drew the same bytes).
const navRowOffGolden = `<ul aria-labelledby="sidebar-agents-label" class="flex flex-col gap-0.5"><li class="min-w-0"><a id="nav-mirror-b7e67c892c74" href="/mirror/terminal?host=atlas&amp;session=scratch3&amp;window=%405" hx-boost="false" data-nav-agent="mirror" title="Billing migration · mango:1 · tmux scratch3:5 @ atlas" data-switcher-name="Billing migration" data-switcher-code="mango:1" data-switcher-title="Billing migration" data-switcher-words="scratch3 atlas" class="press flex min-h-[44px] w-full min-w-0 items-center gap-2 rounded-lg border-l-4 px-3 text-left text-sm font-medium transition border-l-transparent text-fg2 hover:bg-s2 hover:text-fg"><span class="inline-flex w-2 shrink-0 justify-center"></span><span data-host-icon="laptop" aria-hidden="true" title="Host: atlas" class="inline-flex h-4 w-4 shrink-0 items-center justify-center text-fg2"><svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><rect x="4" y="5" width="16" height="11" rx="1"/><path d="M2 19h20"/></svg></span><span class="min-w-0 flex-1 truncate">Billing migration</span></a></li></ul>`

const navRowFocusOffGolden = `<ul aria-labelledby="sidebar-agents-label" class="flex flex-col gap-0.5"><li class="min-w-0"><a id="nav-mirror-b7e67c892c74" href="/mirror/terminal?host=atlas&amp;session=scratch3&amp;window=%405" hx-boost="false" data-nav-agent="mirror" title="Billing migration · mango:1 · tmux scratch3:5 @ atlas" data-switcher-name="Billing migration" data-switcher-code="mango:1" data-switcher-title="Billing migration · Fixing the Stripe webhook retry" data-switcher-words="scratch3 atlas" data-switcher-focus-age="2d" class="press flex min-h-[44px] w-full min-w-0 items-center gap-2 rounded-lg border-l-4 px-3 text-left text-sm font-medium transition border-l-transparent text-fg2 hover:bg-s2 hover:text-fg"><span class="inline-flex w-2 shrink-0 justify-center"></span><span data-host-icon="laptop" aria-hidden="true" title="Host: atlas" class="inline-flex h-4 w-4 shrink-0 items-center justify-center text-fg2"><svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><rect x="4" y="5" width="16" height="11" rx="1"/><path d="M2 19h20"/></svg></span><span class="flex min-w-0 flex-1 flex-col leading-tight"><span data-nav-name="" class="truncate">Billing migration</span><span data-nav-focus="" class="flex min-w-0 items-baseline gap-1 text-xs font-normal text-muted"><span class="min-w-0 truncate">Fixing the Stripe webhook retry</span><span data-focus-age="" title="focus title set 2d ago" class="shrink-0">· 2d</span></span></span></a></li></ul>`

// TestASidebarRowIsByteIdenticalWithTheSwitchOff: off (no TmuxAt), the named
// window's row is exactly main's — the literal goldens above, with and without
// a focus title.
func TestASidebarRowIsByteIdenticalWithTheSwitchOff(t *testing.T) {
	if got := navRowOf(t, namedTmuxCard("", "")); got != navRowOffGolden {
		t.Errorf("the OFF row (no focus) changed:\n got  %s\n want %s", got, navRowOffGolden)
	}
	if got := navRowOf(t, withNavFocus(namedTmuxCard("", ""))); got != navRowFocusOffGolden {
		t.Errorf("the OFF row (a focus title) changed:\n got  %s\n want %s", got, navRowFocusOffGolden)
	}
}

// TestASidebarRowWithTheSwitchOnIsTheOffRowPlusItsPlace: the WHOLE ON row,
// byte for byte — the OFF golden with exactly its edits and nothing else
// (literal classes, not the constants, so a class change is seen):
//
//   - the link carries data-switcher-tmux="mango:1 @ atlas", for the palette;
//   - with a focus title: a third line, data-nav-tmux, after the focus line;
//   - with none: the name becomes the two-line form, the place its second line.
func TestASidebarRowWithTheSwitchOnIsTheOffRowPlusItsPlace(t *testing.T) {
	const attr = ` data-switcher-tmux="mango:1 @ atlas" class="press flex`
	const line = `<span data-nav-tmux="" class="min-w-0 truncate text-xs font-normal text-muted">mango:1 @ atlas</span>`

	want := strings.Replace(navRowFocusOffGolden, ` class="press flex`, attr, 1)
	want = strings.Replace(want, `· 2d</span></span></span></a>`, `· 2d</span></span>`+line+`</span></a>`, 1)
	if strings.Count(want, "mango:1 @ atlas") != 2 { // the attr's and the line's
		t.Fatal("fixture: the ON golden (focus) was not built from the OFF one")
	}
	if got := navRowOf(t, withNavFocus(namedTmuxCard("mango:1 @ atlas", "mango:1"))); got != want {
		t.Errorf("the ON row (a focus title):\n got  %s\n want %s", got, want)
	}

	want = strings.Replace(navRowOffGolden, ` class="press flex`, attr, 1)
	want = strings.Replace(want, `<span class="min-w-0 flex-1 truncate">Billing migration</span>`,
		`<span class="flex min-w-0 flex-1 flex-col leading-tight"><span data-nav-name="" class="truncate">Billing migration</span>`+line+`</span>`, 1)
	if strings.Count(want, "mango:1 @ atlas") != 2 {
		t.Fatal("fixture: the ON golden (no focus) was not built from the OFF one")
	}
	if got := navRowOf(t, namedTmuxCard("mango:1 @ atlas", "mango:1")); got != want {
		t.Errorf("the ON row (no focus):\n got  %s\n want %s", got, want)
	}
}

// TestNoSidebarPlaceForAnUnnamedWindowOrAnAgent: the api's rule sets nothing
// for a window called by its codename already, nor for an agent; their rows
// draw nothing (no data-switcher-tmux, no line). CONTROL: the same list with
// a named window that has a place draws it once.
func TestNoSidebarPlaceForAnUnnamedWindowOrAnAgent(t *testing.T) {
	unnamed := MirrorCardView{Host: "atlas", Session: "scratch3", Window: "@6", Label: "kiwi:2", Online: true}
	agent := AgentCardView{ID: 7, Name: "brave-otter", DisplayName: "Billing migration", Status: "running"}
	off := renderRaw(t, NavAgentsList([]AgentCardView{agent}, []MirrorCardView{unnamed}, "", navNow))
	for _, bad := range []string{"data-switcher-tmux", "data-nav-tmux"} {
		if strings.Contains(off, bad) {
			t.Errorf("an unnamed window or an agent drew %q", bad)
		}
	}
	on := renderRaw(t, NavAgentsList([]AgentCardView{agent}, []MirrorCardView{unnamed, namedTmuxCard("mango:1 @ atlas", "mango:1")}, "", navNow))
	if strings.Count(on, `data-switcher-tmux="mango:1 @ atlas"`) != 1 || strings.Count(on, "data-nav-tmux") != 1 {
		t.Fatalf("control: the named window's place is not drawn exactly once:\n%s", on)
	}
}

// TestTheSidebarPlaceIsEscaped: the place is the host's text (its label);
// markup in it is drawn as text, in the attribute and in the line.
func TestTheSidebarPlaceIsEscaped(t *testing.T) {
	got := navRowOf(t, namedTmuxCard(`<b>x</b>"&:1 @ atlas`, `<b>x</b>"&:1`))
	if strings.Contains(got, "<b>") {
		t.Fatalf("raw markup from the place reached the row:\n%s", got)
	}
	for _, want := range []string{
		`data-switcher-tmux="&lt;b&gt;x&lt;/b&gt;&#34;&amp;:1 @ atlas"`,
		`<span data-nav-tmux="" class="min-w-0 truncate text-xs font-normal text-muted">&lt;b&gt;x&lt;/b&gt;&#34;&amp;:1 @ atlas</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the escaped place lacks\n%s\nin:\n%s", want, got)
		}
	}
}

// TestThePaletteDrawsThePlaceFromTheEntry: the palette's script reads an
// entry's data-switcher-tmux (musterPaletteEntry, the one reading of a pill's
// or row's attributes) and draws it as its own muted line, data-palette-tmux,
// with textContent — after the line under the label; the bare codename line
// is not drawn as well when there is a place (the place starts with it); the
// match rule never reads it (search is unchanged).
func TestThePaletteDrawsThePlaceFromTheEntry(t *testing.T) {
	js := paletteJS
	i, j := strings.Index(js, "window.musterPaletteEntry = function"), strings.Index(js, "window.musterWaitMark = function")
	if i < 0 || j < i {
		t.Fatal("musterPaletteEntry is not found")
	}
	if !strings.Contains(js[i:j], "tmux: a.getAttribute('data-switcher-tmux') || ''") {
		t.Error("musterPaletteEntry does not read data-switcher-tmux as the entry's tmux")
	}
	i, j = strings.Index(js, "function render(keep)"), strings.Index(js, "function go(i)")
	if i < 0 || j < i {
		t.Fatal("the palette's render is not found")
	}
	row := js[i:j]
	for _, want := range []string{
		"var sub = isTitle ? e.title : (e.code !== e.label && !e.tmux ? e.code : '');",
		"if (e.tmux) {",
		"var tl = el('span', 'truncate text-xs text-muted', e.tmux);",
		"tl.setAttribute('data-palette-tmux', '');",
		"li.appendChild(tl);",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("the palette's render lacks %q", want)
		}
	}
	if strings.Index(row, "var sub =") > strings.Index(row, "if (e.tmux) {") {
		t.Error("the place line is drawn before the line under the label")
	}
	if strings.Contains(row, "innerHTML") {
		t.Error("the palette's row is drawn with innerHTML")
	}
	m0, m1 := strings.Index(js, "window.musterPaletteMatch = function"), strings.Index(js, "window.musterPaletteEntry = function")
	if strings.Contains(js[m0:m1], "tmux") {
		t.Error("the palette's match rule reads the place: search must not change")
	}
}

// uiFuncName is fn's name as the ledgers below key it: "Recv.Name" or "Name".
func uiFuncName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		t := fn.Recv.List[0].Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		if id, ok := t.(*ast.Ident); ok {
			return id.Name + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

// uiPackageFiles is this package's non-test source, parsed.
func uiPackageFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.File{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = f
	}
	return out
}

func uiSortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestOnlyTheEntryHelpersDrawTheCodename pins THE SEAM between "Show tmux
// sessions"' value and the surfaces that draw it, in this package — failing
// when any set GROWS or SHRINKS:
//
//  1. the only functions that READ an entry's TmuxAt / TmuxCode are
//     switcherItems (copies the card's onto its item), entryTmuxAttr (the
//     attribute every script draws from), switcherLabel (the pill's line) and
//     navRowName (the sidebar's line);
//  2. the attribute's name is spelled, in Go, only by entryTmuxAttr — so no
//     element carries the place but through it — and entryTmuxAttr is called
//     by exactly the two entry elements, switcherEntry and navAgentRow;
//  3. the scripts that read data-switcher-tmux are exactly the dock's (rowOf)
//     and the palette's (musterPaletteEntry, in paletteMatchJS).
//
// Each set's positive control is the set itself: a walk that saw nothing
// would yield an empty set, which is not the wanted one.
func TestOnlyTheEntryHelpersDrawTheCodename(t *testing.T) {
	files := uiPackageFiles(t)
	readers, spellers, callers, scripts := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for path, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				name := path + " " + uiFuncName(d)
				ast.Inspect(d.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						if x.Sel.Name == "TmuxAt" || x.Sel.Name == "TmuxCode" {
							readers[name] = true
						}
					case *ast.BasicLit:
						if x.Kind == token.STRING {
							if v, err := strconv.Unquote(x.Value); err == nil {
								if v == "data-switcher-tmux" {
									spellers[name] = true
								}
								if strings.Contains(v, "'data-switcher-tmux'") {
									scripts[name] = true
								}
							}
						}
					case *ast.CallExpr:
						if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "entryTmuxAttr" {
							callers[name] = true
						}
					}
					return true
				})
			case *ast.GenDecl:
				for _, s := range d.Specs {
					vs, ok := s.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for k, v := range vs.Values {
						ast.Inspect(v, func(n ast.Node) bool {
							if x, ok := n.(*ast.BasicLit); ok && x.Kind == token.STRING {
								if u, err := strconv.Unquote(x.Value); err == nil {
									if u == "data-switcher-tmux" {
										spellers[path+" "+vs.Names[k].Name] = true
									}
									if strings.Contains(u, "'data-switcher-tmux'") {
										scripts[path+" "+vs.Names[k].Name] = true
									}
								}
							}
							return true
						})
					}
				}
			}
		}
	}
	for _, c := range []struct {
		what string
		got  map[string]bool
		want []string
	}{
		{"functions that read an entry's TmuxAt / TmuxCode", readers,
			[]string{"nav_agents.go navRowName", "switcher.go entryTmuxAttr", "switcher.go switcherItems", "switcher.go switcherLabel"}},
		{"code that spells the data-switcher-tmux attribute", spellers, []string{"switcher.go entryTmuxAttr"}},
		{"callers of entryTmuxAttr", callers, []string{"nav_agents.go navAgentRow", "switcher.go switcherEntry"}},
		{"scripts that read data-switcher-tmux", scripts, []string{"palette.go paletteMatchJS", "switcher_dock.go dockJS"}},
	} {
		if got := uiSortedKeys(c.got); strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n  got  %v\n  want %v\nThe codename reaches a surface only through the entry helpers (entryTmuxAttr, switcherLabel, navRowName) and the scripts that read their attribute.", c.what, got, c.want)
		}
	}
}
