package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// parseDir parses every non-test Go file in dir.
func parseDir(t *testing.T, dir string) (*token.FileSet, []*ast.File) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return fset, files
}

// userTokenMethods is every *gotrue.Client method that takes a USER access
// token (a parameter named accessToken), with that parameter's index.
func userTokenMethods(t *testing.T) map[string]int {
	t.Helper()
	_, files := parseDir(t, filepath.Join("..", "gotrue"))
	out := map[string]int{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "Client" {
				continue
			}
			i := 0
			for _, field := range fd.Type.Params.List {
				for _, n := range field.Names {
					if n.Name == "accessToken" {
						out[fd.Name.Name] = i
					}
					i++
				}
			}
		}
	}
	return out
}

// TestEveryUserTokenForwardGoesThroughTheHelper is the ledger of every place
// internal/api hands GoTrue a user access token. Each must be the access token
// asGoTrueUser passes to its call — so it is refreshed first when GoTrue would
// refuse it — or, listed by name below, a token GoTrue minted in that same
// request (a session muster is ending, never the cookie's).
//
// It fails when the set GROWS (a new direct forwarder, or a new user-token
// method on gotrue.Client nobody has routed) or SHRINKS (a site removed, or a
// walker that stopped finding them).
func TestEveryUserTokenForwardGoesThroughTheHelper(t *testing.T) {
	methods := userTokenMethods(t)
	names := make([]string, 0, len(methods))
	for m := range methods {
		names = append(names, m)
	}
	sort.Strings(names)
	if want := []string{"DeletePasskey", "Logout", "PasskeyRegisterOptions", "PasskeyRegisterVerify", "Passkeys"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("gotrue.Client's user-token methods = %v, want %v.\n"+
			"A new one is called through asGoTrueUser and added here; a removed one is removed here.", names, want)
	}

	fset, files := parseDir(t, ".")
	var got []string
	for _, f := range files {
		file := filepath.Base(fset.Position(f.Pos()).Filename)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var stack []ast.Node
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				idx, ok := methods[sel.Sel.Name]
				if !ok || idx >= len(call.Args) {
					return true
				}
				got = append(got, file+" "+fd.Name.Name+" "+sel.Sel.Name+" "+forwardKind(stack, call.Args[idx]))
				return true
			})
		}
	}
	sort.Strings(got)
	want := []string{
		"login_gotrue.go handleGoTrueLogout Logout helper",
		"login_gotrue.go resetSignalNode Passkeys helper",
		"login_invite.go signInAfterCutoff Logout minted:sess.AccessToken",
		"passkeys.go handlePasskeyAddOptions PasskeyRegisterOptions helper",
		"passkeys.go handlePasskeyAddVerify DeletePasskey helper",
		"passkeys.go handlePasskeyAddVerify PasskeyRegisterVerify helper",
		"passkeys.go handlePasskeyDelete DeletePasskey helper",
		"passkeys.go handlePasskeySignInVerify Logout minted:sess.AccessToken",
		"passkeys.go renderAccountPasskeys Passkeys helper",
		"signin_app.go handleAppSignInRedeem Logout minted:sess.AccessToken",
		"signin_app.go issueGoTrueSessionFor Logout minted:sess.AccessToken",
		"stepup.go handleStepUpPasskeyOptions Passkeys helper",
		"stepup.go verifySessionPasskey Logout minted:sess.AccessToken",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("user-token forwards to GoTrue:\n  got  %s\n  want %s\n"+
			"Forward the session's access token only from inside asGoTrueUser's call (it refreshes a token GoTrue "+
			"would refuse); a token GoTrue minted in the same request is listed here by name.",
			strings.Join(got, "\n       "), strings.Join(want, "\n       "))
	}
}

// forwardKind classifies the access-token argument of a user-token call:
// "helper" when it is the parameter of the func literal passed as
// asGoTrueUser's call, "minted:<expr>" for a Session's AccessToken, and
// "direct:<expr>" for anything else (never allowed).
func forwardKind(stack []ast.Node, arg ast.Expr) string {
	if id, ok := arg.(*ast.Ident); ok {
		for i := len(stack) - 1; i >= 1; i-- {
			lit, ok := stack[i].(*ast.FuncLit)
			if !ok {
				continue
			}
			ps := lit.Type.Params.List
			outer, ok := stack[i-1].(*ast.CallExpr)
			if len(ps) == 1 && len(ps[0].Names) == 1 && ps[0].Names[0].Name == id.Name && ok {
				if sel, ok := outer.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "asGoTrueUser" &&
					len(outer.Args) == 4 && outer.Args[3] == ast.Expr(lit) {
					return "helper"
				}
			}
			break // only the innermost func literal can bind it
		}
	}
	if sel, ok := arg.(*ast.SelectorExpr); ok && sel.Sel.Name == "AccessToken" {
		if x, ok := sel.X.(*ast.Ident); ok {
			return "minted:" + x.Name + ".AccessToken"
		}
	}
	return "direct:" + exprString(arg)
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	default:
		return "<expr>"
	}
}

// TestOnlyTheHelperReadsTheForwardedToken is the other half of the ledger: the
// session's access token (the `access` field of gotrueCreds / gotrueSession) is
// read only where it is set up, and handed to GoTrue only by asGoTrueUser.
// A new reader is a new place that could forward it unrefreshed.
func TestOnlyTheHelperReadsTheForwardedToken(t *testing.T) {
	fset, files := parseDir(t, ".")
	seen := map[string]bool{}
	for _, f := range files {
		file := filepath.Base(fset.Position(f.Pos()).Filename)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "access" {
					seen[file+" "+fd.Name.Name] = true
				}
				return true
			})
		}
	}
	var got []string
	for k := range seen {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{
		"accounts.go gotrueCredsFrom",
		"accounts.go requireGoTrueSession",
		"accounts.go resolveGoTrueSession",
		"gotrue_forward.go asGoTrueUser",
		"gotrue_forward.go refreshUserToken",
		"login_gotrue.go handleGoTrueLogout",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("functions touching the session access token:\n  got  %v\n  want %v\n"+
			"Forward it to GoTrue through asGoTrueUser, and list a new reader here only if it does not forward it.", got, want)
	}
}

// TestGoTrueExpiredRefusalShape pins what the retry keys on to the refusal the
// fake (and GoTrue) give, and that other refusals are not mistaken for it.
func TestGoTrueExpiredRefusalShape(t *testing.T) {
	expired := &gotrue.APIError{Status: 403, Code: "bad_jwt", Msg: gotruetest.ExpiredJWTMsg}
	if !gotrueRefusedAsExpired(expired) {
		t.Fatal("GoTrue's expired-token refusal is not recognised")
	}
	for _, other := range []error{
		&gotrue.APIError{Status: 401, Code: "bad_jwt", Msg: "invalid JWT"},
		&gotrue.APIError{Status: 403, Code: "session_not_found", Msg: "Session not found"},
		&gotrue.APIError{Status: 403, Code: "user_banned", Msg: "User is banned"},
		&gotrue.APIError{Status: 500, Code: "bad_jwt", Msg: "token is expired"},
		nil,
	} {
		if gotrueRefusedAsExpired(other) {
			t.Fatalf("%v was taken for an expired-token refusal", other)
		}
	}
}
