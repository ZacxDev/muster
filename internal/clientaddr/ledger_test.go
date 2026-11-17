package clientaddr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// identityHeaders are the request headers that name a client address.
var identityHeaders = map[string]bool{
	"cf-connecting-ip": true, "x-forwarded-for": true, "x-real-ip": true, "true-client-ip": true, "forwarded": true,
}

// sitesLedger is EVERY place in the module's non-test Go that spells a
// client-address header or reads a connection's peer address, by file. It is a
// relationship ledger: it fails when a site APPEARS (a new direct reading of a
// client's address, which must go through Of / Policy.Resolve instead) and
// when one DISAPPEARS (so the list stays the truth, not a superset).
//
// Every entry outside this package is NOT an identity read, and says why:
var sitesLedger = map[string][]string{
	// The one reading.
	"internal/clientaddr/clientaddr.go": {
		"RemoteAddr", // Resolve: the TCP peer
		// The Header* constants, the `headers` map's keys, and forwarding's
		// two extra names.
		"header:cf-connecting-ip", "header:cf-connecting-ip", "header:x-forwarded-for", "header:x-forwarded-for",
		"header:x-real-ip", "header:x-real-ip", "header:true-client-ip", "header:forwarded",
	},
	// The connector proxy STRIPS these before calling an upstream (hopHeaders).
	"internal/connectors/proxy.go": {"header:forwarded", "header:x-forwarded-for", "header:x-real-ip"},
	// The preview gateway STRIPS these before proxying to a host's app.
	"internal/previewgw/proxy.go": {"header:forwarded", "header:true-client-ip", "header:x-real-ip"},
	// The tunnel's stream type implements net.Conn's RemoteAddr method.
	"internal/preview/tunnel/tunnel.go": {"method:RemoteAddr"},
}

// TestEveryClientAddressReadIsInTheLedger walks the module's non-test Go with
// the parser (so comments, which mention these headers freely, are not sites)
// and compares what it finds with sitesLedger. A site is a string LITERAL that
// is a client-address header name (any case), a use of clientaddr's Header*
// constants outside this package, a .RemoteAddr field read or call, or a
// RemoteAddr method.
//
// ⚠ WHAT IT CANNOT SEE, stated: a header name assembled at run time
// ("X-Forwarded-" + "For", a value read from config) and a peer address taken
// from a net.Conn by another route (a type assertion, reflection). Those are
// not idioms anywhere in this module; this test pins the ones that are.
func TestEveryClientAddressReadIsInTheLedger(t *testing.T) {
	root := moduleRoot(t)
	got := scanSites(t, root)
	// The instrument can see a site at all: this package's own file must show
	// every one of its nine entries, of both kinds. A scanner wired to nothing
	// would pass the comparison below for every file it missed.
	if n := len(got["internal/clientaddr/clientaddr.go"]); n != 9 {
		t.Fatalf("CONTROL: the scanner found %d sites in clientaddr.go, want 9: %v", n, got["internal/clientaddr/clientaddr.go"])
	}
	files := map[string]bool{}
	for f := range got {
		files[f] = true
	}
	for f := range sitesLedger {
		files[f] = true
	}
	var names []string
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		want := append([]string(nil), sitesLedger[f]...)
		sort.Strings(want)
		have := got[f]
		if strings.Join(have, "|") != strings.Join(want, "|") {
			t.Errorf("%s: client-address sites %q, ledger says %q.\n"+
				"A client's address is read in ONE place: use clientaddr.Of(r) (muster-server, after its middleware) "+
				"or Policy.Resolve (another binary). A header that is only STRIPPED, or a site that went away, "+
				"is a ledger edit with its reason.", f, have, want)
		}
	}
}

// scanSites is every site in the module's non-test Go, by slash path.
func scanSites(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "testdata", "vendor", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		calls := map[*ast.SelectorExpr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if s, ok := c.Fun.(*ast.SelectorExpr); ok {
					calls[s] = true
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				if v, err := strconv.Unquote(x.Value); err == nil && identityHeaders[strings.ToLower(strings.TrimSpace(v))] {
					out[rel] = append(out[rel], "header:"+strings.ToLower(strings.TrimSpace(v)))
				}
			case *ast.SelectorExpr:
				// The exported header-name constants are the other way to name a
				// header without a literal: any use outside this package is a site.
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "clientaddr" && strings.HasPrefix(x.Sel.Name, "Header") &&
					!strings.HasPrefix(rel, "internal/clientaddr/") {
					out[rel] = append(out[rel], "const:clientaddr."+x.Sel.Name)
				}
				if x.Sel.Name == "RemoteAddr" {
					if calls[x] {
						out[rel] = append(out[rel], "call:RemoteAddr")
					} else {
						out[rel] = append(out[rel], "RemoteAddr")
					}
				}
			case *ast.FuncDecl:
				if x.Recv != nil && x.Name.Name == "RemoteAddr" {
					out[rel] = append(out[rel], "method:RemoteAddr")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for f := range out {
		sort.Strings(out[f])
	}
	return out
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = up
	}
}
