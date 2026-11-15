package mirror

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attemptcode"
)

// fixtureCode is a code no constant in the package equals.
const fixtureCode = "QW3R-7YP4-K9ZX"

// TestTheHubKeepsOnlyTheCodesHash: after a held attempt presenting a code
// (read off a real request, as ServeConnect reads it), a confirm of it and a
// wrong guess, nothing the hub keeps — its whole hold and rate-limit state,
// printed — carries the code in any form; it carries the hash (the POSITIVE
// CONTROL: the dump can see what it keeps).
func TestTheHubKeepsOnlyTheCodesHash(t *testing.T) {
	h := NewHub(nil)
	key := hostKey{900337, "build-37"}
	h.hosts[key] = &hostConn{key: key}
	req := httptest.NewRequest(http.MethodGet, ConnectPath, nil)
	req.Header.Set(attemptcode.Header, fixtureCode)
	code := attemptCodeOf(req)
	if code == nil {
		t.Fatal("the request's code was not read")
	}
	if admit, _ := h.admitPinned(key, "127.0.0.1", code); admit {
		t.Fatal("the coded attempt was admitted while the host is live")
	}
	if _, _, err := h.ConfirmCode(key.owner, strings.ToLower(fixtureCode)); err != nil {
		t.Fatal(err)
	}
	_, _, _ = h.ConfirmCode(key.owner, "ZZZZ-ZZZZ-ZZZZ")
	dump := ""
	for k, hd := range h.held {
		dump += fmt.Sprintf("%#v %#v\n", k, *hd)
		for ak, a := range hd.attempts {
			dump += fmt.Sprintf("%q %#v %x\n", ak, *a, a.code)
		}
	}
	dump += fmt.Sprintf("%#v", h.confirmFails)
	sum, _ := attemptcode.Hash(fixtureCode)
	if !strings.Contains(dump, fmt.Sprintf("%x", sum)) {
		t.Fatalf("POSITIVE CONTROL: the hub's state does not show the code's hash:\n%s", dump)
	}
	norm, _ := attemptcode.Normalize(fixtureCode)
	for _, form := range []string{fixtureCode, norm, strings.ToLower(norm), strings.ToLower(fixtureCode)} {
		if strings.Contains(dump, form) {
			t.Fatalf("the hub keeps the code itself (%q):\n%s", form, dump)
		}
	}
}

// TestTheGuessLimitAgesOut: confirmFailLimit wrong codes refuse the team's
// confirms for confirmFailWindow, after which the right code arms again.
func TestTheGuessLimitAgesOut(t *testing.T) {
	h := NewHub(nil)
	clock := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return clock }
	key := hostKey{900341, "build-41"}
	h.hosts[key] = &hostConn{key: key}
	sum, _ := attemptcode.Hash(fixtureCode)
	h.admitPinned(key, "127.0.0.1", &sum)
	for i := 0; i < confirmFailLimit; i++ {
		clock = clock.Add(time.Second)
		if _, _, err := h.ConfirmCode(key.owner, "ZZZZ-ZZZZ-ZZZZ"); !errors.Is(err, ErrNoSuchCode) {
			t.Fatalf("wrong code %d = %v; want ErrNoSuchCode", i+1, err)
		}
	}
	if _, _, err := h.ConfirmCode(key.owner, fixtureCode); !errors.Is(err, ErrConfirmRateLimited) {
		t.Fatalf("the right code at the limit = %v; want ErrConfirmRateLimited", err)
	}
	clock = clock.Add(confirmFailWindow)
	h.admitPinned(key, "127.0.0.1", &sum) // the machine is still retrying
	if _, _, err := h.ConfirmCode(key.owner, fixtureCode); err != nil {
		t.Fatalf("the right code once the window passed = %v; want armed", err)
	}
	if admit, _ := h.admitPinned(key, "127.0.0.1", &sum); !admit {
		t.Fatal("the armed code's reconnect was not admitted")
	}
}

// TestTheCodeIsComparedInConstantTime (STRUCTURAL: a timing difference is not
// observable by a test's behaviour, so the source is read). Every function
// that compares a code — heldAttempt.admits, Hub.ConfirmCode, Hub.heldCode —
// calls subtle.ConstantTimeCompare and compares no code with == or !=, and
// ConfirmCode's loops never stop early (break or return), so how far a wrong
// code got says nothing about a right one. The CONTROL: the check finds the
// ConstantTimeCompare call in each (it is not reading an empty function).
func TestTheCodeIsComparedInConstantTime(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "teamhold.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"admits": false, "ConfirmCode": false, "heldCode": false}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		if _, ok := want[fn.Name.Name]; !ok {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
						want[fn.Name.Name] = true
					}
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "bytes" {
						t.Errorf("%s compares with bytes.%s", fn.Name.Name, sel.Sel.Name)
					}
				}
			case *ast.BinaryExpr:
				if (x.Op == token.EQL || x.Op == token.NEQ) && !isNil(x.X) && !isNil(x.Y) {
					for _, side := range []ast.Expr{x.X, x.Y} {
						if codeExpr(side) {
							t.Errorf("%s compares a code with %s at %s", fn.Name.Name, x.Op, fset.Position(x.Pos()))
						}
					}
				}
			case *ast.RangeStmt:
				if fn.Name.Name == "ConfirmCode" {
					ast.Inspect(x.Body, func(m ast.Node) bool {
						switch y := m.(type) {
						case *ast.ReturnStmt:
							t.Errorf("ConfirmCode returns inside its comparison loop at %s", fset.Position(y.Pos()))
						case *ast.BranchStmt:
							if y.Tok == token.BREAK {
								t.Errorf("ConfirmCode breaks out of its comparison loop at %s", fset.Position(y.Pos()))
							}
						}
						return true
					})
				}
			}
			return true
		})
	}
	for name, saw := range want {
		if !saw {
			t.Errorf("%s does not call subtle.ConstantTimeCompare", name)
		}
	}
}

// codeExpr is an expression that is a code hash: a.code, code, want, *code,
// code[:] and the like.
func codeExpr(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "code" || x.Name == "want"
	case *ast.SelectorExpr:
		return x.Sel.Name == "code"
	case *ast.StarExpr:
		return codeExpr(x.X)
	case *ast.SliceExpr:
		return codeExpr(x.X)
	case *ast.ParenExpr:
		return codeExpr(x.X)
	}
	return false
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}
