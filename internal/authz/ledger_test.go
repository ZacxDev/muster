package authz

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The two doors that put a Principal on a context are WithPrincipal and
// System. The key is unexported (see the package doc), so these are the ONLY
// doors — and which code may open them is pinned here as a ledger over the
// module's non-test source.
//
// 🔴 EACH LEDGER IS SET EQUALITY, NOT A SUBSET CHECK. A new caller fails it
// (the hazard: a handler minting its own principal, or a request path calling
// System and reading every user's rows) and so does a vanished one (the
// ledger would otherwise go on vouching for a site that no longer exists —
// and a loop that stopped calling System is a loop that, under PR 3, reads an
// empty board).

const (
	modulePath = "github.com/ZacxDev/muster"
	authzPath  = modulePath + "/internal/authz"
)

// systemLedger is every function allowed to reference authz.System: the
// background loops, which act for every user.
//
// ⚠ NOT LISTED, BECAUSE THERE IS NO LOOP TO LIST: ccpool (selection runs inside
// a dispatch request), a GitHub mirror (none exists; cmd/muster-server's
// connectStaticGitHub is a one-shot boot write of the OPERATOR'S connection,
// which PR 3 gives the operator's principal, not System), and a reconciler
// (agents/reconcile.go is a pure decision table that agentstatus drives).
var systemLedger = []string{
	"internal/agentkickoff.(*Deliverer).Run",
	"internal/agentstatus.(*Promoter).Run",
	"internal/api.(*Server).RunTaskReap",
	"internal/api.(*Server).RunTaskRetention",
}

// withPrincipalLedger is every function allowed to reference
// authz.WithPrincipal. ONE: internal/api's admit, which every require*
// wrapper hands its admitted request through (that second ledger lives in
// internal/api, TestOnlyAdmitAttachesAPrincipal).
var withPrincipalLedger = []string{
	"internal/api.(*Server).admit",
}

func TestSystemIsCalledOnlyByTheListedBackgroundLoops(t *testing.T) {
	assertLedger(t, "authz.System", references(t)["System"], systemLedger)
}

func TestWithPrincipalIsCalledOnlyByAdmit(t *testing.T) {
	assertLedger(t, "authz.WithPrincipal", references(t)["WithPrincipal"], withPrincipalLedger)
}

// TestTheScannerSeesAnAliasedImport is the scanner's positive control. A
// reference through a renamed import, as a function VALUE rather than a call,
// at package level rather than inside a func, must all be seen — each is a way
// to reach System that a call-expression grep would miss.
func TestTheScannerSeesAnAliasedImport(t *testing.T) {
	src := `package x
import az "` + authzPath + `"
var grab = az.System
type T struct{}
func (t *T) M() { _ = az.WithPrincipal }
func f() { g := az.System; _ = g }
`
	got := scanFile(t, "internal/x", src)
	want := map[string][]string{
		"System":        {"internal/x.f", "internal/x.grab"},
		"WithPrincipal": {"internal/x.(*T).M"},
	}
	for name, sites := range want {
		if strings.Join(sorted(got[name]), ",") != strings.Join(sites, ",") {
			t.Fatalf("scanner saw %s at %v; want %v", name, sorted(got[name]), sites)
		}
	}
}

func assertLedger(t *testing.T, what string, got map[string]bool, want []string) {
	t.Helper()
	var g []string
	for k := range got {
		g = append(g, k)
	}
	g = sorted(g)
	w := sorted(append([]string(nil), want...))
	if strings.Join(g, "\n") != strings.Join(w, "\n") {
		t.Fatalf("%s is referenced from:\n  %s\nthe ledger lists:\n  %s\n"+
			"A new site is a new way to put a principal on a context — justify it and add it here; "+
			"a missing one is a loop or wrapper that stopped attaching one.",
			what, strings.Join(g, "\n  "), strings.Join(w, "\n  "))
	}
}

func sorted(s []string) []string { sort.Strings(s); return s }

// references scans every non-test .go file in the module and returns, per
// exported authz identifier, the set of enclosing functions that reference it.
func references(t *testing.T) map[string]map[string]bool {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !strings.Contains(string(b), "module "+modulePath+"\n") {
		t.Fatalf("module root %s is not %s (%v)", root, modulePath, err)
	}
	out := map[string]map[string]bool{}
	files := 0
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		files++
		for name, sites := range scanFile(t, filepath.ToSlash(rel), string(b)) {
			if out[name] == nil {
				out[name] = map[string]bool{}
			}
			for _, s := range sites {
				out[name][s] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Positive control on the walk itself: a scan that read nothing would
	// report an empty ledger as a violation, but a scan that read only a few
	// directories would not. The module has well over a hundred source files.
	if files < 100 {
		t.Fatalf("scanned only %d source files under %s; the walk is broken", files, root)
	}
	return out
}

// scanFile returns, for each identifier selected from the authz import, the
// enclosing top-level declarations ("dir.Func", "dir.(*Recv).Method", or
// "dir.varName") that reference it.
func scanFile(t *testing.T, dir, src string) map[string][]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), dir+"/x.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	local := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == authzPath {
			local = "authz"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
	}
	if local == "" || local == "_" {
		return nil
	}
	out := map[string][]string{}
	for _, decl := range f.Decls {
		site := declName(dir, decl)
		seen := map[string]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local && !seen[sel.Sel.Name] {
				seen[sel.Sel.Name] = true
				out[sel.Sel.Name] = append(out[sel.Sel.Name], site)
			}
			return true
		})
	}
	return out
}

func declName(dir string, decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) == 1 {
			switch r := d.Recv.List[0].Type.(type) {
			case *ast.StarExpr:
				if id, ok := r.X.(*ast.Ident); ok {
					return dir + ".(*" + id.Name + ")." + d.Name.Name
				}
			case *ast.Ident:
				return dir + "." + r.Name + "." + d.Name.Name
			}
		}
		return dir + "." + d.Name.Name
	case *ast.GenDecl:
		var names []string
		for _, s := range d.Specs {
			if vs, ok := s.(*ast.ValueSpec); ok {
				for _, n := range vs.Names {
					names = append(names, n.Name)
				}
			}
		}
		return dir + "." + strings.Join(names, ",")
	}
	return dir + ".?"
}
