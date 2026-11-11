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
// built by (fileProposal's question argument); proposalOptionBuilders every
// one its buttons are (its askOptions argument, when that is no literal list
// of constants).
var (
	proposalQuestionBuilders = []string{"endSessionCardQuestion", "successionCardQuestion"}
	proposalOptionBuilders   = []string{"successionAskOptions"}
)

// TestNoWriterPutsTheKickoffInTheAsk pins the one path from a kickoff to an
// attention ask:
//
//   - the kickoff is copied in one place (kickoffCopy, from the tool);
//   - a proposal's ask is raised only by raiseProposalCard, only from
//     fileProposal, and raiseCard only from it and raiseOwnerCard;
//   - every fileProposal call passes, as the question, a call to a listed
//     builder, and as the options a call to a listed builder or a literal
//     list of constants; no builder (nor any package function it reaches)
//     reads a plugins.Proposal's Kickoff field except as the one argument of
//     len or utf8.RuneCountInString;
//   - kickoffCopy's result (and any variable assigned from it) appears in no
//     fileProposal call's question or options, at any depth (round 3);
//   - the functions that read that field at all are a ledger: the owner's
//     card renderers, the cut rule, the spawn.
func TestNoWriterPutsTheKickoffInTheAsk(t *testing.T) {
	assertCallers(t, ".", "kickoffCopy", []string{"succession.go:toolSuccessionPropose"})
	// routing_rule_propose raises its card here too: its question is
	// ruleCardQuestion over a plugins.RuleNew, which carries no kickoff (a
	// rule is a match and an action copied from a plugin message).
	// agent_dispatch's APPROVAL card (dispatch_approval.go) too: raised
	// unnotified like a proposal's, then notified with no answer actions
	// (proposalCardFiled). Its question is approvalQuestion over the
	// dispatch's own arguments: no succession, so no kickoff.
	assertCallers(t, ".", "raiseProposalCard", []string{"chief_proposal.go:fileProposal", "host_dispatch.go:fileHeldDispatch",
		"routing_rules.go:toolRoutingRulePropose"})
	assertCallers(t, ".", "raiseCard", []string{"owner_ask.go:raiseOwnerCard", "owner_ask.go:raiseProposalCard"})

	pkg := typedAPI(t)
	tainted := pkg.kickoffTaint()
	if len(tainted) == 0 {
		t.Fatal("instrument check: no variable holds kickoffCopy's result")
	}
	qi := paramIndex(t, pkg.funcs["fileProposal"], "question")
	oi := paramIndex(t, pkg.funcs["fileProposal"], "askOptions")
	var builders, optBuilders []string
	for _, fd := range pkg.funcs {
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || calleeName(call) != "fileProposal" {
				return true
			}
			if len(call.Args) <= qi || len(call.Args) <= oi {
				t.Errorf("%s calls fileProposal with %d arguments", fd.Name.Name, len(call.Args))
				return true
			}
			for _, arg := range []ast.Expr{call.Args[qi], call.Args[oi]} {
				if v := pkg.taintedIn(arg, tainted); v != "" {
					t.Errorf("%s passes fileProposal the kickoff (%s) in its question or options", fd.Name.Name, v)
				}
			}
			q, ok := call.Args[qi].(*ast.CallExpr)
			if !ok {
				t.Errorf("%s passes fileProposal a question that is no builder's call", fd.Name.Name)
			} else {
				builders = append(builders, calleeName(q))
			}
			switch o := call.Args[oi].(type) {
			case *ast.CallExpr:
				optBuilders = append(optBuilders, calleeName(o))
			case *ast.CompositeLit:
				for _, e := range o.Elts {
					if _, ok := e.(*ast.Ident); !ok {
						if _, ok := e.(*ast.BasicLit); !ok {
							t.Errorf("%s passes fileProposal an option that is no constant", fd.Name.Name)
						}
					}
				}
			default:
				t.Errorf("%s passes fileProposal options that are neither a builder's call nor a literal list", fd.Name.Name)
			}
			return true
		})
	}
	slices.Sort(builders)
	builders = slices.Compact(builders)
	if !slices.Equal(builders, proposalQuestionBuilders) {
		t.Errorf("proposal questions are built by %v\nwant exactly %v", builders, proposalQuestionBuilders)
	}
	slices.Sort(optBuilders)
	optBuilders = slices.Compact(optBuilders)
	if !slices.Equal(optBuilders, proposalOptionBuilders) {
		t.Errorf("proposal options are built by %v\nwant exactly %v", optBuilders, proposalOptionBuilders)
	}
	for _, b := range append(slices.Clone(proposalQuestionBuilders), proposalOptionBuilders...) {
		if _, ok := pkg.funcs[b]; !ok {
			t.Fatalf("instrument check: no function %s", b)
		}
		for _, bad := range pkg.kickoffReadsReached(b) {
			t.Errorf("the builder %s reaches %s, which reads the kickoff's text", b, bad)
		}
	}

	// agent_dispatch's approval card, the one other raiseProposalCard caller
	// that builds a question of its own (fileHeldDispatch): nothing in its
	// body holds kickoffCopy's result, at any depth, and no package function
	// it reaches reads a Proposal's Kickoff. Pinned here, not by the comment
	// on its caller entry above.
	fhd, ok := pkg.funcs["fileHeldDispatch"]
	if !ok {
		t.Fatal("instrument check: no function fileHeldDispatch")
	}
	raises := 0
	ast.Inspect(fhd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == "raiseProposalCard" {
			raises++
		}
		return true
	})
	if raises != 1 {
		t.Errorf("instrument check: fileHeldDispatch calls raiseProposalCard %d times; want 1", raises)
	}
	if v := pkg.taintedIn(fhd.Body, tainted); v != "" {
		t.Errorf("fileHeldDispatch holds the kickoff (%s): an approval card's question or options could carry it", v)
	}
	for _, bad := range pkg.kickoffReadsReached("fileHeldDispatch") {
		t.Errorf("fileHeldDispatch reaches %s, which reads the kickoff's text", bad)
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

// kickoffTaint is every variable that holds kickoffCopy's result: the first
// result of a call to it, and — to a fixpoint — any variable assigned from an
// expression using one (a composite literal excepted: a Proposal's Kickoff
// field is the field-read rule's).
func (p typedPkg) kickoffTaint() map[types.Object]bool {
	tainted := map[types.Object]bool{}
	obj := func(e ast.Expr) types.Object {
		id, ok := e.(*ast.Ident)
		if !ok {
			return nil
		}
		if o := p.info.Defs[id]; o != nil {
			return o
		}
		return p.info.Uses[id]
	}
	for changed := true; changed; {
		changed = false
		for _, fd := range p.funcs {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Rhs) == 0 {
					return true
				}
				rhs := as.Rhs[0]
				from := false
				if call, ok := rhs.(*ast.CallExpr); ok && calleeName(call) == "kickoffCopy" {
					from = true
				} else if _, lit := rhs.(*ast.CompositeLit); !lit && len(as.Rhs) == len(as.Lhs) {
					for i, r := range as.Rhs {
						if _, lit := r.(*ast.CompositeLit); !lit && p.taintedIn(r, tainted) != "" {
							if o := obj(as.Lhs[i]); o != nil && !tainted[o] {
								tainted[o], changed = true, true
							}
						}
					}
					return true
				}
				if from {
					if o := obj(as.Lhs[0]); o != nil && !tainted[o] {
						tainted[o], changed = true, true
					}
				}
				return true
			})
		}
	}
	return tainted
}

// taintedIn is the name of a tainted variable e uses, at any depth, "" for
// none.
func (p typedPkg) taintedIn(e ast.Node, tainted map[types.Object]bool) string {
	found := ""
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && found == "" && tainted[p.info.Uses[id]] {
			found = id.Name
		}
		return found == ""
	})
	return found
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
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
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
