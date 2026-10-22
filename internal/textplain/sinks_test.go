package textplain

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE TITLE SINK LEDGER.
//
// 🔴 A TITLE IS SHOWN THROUGH Title OR IT IS LEDGERED HERE WITH A REASON. The
// operator's report was "titles sometimes include markdown": not one surface,
// but whichever surface someone had not yet routed through the stripper. A
// table test on Title cannot see a sink that never calls it, so this test
// derives the set of title READS from the source and checks each one:
//
//   - internal/ui, every function: a read of a title-bearing name (titleNames)
//     must sit inside a Title(...) call (or be an argument to one of
//     plainingHelpers, each of which is itself checked to call Title), or be
//     in uiExempt with the reason it is not a display of the title.
//   - internal/api, every function that hand-writes HTML (calls
//     html.EscapeString): the same, against apiExempt. The api package's other
//     title reads build view models (rendered in internal/ui, checked above),
//     JSON/MCP data for agents (not a rendered title) or stored records (never
//     rewritten), so they are out of this ledger by construction.
//   - push: every way a notification leaves the server is reached from
//     pushEmitters only, and goNotify — the one that carries titles — applies
//     Title (TestGoNotifyPlainsTitleAndBody in internal/api holds the
//     behaviour; this holds the structure).
//
// Both directions fail: a new unledgered read (a new sink, or an old one that
// stopped calling Title) and a ledger row that no longer matches a read (a
// stale exemption that would otherwise silently cover the next one).

// titleNames are the selectors and calls that yield a title: a task's, an
// agent's display name, a mirrored window's label/title, a chat session's, a
// focus summary's, an attention item's source.
var titleNames = map[string]bool{
	"Title": true, "DisplayName": true, "Label": true, "DisplayTitle": true,
	"TaskTitle": true,
}

// bareAccessors are the title accessors package ui calls unqualified.
var bareAccessors = map[string]bool{"TaskTitle": true}

// apiTitleNames adds the attention item's fields, which are titles in api's
// hand-written attention HTML.
var apiTitleNames = map[string]bool{"Source": true, "Focus": true}

// plainingHelpers are ui functions that apply Title to their name arguments at
// emit. An argument read passed straight to one counts as covered; each is
// checked below to call Title (or another helper in this set).
var plainingHelpers = map[string]bool{
	"mirrorEntryTitle":    true,
	"mirrorSessionTitle":  true,
	"mirrorTerminalTitle": true,
}

type exemption struct {
	n   int // how many reads of expr in that function
	why string
}

const (
	whyFilter    = "filter haystack / rank key: matched against what the user types, never drawn"
	whyForm      = "edit form value: the raw stored title, edited as written"
	whyStatic    = "a fixed UI label (tab, status, preset, model, runtime, quick key, chip), never author text"
	whyPredicate = "a predicate on the raw value (which subtitle to draw), not a display"
	whyAccessor  = "the accessor itself; every display caller applies Title to its result"
	whyOption    = "an answer option: submitted verbatim as the answer, so shown verbatim"
	whyDispatch  = "a query parameter into the dispatch form; DispatchModalTaskBody applies Title at emit"
	whyPerson    = "a person's (user account's) display name, not a title"
)

// uiExempt: "file func expr" → exemption.
var uiExempt = map[string]exemption{
	"agents.go agentCard a.DisplayName":                {1, whyFilter},
	"mirror.go mirrorCard m.Label":                     {4, whyFilter + "; and " + whyPredicate},
	"mirror.go mirrorCard m.Title":                     {2, whyFilter},
	"agents.go agentFields a.DisplayName":              {2, whyFilter},
	"agents.go agentRenameForm a.DisplayName":          {1, whyForm},
	"agents.go comboOption o.Label":                    {2, whyStatic},
	"agents.go seededComboOption o.Label":              {2, whyStatic},
	"agents.go modelFieldForDisplay m.Label":           {1, whyStatic},
	"chief_sessions.go chiefNewSessionPicker rt.Label": {3, whyStatic},
	"chief_sessions.go sessionModelCombobox m.Label":   {1, whyStatic},
	"components.go sidebar t.Label":                    {1, whyStatic},
	"connectors.go ConnectorForm p.Label":              {1, whyStatic},
	"list_signals.go agentsFilterChips c.Label":        {1, whyStatic},
	"recents.go recentEntry it.Title":                  {2, "recentItem.Title is built through Title by recentItems"},
	"mirror.go mirrorFields m.Label":                   {2, whyFilter},
	"mirror.go mirrorFields m.Title":                   {2, whyFilter},
	"mirror_ask.go askForm o.Label":                    {2, whyOption},
	"mirror_ask.go askReadOnly o.Label":                {1, whyOption},
	"notes.go noteDispatchButton TaskTitle":            {1, whyDispatch},
	"notes.go NotesEditModalBody n.Title":              {1, whyForm},
	"notes.go statusFilterRow taskstatus.Label":        {1, whyStatic},
	"notes.go TaskTitle n.Title":                       {1, whyAccessor},
	"tabs.go Tabs t.Label":                             {1, whyStatic},
	"tags.go tasksFilteredEmpty taskstatus.Label":      {1, whyStatic},
	"terminal.go quickKeyBar k.Label":                  {1, whyStatic},
	"focus.go focusHaystack f.Title":                   {1, whyFilter},
}

// apiExempt: the same, for api's hand-written HTML.
var apiExempt = map[string]exemption{
	"admin.go Server.renderAdminUsers u.DisplayName":                 {3, whyPerson},
	"passkeys.go Server.handleAdminUserPasskeysDelete u.DisplayName": {1, whyPerson},
	"plugins.go Server.renderPluginsPage p.Label":                    {1, "a plugin's configured label (its name), not a title"},
	"push_routes.go Server.pushPreferencesForm e.Label":              {1, whyStatic},
}

// pushEmitters are the only functions that may hand a notification to a push
// service or the router, and why each is safe.
var pushEmitters = map[string]string{
	"push_routes.go Server.sendPush":        "the wrapper goNotify and handlePushTest call",
	"server.go Server.goNotify":             "applies Title to the notification's title and body",
	"push_routes.go Server.handlePushTest":  "a fixed test title and body",
	"devices.go Server.handleDevicePushPut": "a fixed probe payload (devicePushProbe)",
}

func TestTheTitleSinkLedger(t *testing.T) {
	t.Run("ui", func(t *testing.T) {
		checkReads(t, "../ui", titleNames, false, uiExempt)
	})
	t.Run("api", func(t *testing.T) {
		names := map[string]bool{}
		for k := range titleNames {
			names[k] = true
		}
		for k := range apiTitleNames {
			names[k] = true
		}
		checkReads(t, "../api", names, true, apiExempt)
	})
	t.Run("plaining helpers", checkPlainingHelpers)
	t.Run("push emitters", checkPushEmitters)
}

type read struct {
	key     string // "file func expr"
	covered bool
}

func parseDir(t *testing.T, dir string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		out[filepath.Base(f)] = af
	}
	return fset, out
}

func checkReads(t *testing.T, dir string, names map[string]bool, htmlOnly bool, exempt map[string]exemption) {
	fset, files := parseDir(t, dir)
	// 🔴 POSITIVE CONTROL: a walk over nothing reports a clean ledger.
	if len(files) < 20 {
		t.Fatalf("positive control FAILED: only %d files parsed in %s — the walk is not measuring", len(files), dir)
	}
	var reads []read
	for name, f := range files {
		reads = append(reads, titleReads(fset, name, f, names, htmlOnly)...)
	}
	covered, got := 0, map[string]int{}
	for _, r := range reads {
		if r.covered {
			covered++
			continue
		}
		got[r.key]++
	}
	if covered == 0 {
		t.Fatalf("positive control FAILED: no read in %s is covered by Title — either nothing calls it or the walk cannot see the call", dir)
	}
	var problems []string
	for k, n := range got {
		e, ok := exempt[k]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("UNLEDGERED title read %q (×%d): show it through textplain.Title, or ledger it with the reason it is not a display", k, n))
		case e.n != n:
			problems = append(problems, fmt.Sprintf("ledger row %q expects %d raw read(s), found %d", k, e.n, n))
		}
	}
	for k := range exempt {
		if got[k] == 0 {
			problems = append(problems, fmt.Sprintf("STALE ledger row %q: no such raw read any more — delete the row", k))
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("%s: %d title reads, %d through Title, %d ledgered raw", dir, len(reads), covered, len(reads)-covered)
}

// titleReads returns every read of a title-bearing name in f's function
// bodies, and whether each is covered. htmlOnly restricts the walk to
// functions that hand-write HTML.
func titleReads(fset *token.FileSet, file string, f *ast.File, names map[string]bool, htmlOnly bool) []read {
	var out []read
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if htmlOnly && !callsSelector(fn.Body, "html", "EscapeString") {
			continue
		}
		where := file + " " + funcName(fn) + " "
		var stack []ast.Node
		lhs := map[ast.Node]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			if as, ok := n.(*ast.AssignStmt); ok {
				for _, l := range as.Lhs {
					lhs[l] = true
				}
			}
			var hit bool
			switch x := n.(type) {
			case *ast.SelectorExpr:
				// textplain.Title is the rule itself, not a title.
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "textplain" {
					// 🔴 Inspect does not call f(nil) for a node whose f returned
					// false, so pop it here or the stack drifts.
					stack = stack[:len(stack)-1]
					return false
				}
				hit = names[x.Sel.Name] && !lhs[x]
			case *ast.Ident:
				// A bare call of a title accessor (TaskTitle(n) in package ui); a
				// selector's own Sel ident is handled by the case above. Only the
				// accessors: a bare Label(...) is the <label> element.
				if bareAccessors[x.Name] && len(stack) >= 2 {
					if c, ok := stack[len(stack)-2].(*ast.CallExpr); ok && c.Fun == x {
						hit = true
					}
				}
			}
			if !hit {
				return true
			}
			var b strings.Builder
			_ = printer.Fprint(&b, fset, n)
			out = append(out, read{key: where + b.String(), covered: coveredBy(stack)})
			stack = stack[:len(stack)-1] // see above: no f(nil) after a false
			return false
		})
	}
	return out
}

// coveredBy reports whether the innermost node of stack sits inside a
// Title(...) call, or is passed straight to a plaining helper.
func coveredBy(stack []ast.Node) bool {
	for i := len(stack) - 2; i >= 0; i-- {
		c, ok := stack[i].(*ast.CallExpr)
		if !ok {
			continue
		}
		if isTitleCall(c) {
			return true
		}
		// A plaining helper covers only an argument passed STRAIGHT to it:
		// helper(other(m.Label)) draws whatever other returns.
		if id, ok := c.Fun.(*ast.Ident); ok && plainingHelpers[id.Name] && i == len(stack)-2 {
			for _, a := range c.Args {
				if a == stack[i+1] {
					return true
				}
			}
		}
	}
	return false
}

func isTitleCall(c *ast.CallExpr) bool {
	s, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == "textplain" && s.Sel.Name == "Title"
}

func callsSelector(body ast.Node, pkg, sel string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok && id.Name == pkg && s.Sel.Name == sel {
				found = true
			}
		}
		return !found
	})
	return found
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
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

// checkPlainingHelpers: each helper exists and calls Title or another helper,
// so "passed to a helper" cannot mean "passed to something that draws it raw".
func checkPlainingHelpers(t *testing.T) {
	_, files := parseDir(t, "../ui")
	seen := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !plainingHelpers[fn.Name.Name] {
				continue
			}
			seen[fn.Name.Name] = true
			ok = false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, isCall := n.(*ast.CallExpr); isCall {
					if isTitleCall(c) {
						ok = true
					}
					if id, isID := c.Fun.(*ast.Ident); isID && plainingHelpers[id.Name] && id.Name != fn.Name.Name {
						ok = true
					}
				}
				return true
			})
			if !ok {
				t.Errorf("plaining helper %s does not call textplain.Title (nor another plaining helper): its arguments would be drawn raw", fn.Name.Name)
			}
		}
	}
	for h := range plainingHelpers {
		if !seen[h] {
			t.Errorf("plaining helper %s is not declared in internal/ui — delete it from plainingHelpers", h)
		}
	}
}

// checkPushEmitters: the functions that call a push service or the router are
// exactly pushEmitters, and goNotify applies Title.
func checkPushEmitters(t *testing.T) {
	_, files := parseDir(t, "../api")
	got := map[string]bool{}
	for name, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				s, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				var via string
				switch s.Sel.Name {
				case "sendPush", "SendTo", "Probe":
					// s.sendPush, s.ext.Push.SendTo, s.ext.Push.Probe
					if s.Sel.Name == "sendPush" || selectorEndsIn(s.X, "Push") {
						via = s.Sel.Name
					}
				case "Notify":
					if selectorEndsIn(s.X, "router") {
						via = "router.Notify"
					}
				}
				if via != "" {
					got[name+" "+funcName(fn)] = true
				}
				return true
			})
			if funcName(fn) == "Server.goNotify" {
				titled := 0
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok && isTitleCall(c) && len(c.Args) == 1 {
						if s, ok := c.Args[0].(*ast.SelectorExpr); ok && (s.Sel.Name == "Title" || s.Sel.Name == "Body") {
							titled++
						}
					}
					return true
				})
				if titled != 2 {
					t.Errorf("goNotify applies textplain.Title to %d of the notification's Title/Body (want both)", titled)
				}
			}
		}
	}
	if len(got) == 0 {
		t.Fatal("positive control FAILED: no push emitter found at all — the walk is matching nothing")
	}
	var problems []string
	for k := range got {
		if _, ok := pushEmitters[k]; !ok {
			problems = append(problems, fmt.Sprintf("UNLEDGERED push emitter %q: route it through goNotify (which applies Title) or ledger why its text is safe", k))
		}
	}
	for k := range pushEmitters {
		if !got[k] {
			problems = append(problems, fmt.Sprintf("STALE push emitter row %q", k))
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

func selectorEndsIn(x ast.Expr, name string) bool {
	switch v := x.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name == name
	case *ast.Ident:
		return v.Name == name
	}
	return false
}

// TestTheLedgerWalkSeesAnUnplainedRead is the negative control on the walk
// itself: a raw title drawn into a node is reported uncovered, the same read
// inside Title is covered, and a read passed to a plaining helper is covered.
func TestTheLedgerWalkSeesAnUnplainedRead(t *testing.T) {
	src := `package ui
func raw(n Note) Node     { return Text(n.Title) }
func plain(n Note) Node   { return Text(textplain.Title(n.Title)) }
func helped(m View) Node  { return mirrorEntryTitle(m.Label, "", "", "", "") }
func helperArg(m View) Node { return mirrorEntryTitle(other(m.Label), "", "", "", "") }
func write(v *View)       { v.Title = "x" }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range titleReads(fset, "x.go", f, titleNames, false) {
		got[r.key] = r.covered
	}
	want := map[string]bool{
		"x.go raw n.Title":       false,
		"x.go plain n.Title":     true,
		"x.go helped m.Label":    true,
		"x.go helperArg m.Label": false, // not passed STRAIGHT to the helper
	}
	if len(got) != len(want) {
		t.Errorf("walk found %v, want %v (the assignment's LHS must not count as a read)", got, want)
	}
	for k, v := range want {
		if c, ok := got[k]; !ok || c != v {
			t.Errorf("%s: covered=%v (found=%v), want covered=%v", k, c, ok, v)
		}
	}
}
