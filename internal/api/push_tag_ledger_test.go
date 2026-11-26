package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE PUSH TAG LEDGER (push_tags.go; docs/push.md "When a notification is
// withdrawn").
//
// 🔴 A NOTIFICATION WHOSE THING IS GONE MUST GO WITH IT — or be declared as
// one nothing ever withdraws. A fleet proposal's push stayed on a phone after
// its proposal expired (its tap opened "That proposal expired"); connector
// requests and plugin pairings had the same gap. Each was a tag family added
// with a send and no withdrawal, and nothing noticed.
//
// So every tag family this package sends is found HERE, from the source —
// every RouterNotification and push.Payload literal, and every assignment to a
// `.Tag`, in the package's non-test files — and held to pushTagFamilies:
//
//   - a family sent but not declared fails (the set GREW);
//   - a family declared but no longer sent fails (the set SHRANK);
//   - a family declared cleared must have AT LEAST ONE WITHDRAWAL in the
//     source: a literal of Type push.TypeResolved whose tag is that same
//     family, or a withdrawSettled call naming it (push_order.go) — so the
//     declaration "it is withdrawn" is checked against the code. ⚠ Not that
//     every way the thing stops waiting reaches that withdrawal: each family's
//     behavioural tests hold that;
//   - a family declared informational must have none (else it is cleared,
//     and its declaration is wrong).
//
// A tag is reduced to its family by its expression: a string literal is
// itself; a named constant is its value; a call is its function ("draftTag()");
// a concatenation keeps its literal parts with "*" for the rest
// ("connector-*"); fmt.Sprintf is its format; a local variable is every value
// assigned to it in its function. What cannot be reduced is named by its
// expression, and must be declared like any other.

// pushTagSite is one place a tag is set.
type pushTagSite struct {
	family   string
	resolved bool
	at       string
}

func TestEveryPushTagFamilyIsDeclaredWithItsClearPath(t *testing.T) {
	sites := scanPushTagSites(t, ".")
	sent := map[string][]pushTagSite{}
	for _, s := range sites {
		sent[s.family] = append(sent[s.family], s)
	}
	// Positive control: the scan sees the families this file's own story is
	// about. A scan that found nothing would pass every check below.
	for _, must := range []string{fleetPushTag, "connector-*", "pair-*", "attention-*"} {
		if len(sent[must]) == 0 {
			t.Fatalf("POSITIVE CONTROL: the scan found no site for %q; it is not reading the source (found %d sites)", must, len(sites))
		}
	}
	for _, fam := range ptlKeys(sent) {
		decl, ok := pushTagFamilies[fam]
		if !ok {
			t.Errorf("push tag family %q (%s) is not in pushTagFamilies (push_tags.go): declare how its notification is withdrawn when its thing is gone, or that it is informational and never withdrawn",
				fam, sent[fam][0].at)
			continue
		}
		withdrawn := false
		for _, s := range sent[fam] {
			if s.resolved {
				withdrawn = true
			}
		}
		switch {
		case (decl.Cleared == "") == (decl.Informational == ""):
			t.Errorf("pushTagFamilies[%q]: set exactly one of Cleared (how it is withdrawn) or Informational (why it never is)", fam)
		case decl.Cleared != "" && !withdrawn:
			t.Errorf("pushTagFamilies[%q] says it is cleared (%s), but no withdrawal (Type push.TypeResolved) sends that family: its notification is never withdrawn",
				fam, decl.Cleared)
		case decl.Informational != "" && withdrawn:
			t.Errorf("pushTagFamilies[%q] says it is informational, but a withdrawal sends it (%s): declare how it is cleared instead",
				fam, ptlFirstResolved(sent[fam]))
		}
	}
	for _, fam := range ptlKeys(pushTagFamilies) {
		if len(sent[fam]) == 0 {
			t.Errorf("pushTagFamilies declares %q, but no push sends it any more: remove its declaration", fam)
		}
	}
}

func ptlFirstResolved(sites []pushTagSite) string {
	for _, s := range sites {
		if s.resolved {
			return s.at
		}
	}
	return ""
}

func ptlKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// scanPushTagSites parses dir's non-test Go files and returns every tag site.
func scanPushTagSites(t *testing.T, dir string) []pushTagSite {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	consts := map[string]string{}
	helpers := map[string]*ast.FuncDecl{}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, n, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		ptlConsts(f, consts)
		ptlHelpers(f, helpers)
	}
	var out []pushTagSite
	for _, f := range files {
		// Every declaration: a function's body, and a package-level var (a
		// literal there is a send site too, with no locals to resolve).
		for _, d := range f.Decls {
			fn, _ := d.(*ast.FuncDecl)
			var root ast.Node = d
			if fn != nil {
				if fn.Body == nil {
					continue
				}
				root = fn.Body
			}
			r := tagResolver{consts: consts, helpers: helpers, fn: fn}
			ast.Inspect(root, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CompositeLit:
					if !ptlIsPushLiteral(n.Type) {
						return true
					}
					var tag, typ ast.Expr
					for _, e := range n.Elts {
						kv, ok := e.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						switch ptlKeyName(kv.Key) {
						case "Tag":
							tag = kv.Value
						case "Type":
							typ = kv.Value
						}
					}
					at := fset.Position(n.Pos()).String()
					if tag == nil {
						out = append(out, pushTagSite{family: "<no tag>", at: ptlPos(at)})
						return true
					}
					// The sinks forward their caller's tag: not a family of
					// their own (their callers' sites are read instead).
					if fn != nil && ((fn.Name.Name == "goNotify" && ptlRender(tag) == "m.Tag") ||
						(fn.Name.Name == "withdrawSettled" && ptlRender(tag) == "tag")) {
						return true
					}
					resolved := typ != nil && ptlRender(typ) == "push.TypeResolved"
					for _, fam := range r.families(tag, 0) {
						out = append(out, pushTagSite{family: fam, resolved: resolved, at: ptlPos(at)})
					}
				case *ast.CallExpr:
					// withdrawSettled(ctx, owner, id, TAG, gone) sends a
					// withdrawal of TAG (push_order.go): a resolved site.
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "withdrawSettled" && len(n.Args) == 5 {
						for _, fam := range r.families(n.Args[3], 0) {
							out = append(out, pushTagSite{family: fam, resolved: true, at: ptlPos(fset.Position(n.Pos()).String())})
						}
					}
				case *ast.AssignStmt:
					for i, l := range n.Lhs {
						sel, ok := l.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "Tag" || i >= len(n.Rhs) {
							continue
						}
						for _, fam := range r.families(n.Rhs[i], 0) {
							out = append(out, pushTagSite{family: fam, at: ptlPos(fset.Position(n.Pos()).String())})
						}
					}
				}
				return true
			})
		}
	}
	return out
}

func ptlPos(p string) string {
	return filepath.Base(p)
}

func ptlIsPushLiteral(e ast.Expr) bool {
	switch r := ptlRender(e); r {
	case "RouterNotification", "push.Payload":
		return true
	}
	return false
}

func ptlKeyName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// collectStringConsts records f's package-level string constants.
func ptlConsts(f *ast.File, into map[string]string) {
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							into[n.Name] = v
						}
					}
				}
			}
		}
	}
}

// tagResolver reduces a tag expression in fn to its families.
type tagResolver struct {
	consts  map[string]string
	helpers map[string]*ast.FuncDecl
	fn      *ast.FuncDecl
}

// ptlHelpers records f's one-line tag helpers: a plain function whose body
// is `return <expr>` ("func draftTag(id int64) string { return ... }"), so a
// tag built by one reduces to the same family as the same tag spelled inline.
func ptlHelpers(f *ast.File, into map[string]*ast.FuncDecl) {
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil || len(fn.Body.List) != 1 {
			continue
		}
		if ret, ok := fn.Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			into[fn.Name.Name] = fn
		}
	}
}

// sprintfVerb is a fmt verb in a tag's format: a part that varies.
var sprintfVerb = regexp.MustCompile(`%[-+# 0-9.]*[dsvqx]`)

func (r tagResolver) families(e ast.Expr, depth int) []string {
	if depth > 4 {
		return []string{ptlRender(e)}
	}
	switch e := e.(type) {
	case *ast.BasicLit:
		if v, err := strconv.Unquote(e.Value); err == nil {
			return []string{v}
		}
	case *ast.ParenExpr:
		return r.families(e.X, depth+1)
	case *ast.Ident:
		if v, ok := r.consts[e.Name]; ok {
			return []string{v}
		}
		if vals := r.assigned(e.Name); len(vals) > 0 {
			var out []string
			for _, v := range vals {
				out = append(out, r.families(v, depth+1)...)
			}
			return out
		}
	case *ast.CallExpr:
		if ptlRender(e.Fun) == "fmt.Sprintf" && len(e.Args) > 0 {
			if lit, ok := e.Args[0].(*ast.BasicLit); ok {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					return []string{sprintfVerb.ReplaceAllString(v, "*")}
				}
			}
		}
		if id, ok := e.Fun.(*ast.Ident); ok {
			if h, ok := r.helpers[id.Name]; ok {
				hr := tagResolver{consts: r.consts, helpers: r.helpers, fn: h}
				fams := hr.families(h.Body.List[0].(*ast.ReturnStmt).Results[0], depth+1)
				// A family with no fixed start ("*-*") names nothing: the
				// helper's name does.
				if len(fams) == 1 && !strings.HasPrefix(fams[0], "*") && !strings.ContainsAny(fams[0], "()?") {
					return fams
				}
			}
		}
		return []string{ptlRender(e.Fun) + "()"}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return []string{r.concat(e)}
		}
	}
	return []string{ptlRender(e)}
}

// concat is a + chain with its literal parts kept and the rest "*".
func (r tagResolver) concat(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return r.concat(e.X) + r.concat(e.Y)
		}
	case *ast.BasicLit:
		if v, err := strconv.Unquote(e.Value); err == nil {
			return v
		}
	case *ast.Ident:
		if v, ok := r.consts[e.Name]; ok {
			return v
		}
	}
	return "*"
}

// assigned is every value fn assigns to the local name.
func (r tagResolver) assigned(name string) []ast.Expr {
	var out []ast.Expr
	if r.fn == nil {
		return nil
	}
	ast.Inspect(r.fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == name && len(n.Rhs) == len(n.Lhs) {
					out = append(out, n.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if id.Name == name && i < len(n.Values) {
					out = append(out, n.Values[i])
				}
			}
		}
		return true
	})
	return out
}

// render is e as source, roughly (enough to name it).
func ptlRender(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return ptlRender(e.X) + "." + e.Sel.Name
	case *ast.BasicLit:
		return e.Value
	case *ast.CallExpr:
		return ptlRender(e.Fun) + "()"
	case *ast.StarExpr:
		return "*" + ptlRender(e.X)
	case *ast.IndexExpr:
		return ptlRender(e.X) + "[]"
	case *ast.ParenExpr:
		return ptlRender(e.X)
	}
	return "?"
}
