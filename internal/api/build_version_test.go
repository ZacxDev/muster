package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildVersionAssignments lists every assignment to BuildVersion in one file's
// source, as "file:line".
func buildVersionAssignments(t *testing.T, fset *token.FileSet, name string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		var lhs []ast.Expr
		switch n := n.(type) {
		case *ast.AssignStmt:
			lhs = n.Lhs
		case *ast.UnaryExpr: // &BuildVersion would let a write through a pointer
			if n.Op == token.AND {
				lhs = []ast.Expr{n.X}
			}
		}
		for _, e := range lhs {
			if id, ok := e.(*ast.Ident); ok && id.Name == "BuildVersion" {
				out = append(out, fset.Position(id.Pos()).String())
			}
		}
		return true
	})
	return out
}

// TestNothingAssignsBuildVersion: BuildVersion stays a var only so -ldflags -X
// can stamp it; no code in this package — test code included — assigns it or
// takes its address. A test that needs another build sets its Server's
// buildVersion (Server.version). Reassigning the variable raced every request
// handler still running from an earlier test, since every page reads it.
//
// The CONTROL is a source that does assign it: the scan must find it, so a
// zero below is a zero from a scan that can see one.
func TestNothingAssignsBuildVersion(t *testing.T) {
	fset := token.NewFileSet()
	control := buildVersionAssignments(t, fset, "control.go",
		"package api\nfunc f() { old := BuildVersion; BuildVersion = \"x\"; p := &BuildVersion; _, _ = old, p }\n")
	if len(control) != 2 {
		t.Fatalf("control: the scan found %v in a source with one assignment and one &; want 2", control)
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list the package's files: %v (%d)", err, len(files))
	}
	var found []string
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, buildVersionAssignments(t, fset, name, src)...)
	}
	if len(found) > 0 {
		t.Errorf("BuildVersion is assigned at run time:\n  %s\nset the Server's buildVersion instead", strings.Join(found, "\n  "))
	}
}
