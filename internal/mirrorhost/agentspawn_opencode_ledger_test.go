package mirrorhost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyTheLauncherAndTheSpawnCreateOrRemoveOpenCodeSessions: a SEAM LEDGER
// over the whole module's non-test Go source. opencodehost.Client.CreateSession
// is called only by the launcher (`muster-mirror opencode`, twice: a fresh
// session and a -s that does not exist yet) and the host spawn
// (openCodeSpawn.createSession); Client.RemoveSession only by the host spawn's
// undo (openCodeSpawn.removeSession), for a session it created. The test fails
// when either set grows OR shrinks (docs/design/chief-dispatch.md 3.6).
//
// It counts calls in files that import opencodehost, by the enclosing function
// — so another package's own CreateSession (the agents store's) is not one.
func TestOnlyTheLauncherAndTheSpawnCreateOrRemoveOpenCodeSessions(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string][]string{}
	files := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		imports := false
		for _, im := range f.Imports {
			if v, _ := strconv.Unquote(im.Path.Value); v == "github.com/ZacxDev/muster/internal/opencodehost" {
				imports = true
			}
		}
		if !imports {
			return nil
		}
		files++
		rel, _ := filepath.Rel(root, p)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				rt := fd.Recv.List[0].Type
				if s, ok := rt.(*ast.StarExpr); ok {
					rt = s.X
				}
				if id, ok := rt.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "CreateSession" || sel.Sel.Name == "RemoveSession") {
					calls[sel.Sel.Name] = append(calls[sel.Sel.Name], rel+" "+name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Instrument check: the walk saw the importers it must (a walk that read
	// nothing would report an empty, "clean" ledger).
	if files < 5 {
		t.Fatalf("instrument check: only %d files import opencodehost", files)
	}
	for _, c := range []struct {
		method string
		want   []string
	}{
		{"CreateSession", []string{
			"cmd/muster-mirror/opencode_launch.go runOpenCodeLaunch",
			"cmd/muster-mirror/opencode_launch.go runOpenCodeLaunch",
			"internal/mirrorhost/agentspawn_opencode.go openCodeSpawn.createSession",
		}},
		{"RemoveSession", []string{"internal/mirrorhost/agentspawn_opencode.go openCodeSpawn.removeSession"}},
	} {
		got := calls[c.method]
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s is called from\n  %s\nwant exactly\n  %s", c.method, strings.Join(got, "\n  "), strings.Join(c.want, "\n  "))
		}
	}
}
