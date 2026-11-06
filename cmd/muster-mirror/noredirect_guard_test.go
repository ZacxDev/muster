package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// 🔴 EVERY HTTP CLIENT MUSTER'S OWN CODE BUILDS INTO THE MIRROR BINARY REFUSES
// REDIRECTS. Its clients carry credentials (the mirror token, the opencode
// secret, OTLP export headers), which net/http re-sends to a
// redirect Location; internal/noredirect is the one rule.
//
// This guard TYPE-CHECKS every muster package linked into muster-mirror (go
// list -deps: build-constrained, non-test files; imports resolved from the
// compiler's export data), so a renamed import or a type alias resolves to
// net/http.Client, and fails:
//
//   - [client-literal] a composite literal of type http.Client (or an alias
//     of it) that is not the direct argument of noredirect.Client;
//   - [client-new] new(http.Client), new(websocket.DialOptions);
//   - [zero-value] a var or struct field of type http.Client or
//     websocket.DialOptions (by value) declared with no value: its zero value
//     follows redirects / has no HTTPClient;
//   - [client-overwrite] an assignment whose target is an http.Client by value
//     (*c = …), which would replace a wrapped client wholesale;
//   - [checkredirect-assign] any assignment to an http.Client's CheckRedirect
//     field outside internal/noredirect, whatever the value — it would undo a
//     noredirect.Client wrap, even in a file the ledger lists;
//   - [default-client] any use of http.DefaultClient, or of http.Get, Head,
//     Post or PostForm (which use it), called or not;
//   - [dial-nil-opts] websocket.Dial with nil options (it falls back to
//     http.DefaultClient);
//   - [dialoptions-no-client] a websocket.DialOptions literal with no
//     HTTPClient, or with HTTPClient: nil (the same fallback).
//
// And it pins the LEDGER of noredirect.Client call sites, so the set of
// clients cannot grow or shrink without this file saying so.
//
// ⚠ WHAT IT DOES NOT SEE: clients built INSIDE third-party dependencies (only
// muster packages are read) — e.g. an exporter or SDK that builds its own
// http.Client when not handed one, or a library that returns a *http.Client
// it made; a client passed in through a variable whose origin is such a
// dependency; anything done through reflect or unsafe. A muster site that
// hands a dependency NO client, or nil (other than websocket's nil options or
// HTTPClient, which are checked), is not seen — e.g. dropping
// otlptracehttp.WithHTTPClient leaves the wrapped client built, the ledger
// intact and this guard green (internal/telemetry's redirect test catches it). Only the
// muster-mirror binary's dependency set is read, not muster-server's.

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
	"internal/mirrorhost/client.go":   1, // the tunnel's WebSocket connect (mirror token)
	"internal/mirrorhost/mcpproxy.go": 1, // the MCP proxy's forward (mirror token)
	"internal/opencodehost/client.go": 1, // the opencode server client (pair token / password)
	"internal/telemetry/telemetry.go": 1, // the OTLP trace exporter (OTEL_EXPORTER_OTLP_HEADERS)
}

// listedPkg is one package of `go list -deps -export -json`.
type listedPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
}

// world is the mirror binary's dependency set, and one importer over its
// export data (shared, so net/http.Client is one object for every check).
type world struct {
	fset *token.FileSet
	imp  types.Importer
	pkgs []listedPkg
	root string // the module root

	httpClient, dialOptions types.Type
	defaultClient           types.Object
	defaultFuncs            map[types.Object]string
	nrClient, wsDial        types.Object
}

var (
	worldOnce sync.Once
	theWorld  *world
	worldErr  error
)

func loadWorld(t *testing.T) *world {
	t.Helper()
	worldOnce.Do(func() { theWorld, worldErr = buildWorld() })
	if worldErr != nil {
		t.Fatal(worldErr)
	}
	return theWorld
}

func buildWorld() (*world, error) {
	out, err := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Dir,GoFiles,Export", modulePath+"/cmd/muster-mirror").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("go list: %v: %s", err, ee.Stderr)
		}
		return nil, fmt.Errorf("go list: %v", err)
	}
	w := &world{fset: token.NewFileSet(), defaultFuncs: map[types.Object]string{}}
	exports := map[string]string{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p listedPkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		exports[p.ImportPath] = p.Export
		w.pkgs = append(w.pkgs, p)
		if p.ImportPath == modulePath+"/cmd/muster-mirror" {
			w.root = filepath.Dir(filepath.Dir(p.Dir))
		}
	}
	w.imp = importer.ForCompiler(w.fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, fmt.Errorf("no export data for %q", path)
		}
		return os.Open(f)
	})
	lookup := func(pkg, name string) (types.Object, error) {
		p, err := w.imp.Import(pkg)
		if err != nil {
			return nil, err
		}
		o := p.Scope().Lookup(name)
		if o == nil {
			return nil, fmt.Errorf("%s.%s not found", pkg, name)
		}
		return o, nil
	}
	for _, x := range []struct {
		pkg, name string
		set       func(types.Object)
	}{
		{"net/http", "Client", func(o types.Object) { w.httpClient = o.Type() }},
		{websocketPath, "DialOptions", func(o types.Object) { w.dialOptions = o.Type() }},
		{"net/http", "DefaultClient", func(o types.Object) { w.defaultClient = o }},
		{noredirectPath, "Client", func(o types.Object) { w.nrClient = o }},
		{websocketPath, "Dial", func(o types.Object) { w.wsDial = o }},
	} {
		o, err := lookup(x.pkg, x.name)
		if err != nil {
			return nil, err
		}
		x.set(o)
	}
	for _, fn := range []string{"Get", "Head", "Post", "PostForm"} {
		o, err := lookup("net/http", fn)
		if err != nil {
			return nil, err
		}
		w.defaultFuncs[o] = "http." + fn
	}
	return w, nil
}

// check type-checks one package's files.
func (w *world) check(path string, files []*ast.File) (*types.Info, error) {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	_, err := (&types.Config{Importer: w.imp}).Check(path, w.fset, files, info)
	return info, err
}

// clientScan is what scanFile found in one file.
type clientScan struct {
	violations []string
	wrapped    int // noredirect.Client calls
}

// callee is the object a call's function expression names, if any.
func callee(info *types.Info, fun ast.Expr) types.Object {
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		return info.Uses[f]
	case *ast.SelectorExpr:
		return info.Uses[f.Sel]
	}
	return nil
}

func (w *world) scanFile(pkgPath string, f *ast.File, info *types.Info) clientScan {
	is := func(t, want types.Type) bool { return t != nil && types.Identical(t, want) } // Identical sees through aliases
	isByValue := func(t types.Type) bool { return is(t, w.httpClient) || is(t, w.dialOptions) }
	var out clientScan
	allowed := map[ast.Node]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && callee(info, c.Fun) == w.nrClient {
			out.wrapped++
			for _, a := range c.Args {
				a = ast.Unparen(a)
				if u, ok := a.(*ast.UnaryExpr); ok && u.Op == token.AND {
					a = ast.Unparen(u.X)
				}
				allowed[a] = true
			}
		}
		return true
	})
	bad := func(n ast.Node, code, msg string) {
		out.violations = append(out.violations, fmt.Sprintf("%s: [%s] %s", w.fset.Position(n.Pos()), code, msg))
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			t := info.TypeOf(x)
			if is(t, w.httpClient) && !allowed[x] {
				bad(x, "client-literal", "an http.Client that does not refuse redirects: build it with noredirect.Client(&http.Client{…})")
			}
			if is(t, w.dialOptions) {
				has, nilClient := false, false
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "HTTPClient" {
							has = true
							nilClient = info.Types[kv.Value].IsNil()
						}
					}
				}
				if !has || nilClient {
					bad(x, "dialoptions-no-client", "websocket.DialOptions with no (or a nil) HTTPClient falls back to http.DefaultClient, which follows redirects")
				}
			}
		case *ast.Ident:
			if o := info.Uses[x]; o != nil {
				if o == w.defaultClient {
					bad(x, "default-client", "http.DefaultClient follows redirects")
				} else if name, ok := w.defaultFuncs[o]; ok {
					bad(x, "default-client", name+" uses http.DefaultClient, which follows redirects")
				}
			}
			if o, ok := info.Defs[x].(*types.Var); ok && o.IsField() && isByValue(o.Type()) {
				bad(x, "zero-value", "a struct field holding an "+o.Type().String()+" by value: its zero value does not refuse redirects")
			}
		case *ast.ValueSpec:
			if len(x.Values) == 0 {
				for _, name := range x.Names {
					if o := info.Defs[name]; o != nil && isByValue(o.Type()) {
						bad(name, "zero-value", "a zero "+o.Type().String()+" does not refuse redirects")
					}
				}
			}
		case *ast.CallExpr:
			if b, ok := callee(info, x.Fun).(*types.Builtin); ok && b.Name() == "new" && len(x.Args) == 1 && isByValue(info.TypeOf(x.Args[0])) {
				bad(x, "client-new", "new("+info.TypeOf(x.Args[0]).String()+") does not refuse redirects")
			}
			if callee(info, x.Fun) == w.wsDial && len(x.Args) == 3 && info.Types[x.Args[2]].IsNil() {
				bad(x, "dial-nil-opts", "websocket.Dial with nil options falls back to http.DefaultClient, which follows redirects")
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if x.Tok != token.DEFINE && is(info.TypeOf(lhs), w.httpClient) {
					bad(lhs, "client-overwrite", "overwriting an http.Client by value replaces its CheckRedirect")
				}
				sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
				if !ok || pkgPath == noredirectPath {
					continue
				}
				if s := info.Selections[sel]; s != nil && s.Kind() == types.FieldVal && s.Obj().Name() == "CheckRedirect" &&
					s.Obj().Pkg() != nil && s.Obj().Pkg().Path() == "net/http" {
					bad(lhs, "checkredirect-assign", "setting CheckRedirect outside internal/noredirect undoes noredirect.Client")
				}
			}
		}
		return true
	})
	return out
}

// TestTheRedirectGuardSeesEachHazard is the guard's own control: each hazard
// shape is reported by ITS check (the [code]), exactly once, and the wrapped
// shapes are not.
func TestTheRedirectGuardSeesEachHazard(t *testing.T) {
	w := loadWorld(t)
	const head = "package x\nimport (\n\"context\"\n\"net/http\"\nws \"github.com/coder/websocket\"\nnr \"github.com/ZacxDev/muster/internal/noredirect\"\n)\n" +
		"var _ context.Context\nvar _ *http.Request\nvar _ = ws.Dial\nvar _ = nr.Check\n"
	// src is a whole file when it starts with "package", else a body after head.
	scan := func(t *testing.T, src string) clientScan {
		t.Helper()
		if !strings.HasPrefix(src, "package ") {
			src = head + src
		}
		f, err := parser.ParseFile(w.fset, "x.go", src+"\n", 0)
		if err != nil {
			t.Fatal(err)
		}
		info, err := w.check("example.com/x", []*ast.File{f})
		if err != nil {
			t.Fatalf("type-check %q: %v", src, err)
		}
		return w.scanFile("example.com/x", f, info)
	}
	for name, c := range map[string]struct{ body, code string }{
		"a bare literal":           {"var c = &http.Client{}", "client-literal"},
		"a value literal":          {"var c = http.Client{Timeout: 1}", "client-literal"},
		"a literal inside another": {"func wrap(c *http.Client) *http.Client { return c }\nvar c = nr.Client(wrap(&http.Client{}))", "client-literal"},
		"new":                      {"var c = new(http.Client)", "client-new"},
		"DefaultClient":            {"var c = http.DefaultClient", "default-client"},
		"http.Get":                 {"var _, _ = http.Get(\"http://127.0.0.1/\")", "default-client"},
		"http.Post as a value":     {"var f = http.Post", "default-client"},
		"DialOptions, no client":   {"var o = &ws.DialOptions{HTTPHeader: nil}", "dialoptions-no-client"},
		"a renamed http import":    {"package x\nimport h \"net/http\"\nvar c = &h.Client{}", "client-literal"},
		"W1 Dial with nil options": {"func f(ctx context.Context) { _, _, _ = ws.Dial(ctx, \"ws://127.0.0.1/\", nil) }", "dial-nil-opts"},
		"W2 a zero var":            {"var c http.Client", "zero-value"},
		"W2 a zero local":          {"func f() *http.Client { var c http.Client; return &c }", "zero-value"},
		"W2 a struct field":        {"type s struct{ c http.Client }", "zero-value"},
		"W3 a type alias":          {"type hc = http.Client\nvar c = &hc{}", "client-literal"},
		"W4 CheckRedirect = nil":   {"func f() { c := nr.Client(nil); c.CheckRedirect = nil }", "checkredirect-assign"},
		"W5 CheckRedirect = fn":    {"func f() { c := nr.Client(nil); c.CheckRedirect = func(*http.Request, []*http.Request) error { return nil } }", "checkredirect-assign"},
		"W5 via an alias":          {"type hc = http.Client\nfunc f(c *hc) { c.CheckRedirect = nil }", "checkredirect-assign"},
		"overwrite by value":       {"func f(c, d *http.Client) { *c = *d }", "client-overwrite"},
		"W6 HTTPClient: nil":       {"var o = &ws.DialOptions{HTTPClient: nil}", "dialoptions-no-client"},
		"a zero DialOptions":       {"var o ws.DialOptions", "zero-value"},
	} {
		s := scan(t, c.body)
		if len(s.violations) != 1 || !strings.Contains(s.violations[0], "["+c.code+"]") {
			t.Errorf("%s: violations %q, want one [%s]", name, s.violations, c.code)
		}
	}
	for name, body := range map[string]string{
		"a wrapped pointer literal":    "var c = nr.Client(&http.Client{})",
		"a wrapped value literal":      "var c = nr.Client(&http.Client{Timeout: 1})",
		"a wrapped alias literal":      "type hc = http.Client\nvar c = nr.Client(&hc{})",
		"a wrapped nil":                "var c = nr.Client(nil)",
		"DialOptions with a client":    "var o = &ws.DialOptions{HTTPClient: nr.Client(nil)}",
		"Dial with options":            "func f(ctx context.Context) { _, _, _ = ws.Dial(ctx, \"ws://127.0.0.1/\", &ws.DialOptions{HTTPClient: nr.Client(nil)}) }",
		"reading CheckRedirect":        "var c = nr.Client(nil)\nvar _ = c.CheckRedirect != nil",
		"a pointer var":                "var c *http.Client = nr.Client(nil)",
		"another type's CheckRedirect": "type s struct{ CheckRedirect func() }\nvar c = nr.Client(nil)\nfunc f(x *s) { x.CheckRedirect = nil }",
	} {
		s := scan(t, body)
		if len(s.violations) != 0 || s.wrapped != 1 {
			t.Errorf("%s: violations %q, wrapped %d, want none and 1", name, s.violations, s.wrapped)
		}
	}
}

// TestEveryMirrorHTTPClientRefusesRedirects type-checks and scans every muster
// package linked into this binary.
func TestEveryMirrorHTTPClientRefusesRedirects(t *testing.T) {
	w := loadWorld(t)
	var violations []string
	got := map[string]int{}
	pkgs := 0
	for _, p := range w.pkgs {
		if !(p.ImportPath == modulePath || strings.HasPrefix(p.ImportPath, modulePath+"/")) || p.ImportPath == noredirectPath || len(p.GoFiles) == 0 {
			continue
		}
		pkgs++
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(w.fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info, err := w.check(p.ImportPath, files)
		if err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for _, f := range files {
			s := w.scanFile(p.ImportPath, f, info)
			violations = append(violations, s.violations...)
			if s.wrapped > 0 {
				got[w.fset.Position(f.Package).Filename] = s.wrapped
			}
		}
	}
	if w.root == "" || pkgs < 5 {
		t.Fatalf("go list found %d muster packages and no cmd/muster-mirror; the guard read nothing", pkgs)
	}
	rel := map[string]int{}
	for p, n := range got {
		r, err := filepath.Rel(w.root, p)
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
