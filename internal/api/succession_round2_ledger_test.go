package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// #546 ROUND 2: THE KICKOFF IS NEVER IN AN ATTENTION ASK'S QUESTION, by
// construction. Asserted over the type-checked Go source of internal/api,
// each set failing when it GROWS or SHRINKS.

// proposalQuestionBuilders is every function a proposal card's question is
// built by (fileProposal's question argument).
var proposalQuestionBuilders = []string{"endSessionCardQuestion", "successionCardQuestion"}

// TestNoWriterPutsTheKickoffInTheAsk pins the one path from a kickoff to an
// attention ask:
//
//   - the kickoff is copied in one place (kickoffCopy, from the tool);
//   - a proposal's ask is raised only by raiseProposalCard, only from
//     fileProposal, and raiseCard only from it and raiseOwnerCard;
//   - every fileProposal call passes, as the question, a call to a listed
//     builder, and no builder (nor any package function it reaches) reads a
//     plugins.Proposal's Kickoff field except as the one argument of len or
//     utf8.RuneCountInString;
//   - the functions that read that field at all are a ledger: the owner's
//     card renderers, the cut rule, the spawn.
func TestNoWriterPutsTheKickoffInTheAsk(t *testing.T) {
	assertCallers(t, ".", "kickoffCopy", []string{"succession.go:toolSuccessionPropose"})
	assertCallers(t, ".", "raiseProposalCard", []string{"chief_proposal.go:fileProposal"})
	assertCallers(t, ".", "raiseCard", []string{"owner_ask.go:raiseOwnerCard", "owner_ask.go:raiseProposalCard"})

	pkg := typedAPI(t)
	qi := paramIndex(t, pkg.funcs["fileProposal"], "question")
	var builders []string
	for _, fd := range pkg.funcs {
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || calleeName(call) != "fileProposal" {
				return true
			}
			if len(call.Args) <= qi {
				t.Errorf("%s calls fileProposal with %d arguments; its question is argument %d", fd.Name.Name, len(call.Args), qi)
				return true
			}
			q, ok := call.Args[qi].(*ast.CallExpr)
			if !ok {
				t.Errorf("%s passes fileProposal a question that is no builder's call", fd.Name.Name)
				return true
			}
			builders = append(builders, calleeName(q))
			return true
		})
	}
	slices.Sort(builders)
	builders = slices.Compact(builders)
	if !slices.Equal(builders, proposalQuestionBuilders) {
		t.Errorf("proposal questions are built by %v\nwant exactly %v", builders, proposalQuestionBuilders)
	}
	for _, b := range proposalQuestionBuilders {
		if _, ok := pkg.funcs[b]; !ok {
			t.Fatalf("instrument check: no function %s", b)
		}
		for _, bad := range pkg.kickoffReadsReached(b) {
			t.Errorf("the question builder %s reaches %s, which reads the kickoff's text", b, bad)
		}
	}

	var readers []string
	for name, fd := range pkg.funcs {
		if pkg.readsKickoff(fd.Body, false) {
			readers = append(readers, name)
		}
	}
	slices.Sort(readers)
	want := []string{
		"proposalKickoffBody",       // the Attention view's card and the ask page: the owner's own
		"reserveSuccession",         // the successor's prompt
		"startSuccession",           // the successor's prompt room and the sha check
		"successionCard",            // the session view's card
		"successionCardQuestion",    // its length only
		"successionKickoffFitsCard", // the cut rule: its length
	}
	if !slices.Equal(readers, want) {
		t.Errorf("a proposal's Kickoff field is read by %v\nwant exactly %v", readers, want)
	}
}

// paramIndex is the index of fd's parameter named name.
func paramIndex(t *testing.T, fd *ast.FuncDecl, name string) int {
	t.Helper()
	if fd == nil {
		t.Fatal("instrument check: no fileProposal")
	}
	i := 0
	for _, f := range fd.Type.Params.List {
		for _, n := range f.Names {
			if n.Name == name {
				return i
			}
			i++
		}
	}
	t.Fatalf("instrument check: fileProposal has no parameter %q", name)
	return -1
}

// typedPkg is internal/api's non-test source, type-checked.
type typedPkg struct {
	funcs map[string]*ast.FuncDecl // by bare name
	info  *types.Info
}

// typedAPI type-checks internal/api against `go list -export` data (as
// scanGatewayTurns does for the module).
func typedAPI(t *testing.T) typedPkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,Export", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, stderr.String())
	}
	type listed struct {
		ImportPath, Dir, Export string
		GoFiles                 []string
	}
	exports := map[string]string{}
	var self listed
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var lp listed
		if err := dec.Decode(&lp); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[lp.ImportPath] = lp.Export
		self = lp // the package itself is listed last
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(f)
	})
	var files []*ast.File
	for _, name := range self.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(self.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
	if _, err := (&types.Config{Importer: imp}).Check(self.ImportPath, fset, files, info); err != nil {
		t.Fatalf("type-check %s: %v", self.ImportPath, err)
	}
	p := typedPkg{funcs: map[string]*ast.FuncDecl{}, info: info}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				p.funcs[fd.Name.Name] = fd
			}
		}
	}
	return p
}

// isProposalKickoff reports whether sel reads the Kickoff field of a
// plugins.Proposal.
func (p typedPkg) isProposalKickoff(sel *ast.SelectorExpr) bool {
	s, ok := p.info.Selections[sel]
	if !ok || s.Kind() != types.FieldVal || sel.Sel.Name != "Kickoff" {
		return false
	}
	r := s.Recv()
	if ptr, ok := r.(*types.Pointer); ok {
		r = ptr.Elem()
	}
	n, ok := r.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Name() == "plugins" && n.Obj().Name() == "Proposal"
}

// readsKickoff reports whether body reads a proposal's Kickoff field;
// lengthOK lets a read that is the one argument of len or
// utf8.RuneCountInString pass.
func (p typedPkg) readsKickoff(body ast.Node, lengthOK bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && lengthOK && len(call.Args) == 1 {
			if name := calleeName(call); name == "len" || name == "RuneCountInString" {
				if sel, ok := call.Args[0].(*ast.SelectorExpr); ok && p.isProposalKickoff(sel) {
					return false
				}
			}
		}
		if sel, ok := n.(*ast.SelectorExpr); ok && p.isProposalKickoff(sel) {
			found = true
			return false
		}
		return true
	})
	return found
}

// kickoffReadsReached is every package function reachable from root (root
// included) that reads a proposal's kickoff text (anything but its length).
func (p typedPkg) kickoffReadsReached(root string) []string {
	seen := map[string]bool{}
	var bad []string
	var visit func(name string)
	visit = func(name string) {
		fd, ok := p.funcs[name]
		if !ok || seen[name] {
			return
		}
		seen[name] = true
		if p.readsKickoff(fd.Body, true) {
			bad = append(bad, name)
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				visit(calleeName(call))
			}
			return true
		})
	}
	visit(root)
	slices.Sort(bad)
	return bad
}

// calleeName is a call's function or method name.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	}
	return ""
}
