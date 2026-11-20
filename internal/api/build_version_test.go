package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// apiImportPath is this package's import path: a file elsewhere in the module
// reaches BuildVersion as <its name for this import>.BuildVersion.
const apiImportPath = "github.com/ZacxDev/muster/internal/api"

// buildVersionAssignments lists every assignment to BuildVersion, and every
// &BuildVersion, in one file's source, as "file:line". inAPI says the file is
// this package's own, where the variable is a bare identifier; anywhere else it
// is a selector on the file's name for this package (or bare, under a dot
// import).
func buildVersionAssignments(t *testing.T, fset *token.FileSet, name string, src any, inAPI bool) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	bare := inAPI
	var qual []string // the names this file imports this package as
	for _, im := range f.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); p != apiImportPath {
			continue
		}
		switch {
		case im.Name == nil:
			qual = append(qual, "api")
		case im.Name.Name == ".":
			bare = true
		case im.Name.Name != "_":
			qual = append(qual, im.Name.Name)
		}
	}
	if !bare && len(qual) == 0 {
		return nil
	}
	isBuildVersion := func(e ast.Expr) (token.Pos, bool) {
		switch e := ast.Unparen(e).(type) {
		case *ast.Ident:
			return e.Pos(), bare && e.Name == "BuildVersion"
		case *ast.SelectorExpr:
			x, ok := ast.Unparen(e.X).(*ast.Ident)
			if !ok || e.Sel.Name != "BuildVersion" {
				return 0, false
			}
			for _, q := range qual {
				if x.Name == q {
					return e.Pos(), true
				}
			}
		}
		return 0, false
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		var lhs []ast.Expr
		switch n := n.(type) {
		case *ast.AssignStmt:
			lhs = n.Lhs
		case *ast.IncDecStmt:
			lhs = []ast.Expr{n.X}
		case *ast.UnaryExpr: // &BuildVersion would let a write through a pointer
			if n.Op == token.AND {
				lhs = []ast.Expr{n.X}
			}
		}
		for _, e := range lhs {
			if pos, ok := isBuildVersion(e); ok {
				out = append(out, fset.Position(pos).String())
			}
		}
		return true
	})
	return out
}

// TestNothingAssignsBuildVersion: BuildVersion stays a var only so -ldflags -X
// can stamp it; no Go file in the module — test code included, any package —
// assigns it or takes its address, as BuildVersion here or as
// api.BuildVersion (under any import name) elsewhere. A test that needs
// another build sets its Server's buildVersion (Server.version). Reassigning
// the variable raced every request handler still running from an earlier
// test, since every page reads it.
//
// The CONTROL is sources that do write it, in every form the scan claims to
// catch: it must find each one, so a zero below is a zero from a scan that can
// see one.
func TestNothingAssignsBuildVersion(t *testing.T) {
	fset := token.NewFileSet()
	for _, c := range []struct {
		name  string
		inAPI bool
		src   string
		want  int
	}{
		{"control_api.go", true, "package api\nfunc f() { old := BuildVersion; BuildVersion = \"x\"; (BuildVersion) = \"y\"; p := &BuildVersion; _, _ = old, p }\n", 3},
		{"control_main.go", false, "package main\nimport \"" + apiImportPath + "\"\nfunc f() { old := api.BuildVersion; api.BuildVersion = \"x\"; (api.BuildVersion) = \"y\"; p := &api.BuildVersion; _, _ = old, p }\n", 3},
		{"control_alias.go", false, "package main\nimport srv \"" + apiImportPath + "\"\nfunc f() { srv.BuildVersion = \"x\"; api.BuildVersion = \"not this package\" }\n", 1},
	} {
		if got := buildVersionAssignments(t, fset, c.name, c.src, c.inAPI); len(got) != c.want {
			t.Fatalf("control %s: the scan found %v; want %d", c.name, got, c.want)
		}
	}

	root := moduleRoot(t) // routes_golden_test.go
	apiDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		found = append(found, buildVersionAssignments(t, fset, path, src, filepath.Dir(path) == apiDir)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned < 100 {
		t.Fatalf("control: scanned only %d Go files under %s; the walk is not seeing the module", scanned, root)
	}
	if len(found) > 0 {
		t.Errorf("BuildVersion is assigned at run time:\n  %s\nset the Server's buildVersion instead", strings.Join(found, "\n  "))
	}
}
