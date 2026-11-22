package users

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// usersWrite matches SQL that writes the users table (not users_* or
// team_members: `_` is a word character, so `users\b` does not match inside
// users_foo).
var usersWrite = regexp.MustCompile(`(?i)\b(insert\s+into|update)\s+(public\.)?users\b`)

var displayNameColumn = regexp.MustCompile(`\bdisplay_name\b`)

// displayNameWriterLedger is EVERY function in the module's non-test Go source
// holding a string literal that writes users.display_name (an INSERT INTO or
// UPDATE of users that names display_name), keyed by file and function.
//
// 🔴 A LEDGER, NOT A SPOT CHECK (#660). Each entry must call
// branding.CheckNameText, so a display name holding a control or a
// text-direction / invisible formatting character is refused when saved. A
// new writer fails this test until it is added here — and it can only be
// added once it calls the check. A stale entry fails it too. An entry whose
// reason starts "FIXTURE" is exempt from the call: it writes a fixed literal
// into a throwaway e2e database, never anyone's input.
//
// ⚠ WHAT IT CANNOT SEE: SQL assembled at run time (a column name split across
// literals, fmt.Sprintf, a file), and migrations (.sql — the schema, which
// seeds no person's name). It proves the call is in the writer, not that it
// runs before the write: TestEveryDisplayNameWriterRefusesControlAndFormatCharacters
// is the behavioural half, for the entries below.
var displayNameWriterLedger = map[string]string{
	"internal/users/invites.go:RedeemInvite": "a person's name, from the invite form",
	"internal/users/teams.go:CreateTeam":     "a team's name, from /admin/teams",
	// A fixture writes a fixed literal ("e2e member") into an *_e2e database
	// only (e2edb.Check); no one's input reaches it, so it is exempt.
	"e2e/mkmember/main.go:run": "FIXTURE: a fixed name, e2e only",
	// The officer-flow spec's team maker: a spec's fixed team name, into an
	// *_e2e database only. It calls the check as users.CreateTeam does.
	"e2e/mkteam/main.go:create":     "FIXTURE: a spec's fixed team name, e2e only",
	"e2e/mkteampage/main.go:create": "FIXTURE: team-pages.spec.ts's team and its page's creator, e2e only",
	"e2e/asksfiltfix/main.go:seed":  "FIXTURE: asks-filters.spec.ts's second owner, a fixed name, e2e only",
}

func TestEveryDisplayNameWriterIsChecked(t *testing.T) {
	root := displayNameModuleRoot(t)
	found := map[string]bool{}
	checked := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "node_modules" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			name := "<package>"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				name = fd.Name.Name
			}
			key := rel + ":" + name
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BasicLit:
					if n.Kind != token.STRING {
						return true
					}
					s, err := strconv.Unquote(n.Value)
					if err != nil {
						s = n.Value
					}
					if usersWrite.MatchString(s) && displayNameColumn.MatchString(s) {
						found[key] = true
					}
				case *ast.CallExpr:
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CheckNameText" {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "branding" {
							checked[key] = true
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got, want []string
	for k := range found {
		got = append(got, k)
	}
	for k := range displayNameWriterLedger {
		want = append(want, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("the users.display_name writers changed.\n got: %q\nwant: %q\n"+
			"A new writer must call branding.CheckNameText and be added to displayNameWriterLedger; a removed one must leave it.", got, want)
	}
	for _, k := range got {
		if !checked[k] && !strings.HasPrefix(displayNameWriterLedger[k], "FIXTURE") {
			t.Errorf("%s writes users.display_name without calling branding.CheckNameText", k)
		}
	}
}

func displayNameModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}
