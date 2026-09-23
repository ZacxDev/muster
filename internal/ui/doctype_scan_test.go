package ui

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

// htmlImportNames returns the local identifiers this file binds to
// maragu.dev/gomponents/html — `h` for `h "maragu.dev/gomponents/html"`, `html`
// for an unaliased import. A DOT import binds no identifier (its Doctype is a
// bare ident, already matched) and a blank import cannot be called at all, so
// neither contributes a name.
func htmlImportNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != gomponentsHTMLPath {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "." || imp.Name.Name == "_" {
				continue
			}
			names[imp.Name.Name] = true
			continue
		}
		// Unaliased: the identifier is the package's own name, which for this one
		// known path is its last path segment.
		names[path[strings.LastIndex(path, "/")+1:]] = true
	}
	return names
}

// doctypeEmitters parses this package's non-test source and returns the name of
// every function whose body calls Doctype(). That call IS the definition of "a
// standalone document" here — a fragment renderer cannot have one, and a
// document cannot lack one.
func doctypeEmitters(t *testing.T) []string {
	t.Helper()
	return doctypeEmittersIn(t, ".")
}

// doctypeEmittersIn is doctypeEmitters against a NAMED directory, so the scan
// itself can be given a synthetic package with known contents and checked —
// see TestTheDoctypeScanSeesBothDeclarationShapes. Reading the real package is
// a claim about the package; only a fixture with a known answer is a claim
// about the scanner.
//
// 🔴 IT WALKS TWO DECLARATION SHAPES, NOT ONE. `file.Decls` yields *ast.FuncDecl
// for `func X() …` but *ast.GenDecl for `var X = func() …`, and the earlier
// FuncDecl-only version therefore MISSED a document held in a package-level var
// entirely — reporting a smaller set, which makes the registry comparison pass
// while the document goes unswept. Both shapes are now walked and named.
//
// ⚠ KNOWN LIMIT, deliberately not papered over: it attributes a Doctype to the
// function whose body contains the CALL. If a shared shell helper ever emits the
// Doctype and the pages call the helper, this reports the HELPER and not the
// pages, and TestEveryStandaloneDocumentIsRegistered fails with a mismatch that
// looks like a stale registry. That is a loud, not a silent, failure — its error
// message says so — but the derivation would then need to follow the call.
//
// 🔴 IT MATCHES A CALL SHAPE, NOT A SPELLING — measured, because the earlier
// version matched only the bare `Doctype` *ast.Ident. Every non-test file in this
// package happens to DOT-import maragu.dev/gomponents/html, so a bare ident is
// what they all produce; but a new file written `h "maragu.dev/gomponents/html"`
// + `h.Doctype(...)` is an *ast.SelectorExpr, and the ident-only walk reported
// NOTHING for it. A real non-test file in this shape, carrying both defects the
// document sweeps exist to catch (no <h1>, and a focusable <div tabindex="0">
// inside an aria-hidden subtree), left `go test ./internal/ui/` GREEN: no
// registry row was demanded, so the page reached no document-level guard —
// F2's failure mode one level down, inside the instrument.
//
// That is not a hypothetical spelling either: documents_test.go itself now
// imports the package that way (brokenSeventhDocument), so the unseen shape is
// already the in-repo template a new file would be copied from.
//
// So the selector half resolves the IMPORT rather than trusting the receiver's
// name: `x.Doctype(...)` counts only when `x` is bound, in that file, to
// maragu.dev/gomponents/html. An unrelated `cfg.Doctype()` is not a document.
func doctypeEmittersIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var found []string

	// emitsDoctypeWith is parameterised by the file's own gomponents/html import
	// names, so the selector shape is resolved per file rather than by spelling.
	emitsDoctypeWith := func(htmlPkgNames map[string]bool) func(ast.Node) bool {
		return func(n ast.Node) bool {
			emits := false
			ast.Inspect(n, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					// A dot-import of gomponents/html (what every non-test file in this
					// package uses today).
					if fn.Name == "Doctype" {
						emits = true
						return false
					}
				case *ast.SelectorExpr:
					// `h.Doctype(...)` / `hh.Doctype(...)` — a NAMED import of the same
					// package. Only counted when the receiver resolves to that import.
					if fn.Sel == nil || fn.Sel.Name != "Doctype" {
						return true
					}
					recv, ok := fn.X.(*ast.Ident)
					if !ok || !htmlPkgNames[recv.Name] {
						return true
					}
					emits = true
					return false
				}
				return true
			})
			return emits
		}
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		emitsDoctype := emitsDoctypeWith(htmlImportNames(file))
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil && emitsDoctype(d.Body) {
					found = append(found, d.Name.Name)
				}
			case *ast.GenDecl:
				// `var X = func() g.Node { … Doctype … }` / `var X = someWrapper(func() …)`.
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, val := range vs.Values {
						if i >= len(vs.Names) || !emitsDoctype(val) {
							continue
						}
						found = append(found, vs.Names[i].Name)
					}
				}
			}
		}
	}
	sort.Strings(found)
	return found
}

// gomponentsHTMLPath is the one package whose Doctype() declares a standalone
// document. Naming it is what keeps the selector half of the scan STRUCTURAL: it
// is the import that is matched, never the receiver's spelling.
const gomponentsHTMLPath = "maragu.dev/gomponents/html"
