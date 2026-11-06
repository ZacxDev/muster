package mirrorhost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEveryWayThisHostTypesIntoAPaneGoesThroughThePasteGuard is a SEAM
// LEDGER over this package's source, not a behaviour test: the behaviour is
// TestEveryHostPasteTheGuardRefusesTypesNothing, and this is what keeps it
// complete. The paste guard lives in hostSide.typeInto, so:
//
//  1. every call that types into a pane — PastePane, Paste, PasteChecked (the
//     Claude prompt guard's checked paste), AnswerAsk — is
//     either the tmux backend's own implementation (a Tmux method delegating
//     to another), or sits in the function literal handed to typeInto;
//  2. the typeInto call sites are EXACTLY the three host entry points that
//     type: paste (the Terminal view's and the rendered view's) and answer.
//     A fourth fails here until it is added on purpose; one fewer fails too;
//  3. the tmux commands that put bytes into a pane ("paste-buffer") and drive
//     its copy-mode ("send-keys") are spelled only in the functions listed.
//     A new one fails here, so a new way to type cannot arrive unguarded;
//  4. the typing primitives (pasteBracketed, pasteEnter) are called only by
//     PastePane and PasteChecked (the Claude prompt guard's paste, itself
//     reached only through typeInto). There is no exception to typeInto.
func TestEveryWayThisHostTypesIntoAPaneGoesThroughThePasteGuard(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var guarded, unguarded []string
	literals := map[string][]string{}
	callers := map[string][]string{}
	files := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name, recv := fd.Name.Name, ""
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				switch r := fd.Recv.List[0].Type.(type) {
				case *ast.Ident:
					recv = r.Name
				case *ast.StarExpr:
					if id, ok := r.X.(*ast.Ident); ok {
						recv = id.Name
					}
				}
			}
			where := recv + "." + name
			// Which call expressions are function literals handed to typeInto.
			inTypeInto := map[ast.Node]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "typeInto" {
					for _, a := range call.Args {
						if lit, ok := a.(*ast.FuncLit); ok {
							ast.Inspect(lit, func(m ast.Node) bool { inTypeInto[m] = true; return true })
						}
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch sel.Sel.Name {
					case "pasteBracketed", "pasteEnter":
						callers[sel.Sel.Name] = append(callers[sel.Sel.Name], where)
					}
					switch sel.Sel.Name {
					case "PastePane", "Paste", "PasteChecked", "AnswerAsk", "CancelAsk":
						switch {
						case inTypeInto[x]:
							// counted below, by its typeInto call
						case recv == "Tmux":
							// the backend's own implementation
						default:
							unguarded = append(unguarded, fset.Position(x.Pos()).String()+" "+where+" calls "+sel.Sel.Name)
						}
					case "typeInto":
						guarded = append(guarded, where)
					}
				case *ast.BasicLit:
					if x.Kind != token.STRING {
						return true
					}
					if s, err := strconv.Unquote(x.Value); err == nil && (s == "paste-buffer" || s == "send-keys") {
						literals[s] = append(literals[s], where)
					}
				}
				return true
			})
		}
	}
	if files < 10 {
		t.Fatalf("instrument check: parsed %d source files; is the test running in the package directory?", files)
	}
	if len(unguarded) > 0 {
		t.Fatalf("a call types into a pane WITHOUT the paste guard (hostSide.typeInto):\n  %s", strings.Join(unguarded, "\n  "))
	}
	slices.Sort(guarded)
	if want := []string{"hostSide.answer", "hostSide.cancelAsk", "hostSide.paste", "hostSide.paste"}; !slices.Equal(guarded, want) {
		t.Fatalf("typeInto is called from %q; want exactly %q — update this ledger only for a NEW way of typing that goes through it", guarded, want)
	}
	for name, want := range map[string][]string{
		"pasteBracketed": {"Tmux.PasteChecked", "Tmux.PastePane"},
		"pasteEnter":     {"Tmux.PasteChecked", "Tmux.PastePane"},
	} {
		got := slices.Clone(callers[name])
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s is called from %q; want exactly %q — a typing primitive is reached only through typeInto", name, got, want)
		}
	}
	for s, want := range map[string][]string{
		"paste-buffer": {"Tmux.KeyPaste", "Tmux.pasteBracketed", "Tmux.pasteEnter", "tmuxPane.Send"},
		"send-keys":    {"Tmux.CancelCopyMode", "Tmux.ScrollPane"},
	} {
		got := slices.Clone(literals[s])
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%q is spelled in %q; want exactly %q. A new place that drives tmux into a pane must be reached only through typeInto (or be a copy-mode command), and is added here on purpose", s, got, want)
		}
	}
}
