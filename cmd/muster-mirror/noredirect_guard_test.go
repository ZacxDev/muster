package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 🔴 EVERY HTTP CLIENT THE MIRROR BINARY BUILDS REFUSES REDIRECTS. Its
// clients carry credentials (the mirror token, the opencode secret, a Hermes
// ticket), which net/http re-sends to a redirect Location; internal/noredirect
// is the one rule. This guard reads the source of every muster package
// linked into muster-mirror and fails:
//
//   - an http.Client literal that is not the direct argument of
//     noredirect.Client, and new(http.Client);
//   - http.DefaultClient, and http.Get/Head/Post/PostForm (which use it);
//   - a websocket.DialOptions literal with no HTTPClient (it falls back to
//     http.DefaultClient).
//
// And it pins the LEDGER of noredirect.Client call sites, so the set of
// clients cannot grow or shrink without this file saying so.

const (
	modulePath     = "github.com/ZacxDev/muster"
	noredirectPath = modulePath + "/internal/noredirect"
	websocketPath  = "github.com/coder/websocket"
)

// wantNoRedirectSites is every noredirect.Client call in the mirror binary,
// by file (relative to the module root): the ledger.
var wantNoRedirectSites = map[string]int{
	"cmd/muster-mirror/ask.go":        1, // the AskUserQuestion hook → muster /mirror/ask (mirror token)
	"internal/askq/hook.go":           1, // the hook's request, whatever client it was given
	"internal/hermeshost/viewer.go":   1, // the Hermes gateway WebSocket (ticket subprotocol)
	"internal/mirrorhost/client.go":   1, // the tunnel's WebSocket connect (mirror token)
	"internal/mirrorhost/mcpproxy.go": 1, // the MCP proxy's forward (mirror token)
	"internal/opencodehost/client.go": 1, // the opencode server client (pair token / password)
}

// clientScan is what scanFile found in one file.
type clientScan struct {
	violations []string
	wrapped    int // noredirect.Client calls
}

func scanFile(fset *token.FileSet, f *ast.File) clientScan {
	names := map[string]string{} // local import name -> path
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = p
	}
	is := func(e ast.Expr, pkg, sel string) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != sel {
			return false
		}
		id, ok := s.X.(*ast.Ident)
		return ok && names[id.Name] == pkg
	}
	var out clientScan
	allowed := map[ast.Node]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && is(c.Fun, noredirectPath, "Client") {
			out.wrapped++
			for _, a := range c.Args {
				if u, ok := a.(*ast.UnaryExpr); ok && u.Op == token.AND {
					a = u.X
				}
				allowed[a] = true
			}
		}
		return true
	})
	bad := func(n ast.Node, msg string) {
		out.violations = append(out.violations, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), msg))
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if is(x.Type, "net/http", "Client") && !allowed[x] {
				bad(x, "an http.Client that does not refuse redirects: build it with noredirect.Client(&http.Client{…})")
			}
			if is(x.Type, websocketPath, "DialOptions") {
				has := false
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "HTTPClient" {
							has = true
						}
					}
				}
				if !has {
					bad(x, "websocket.DialOptions with no HTTPClient falls back to http.DefaultClient, which follows redirects")
				}
			}
		case *ast.SelectorExpr:
			if is(x, "net/http", "DefaultClient") {
				bad(x, "http.DefaultClient follows redirects")
			}
		case *ast.CallExpr:
			for _, fn := range []string{"Get", "Head", "Post", "PostForm"} {
				if is(x.Fun, "net/http", fn) {
					bad(x, "http."+fn+" uses http.DefaultClient, which follows redirects")
				}
			}
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && is(x.Args[0], "net/http", "Client") {
				bad(x, "new(http.Client) does not refuse redirects")
			}
		}
		return true
	})
	return out
}

func parseSrc(t *testing.T, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fset, f
}

// TestTheRedirectGuardSeesEachHazard is the guard's own control: each hazard
// shape is reported, and the wrapped shapes are not.
func TestTheRedirectGuardSeesEachHazard(t *testing.T) {
	const head = "package x\nimport (\n\"net/http\"\nws \"github.com/coder/websocket\"\nnr \"github.com/ZacxDev/muster/internal/noredirect\"\n)\n"
	for name, body := range map[string]string{
		"a bare literal":           "var c = &http.Client{}",
		"a value literal":          "var c = http.Client{Timeout: 1}",
		"a literal inside another": "var c = nr.Client(wrap(&http.Client{}))",
		"new":                      "var c = new(http.Client)",
		"DefaultClient":            "var c = http.DefaultClient",
		"http.Get":                 "var _, _ = http.Get(\"http://127.0.0.1/\")",
		"DialOptions, no client":   "var o = &ws.DialOptions{HTTPHeader: nil}",
	} {
		fset, f := parseSrc(t, head+body+"\n")
		if s := scanFile(fset, f); len(s.violations) != 1 {
			t.Errorf("%s: %d violations %q, want 1", name, len(s.violations), s.violations)
		}
	}
	for name, body := range map[string]string{
		"a wrapped pointer literal": "var c = nr.Client(&http.Client{})",
		"a wrapped value literal":   "var c = nr.Client(&http.Client{Timeout: 1})",
		"a wrapped nil":             "var c = nr.Client(nil)",
		"DialOptions with a client": "var o = &ws.DialOptions{HTTPClient: nr.Client(nil)}",
	} {
		fset, f := parseSrc(t, head+body+"\n")
		if s := scanFile(fset, f); len(s.violations) != 0 || s.wrapped != 1 {
			t.Errorf("%s: violations %q, wrapped %d, want none and 1", name, s.violations, s.wrapped)
		}
	}
}

// TestEveryMirrorHTTPClientRefusesRedirects reads every muster package linked
// into this binary (go list -deps: build-constrained, non-test files).
func TestEveryMirrorHTTPClientRefusesRedirects(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}\t{{.Dir}}\t{{join .GoFiles \",\"}}", modulePath+"/cmd/muster-mirror").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	root := ""
	fset := token.NewFileSet()
	var violations []string
	got := map[string]int{}
	pkgs := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || !(parts[0] == modulePath || strings.HasPrefix(parts[0], modulePath+"/")) {
			continue
		}
		if parts[0] == modulePath+"/cmd/muster-mirror" {
			root = filepath.Dir(filepath.Dir(parts[1]))
		}
		if parts[0] == noredirectPath || parts[2] == "" {
			continue
		}
		pkgs++
		for _, name := range strings.Split(parts[2], ",") {
			path := filepath.Join(parts[1], name)
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			s := scanFile(fset, f)
			violations = append(violations, s.violations...)
			if s.wrapped > 0 {
				got[path] = s.wrapped
			}
		}
	}
	if root == "" || pkgs < 5 {
		t.Fatalf("go list found %d muster packages and no cmd/muster-mirror; the guard read nothing", pkgs)
	}
	rel := map[string]int{}
	for p, n := range got {
		r, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		rel[filepath.ToSlash(r)] = n
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
	if !reflect.DeepEqual(rel, wantNoRedirectSites) {
		t.Errorf("noredirect.Client sites = %v\nwant (the ledger) %v\nA new credential-bearing client belongs in the ledger; a removed one must leave it.", rel, wantNoRedirectSites)
	}
}
