package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE HOST-DISPATCH SEAM LEDGERS (docs/design/chief-dispatch.md 3.7, 5.1):
// asserted over the Go source, each failing when its set GROWS or SHRINKS.

// callersIn is every "<file>:<func>" in the non-test Go files of dir whose body
// calls a method or function named name.
func callersIn(t *testing.T, dir, name string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fn.Sel.Name == name {
						out = append(out, filepath.Base(f)+":"+fd.Name.Name)
					}
				case *ast.Ident:
					if fn.Name == name {
						out = append(out, filepath.Base(f)+":"+fd.Name.Name)
					}
				}
				return true
			})
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func assertCallers(t *testing.T, dir, name string, want []string) {
	t.Helper()
	got := callersIn(t, dir, name)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s is called from %v\nwant exactly %v", name, got, want)
	}
}

// TestHostSpawnHasOneDoor: the ONLY code that asks a host to start an agent is
// agent_dispatch's host path (spawnReserved, from toolHostDispatch). No HTTP
// route, token or CLI verb reaches it.
func TestHostSpawnHasOneDoor(t *testing.T) {
	assertCallers(t, ".", "SpawnAgent", []string{"host_dispatch.go:spawnReserved"})
	assertCallers(t, ".", "spawnReserved", []string{"host_dispatch.go:toolHostDispatch"})
	assertCallers(t, ".", "toolHostDispatch", []string{"agent_dispatch.go:toolAgentDispatch"})
}

// TestTheHostDispatchWritersAreALedger: in internal/api the reconciler is the
// only caller of UpdateHostDispatch and SetHostDispatchOrphanNotice;
// EndHostDispatch (the only writer of ended) is reached only through
// endHostDispatch, whose callers are the reconciler (a reply, a listing, the
// sweep) and agent_destroy; the pending insert is ReserveHostDispatch, from
// toolHostDispatch only.
func TestTheHostDispatchWritersAreALedger(t *testing.T) {
	assertCallers(t, ".", "UpdateHostDispatch", []string{"host_dispatch_reconcile.go:applySpawnReply", "host_dispatch_reconcile.go:reconcileListing"})
	assertCallers(t, ".", "SetHostDispatchOrphanNotice", []string{"host_dispatch_reconcile.go:orphanWindow"})
	assertCallers(t, ".", "EndHostDispatch", []string{"host_dispatch.go:endHostDispatch"})
	assertCallers(t, ".", "endHostDispatch", []string{"agent_destroy_tool.go:destroyHostDispatch",
		"host_dispatch_reconcile.go:applySpawnReply", "host_dispatch_reconcile.go:reconcileListing", "host_dispatch_reconcile.go:sweepHostDispatches"})
	assertCallers(t, ".", "ReserveHostDispatch", []string{"host_dispatch.go:toolHostDispatch"})
	// In the store, the SQL that writes host_dispatches is in exactly these
	// four functions.
	src, err := os.ReadFile(filepath.Join("..", "plugins", "host_dispatches.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "host_dispatches.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var writers []string
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		body := string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
		if strings.Contains(body, "INSERT INTO host_dispatches") || strings.Contains(body, "UPDATE host_dispatches") {
			writers = append(writers, fd.Name.Name)
		}
	}
	slices.Sort(writers)
	want := []string{"EndHostDispatch", "ReserveHostDispatch", "SetHostDispatchOrphanNotice", "UpdateHostDispatch"}
	if !slices.Equal(writers, want) {
		t.Errorf("host_dispatches is written by %v\nwant exactly %v", writers, want)
	}
	// And no other file in the store writes it.
	files, _ := filepath.Glob(filepath.Join("..", "plugins", "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || filepath.Base(f) == "host_dispatches.go" {
			continue
		}
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "INSERT INTO host_dispatches") || strings.Contains(string(b), "UPDATE host_dispatches") {
			t.Errorf("%s writes host_dispatches", f)
		}
	}
}
