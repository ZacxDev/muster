package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// doneBeforeRecover lists each `defer <x>.Done()` in a safeGo/safeGoDone call's function
// literal that has no `defer recoverGoroutine(…)` after it in that literal:
// a Done that unwinds BEFORE safeGo's recover, releasing its waiter while the
// panic is still being logged. Nested function literals are not walked (their
// defers belong to them).
func doneBeforeRecover(fset *token.FileSet, file *ast.File) (bad []string, calls int) {
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		// fn is safeGo's third argument, safeGoDone's fourth (a Done of its
		// own besides the WaitGroup safeGoDone releases is the same hazard).
		fnArg := map[string]int{"safeGo": 2, "safeGoDone": 3}[id.Name]
		if fnArg == 0 || len(call.Args) != fnArg+1 {
			return true
		}
		lit, ok := call.Args[fnArg].(*ast.FuncLit)
		if !ok {
			return true
		}
		calls++
		var dones []token.Pos
		lastRecover := token.NoPos
		ast.Inspect(lit.Body, func(m ast.Node) bool {
			if _, nested := m.(*ast.FuncLit); nested {
				return false
			}
			d, ok := m.(*ast.DeferStmt)
			if !ok {
				return true
			}
			switch f := d.Call.Fun.(type) {
			case *ast.SelectorExpr:
				if f.Sel.Name == "Done" && len(d.Call.Args) == 0 {
					dones = append(dones, d.Pos())
				}
			case *ast.Ident:
				if f.Name == "recoverGoroutine" {
					lastRecover = d.Pos()
				}
			}
			return true
		})
		for _, p := range dones {
			if lastRecover == token.NoPos || lastRecover < p {
				bad = append(bad, fset.Position(p).String())
			}
		}
		return true
	})
	return bad, calls
}

// TestNoSafeGoReleasesAWaiterBeforeItsPanicIsLogged: no goroutine in this
// package runs as safeGo(…, func() { defer x.Done(); … }) without a
// recoverGoroutine deferred after that Done — use safeGoDone. That shape let
// WaitForOfficerReprompts (and 17 other waits) return while a panic was still
// being logged (safeGoDone's comment).
func TestNoSafeGoReleasesAWaiterBeforeItsPanicIsLogged(t *testing.T) {
	// NEGATIVE CONTROL: the walk must flag the shape, and accept the fixed one.
	const src = `package p
func a() { safeGo(l, "bad", func() { defer wg.Done(); work() }) }
func b() { safeGo(l, "ok", func() { if w != nil { defer w.Done() }; defer recoverGoroutine(l, "ok"); work() }) }
func c() { safeGo(l, "late", func() { defer recoverGoroutine(l, "late"); defer wg.Done(); work() }) }
func d() { safeGoDone(l, "two", &wg, func() { defer other.Done(); work() }) }
`
	cfset := token.NewFileSet()
	cf, err := parser.ParseFile(cfset, "control.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bad, calls := doneBeforeRecover(cfset, cf); calls != 4 || len(bad) != 3 ||
		!strings.HasPrefix(bad[0], "control.go:2:") || !strings.HasPrefix(bad[1], "control.go:4:") ||
		!strings.HasPrefix(bad[2], "control.go:5:") {
		t.Fatalf("control FAILED: over 4 calls (lines 2, 4 and 5 bad) the walk saw %d and flagged %v", calls, bad)
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	total := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		bad, calls := doneBeforeRecover(fset, file)
		total += calls
		for _, b := range bad {
			t.Errorf("%s: a safeGo goroutine defers a Done that runs BEFORE its panic is logged; use safeGoDone (or defer recoverGoroutine below the Done)", b)
		}
	}
	// POSITIVE CONTROL: the walk saw this package's safeGo calls at all.
	if total < 20 {
		t.Fatalf("positive control FAILED: only %d safeGo calls with a function literal found; the walk is not measuring", total)
	}
	t.Logf("%d safeGo calls with a function literal walked", total)
}
