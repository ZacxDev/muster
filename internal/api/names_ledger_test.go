package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sessionNameAdapters are names.go's four doors to the one name rule
// (internal/sessionname): the only functions that may call sessionname.Agent
// or sessionname.Mirror.
var sessionNameAdapters = []string{
	"names.go focusIndex.agentName",
	"names.go focusIndex.mirrorName",
	"names.go Server.agentName",
	"names.go Server.mirrorName",
}

// sessionNameCallers is EVERY function in this package that names a session
// through an adapter — each a surface that shows or speaks a session's name.
//
// 🔴 A LEDGER, NOT A SPOT CHECK: it fails when the set GROWS (a new surface
// is a decision: add it here, with what it is) or SHRINKS (a surface that
// stopped asking the rule now names sessions some other way, or not at all).
var sessionNameCallers = []string{
	"agents.go Server.cardViewIndexed",           // the sidebar rows, switcher pills and palette (agents)
	"agents.go Server.handleAgentDetail",         // the agent page's header and <title>
	"mirror.go Server.mirrorCards",               // the sidebar rows, switcher pills and palette (mirrored)
	"mirror.go Server.handleMirrorTerminalPage",  // a mirrored terminal page's header and <title>
	"mirror_view.go Server.handleMirrorViewPage", // the Rendered view's header and <title>
	"terminal.go Server.handleAgentTerminalPage", // an agent's terminal page's header and <title>
}

// TestTheSessionNameCallerLedger pins the SEAM between the name rule and the
// surfaces: (1) in the whole module, only names.go's adapters call
// sessionname.Agent / sessionname.Mirror; (2) in this package, exactly the
// ledgered functions call an adapter. A positive control proves the walk can
// see a call: names.go's own adapters ARE found calling the rule.
func TestTheSessionNameCallerLedger(t *testing.T) {
	ruleCallers := map[string]bool{}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "e2e", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, fn := range nameLedgerCallers(t, path, func(c *ast.CallExpr) bool {
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			id, ok := sel.X.(*ast.Ident)
			return ok && id.Name == "sessionname" && (sel.Sel.Name == "Agent" || sel.Sel.Name == "Mirror")
		}) {
			rel, _ := filepath.Rel(root, path)
			if filepath.Dir(rel) == filepath.Join("internal", "api") {
				fn = filepath.Base(rel) + " " + fn
			} else {
				fn = rel + " " + fn
			}
			ruleCallers[fn] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ruleCallers["names.go Server.agentName"] {
		t.Fatalf("positive control FAILED: the walk did not find names.go's adapter calling the rule (found %v)", nameLedgerKeys(ruleCallers))
	}
	if got, want := nameLedgerKeys(ruleCallers), nameLedgerSorted(sessionNameAdapters); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("callers of sessionname.Agent / sessionname.Mirror:\n  got  %v\n  want %v\nOnly names.go's adapters may apply the rule; a surface asks an adapter.", got, want)
	}

	adapterCallers := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || path == "names.go" {
			continue
		}
		for _, fn := range nameLedgerCallers(t, path, func(c *ast.CallExpr) bool {
			sel, ok := c.Fun.(*ast.SelectorExpr)
			return ok && (sel.Sel.Name == "agentName" || sel.Sel.Name == "mirrorName")
		}) {
			adapterCallers[path+" "+fn] = true
		}
	}
	if got, want := nameLedgerKeys(adapterCallers), nameLedgerSorted(sessionNameCallers); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("functions that name a session through an adapter:\n  got  %v\n  want %v\nA new surface goes on the ledger with what it is; a dropped one is removed with why.", got, want)
	}
}

// callersOf is the functions of the file at path ("Recv.Name" or "Name")
// containing a call match accepts.
func nameLedgerCallers(t *testing.T, path string, match func(*ast.CallExpr) bool) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && match(c) {
				found = true
			}
			return !found
		})
		if found {
			out = append(out, nameLedgerFunc(fn))
		}
	}
	return out
}

func nameLedgerFunc(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

func nameLedgerKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func nameLedgerSorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
