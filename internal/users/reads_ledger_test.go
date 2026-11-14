package users

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// usersReads is EVERY function in the module (non-test Go under internal/ and
// cmd/) whose SQL reads the users table (FROM/JOIN users), by package dir and
// function, with its class. It fails when the set grows OR shrinks (#654 round
// 1: one unfiltered owner list — chiefnudge.PG.Owners — made every per-owner
// sweep act as a team, and one unfiltered name lookup — pairing — matched a
// team's name). A new read must say here whether it can yield a team.
//
//   - readPerson: it yields a user to act as, notify, authenticate a credential
//     for or look up by name, so it must be persons only. Outside this
//     package it must use PersonIDs; inside, `kind = 'person'`.
//   - readTeam: it reads teams on purpose (teams.go).
//   - readByID: it reads one row by an id it was handed (a principal being
//     built, a row being locked, a disabled check) and returns no new user.
var usersReads = map[string]string{
	"internal/apitokens PGStore.Authenticate":              readPerson,
	"internal/apitokens PGStore.Create":                    readByID,
	"internal/chiefnudge PG.Owners":                        readPerson,
	"internal/devices PGStore.Redeem":                      readByID,
	"internal/devices PGStore.Authenticate":                readPerson,
	"internal/flows PGStore.AuthenticateRunner":            readPerson,
	"internal/focus PGStore.Owners":                        readPerson,
	"internal/mirror PGStore.Authenticate":                 readPerson,
	"internal/officers/builtin Store.UsersExist":           readPerson,
	"internal/plugins PGStore.CreatePairRequest":           readPerson,
	"internal/plugins PGStore.Authenticate":                readPerson,
	"internal/push PGStore.ForOwner":                       readPerson,
	"internal/users emailTaken":                            readByID, // uniqueness over every row; a team has no email
	"internal/users PGStore.RedeemInvite":                  readByID, // a count by GoTrue subject; a team has none
	"internal/users PGStore.ListUsers":                     readPerson,
	"internal/users guardLastAdmin":                        readByID, // admins only; a team is never an admin (CHECK)
	"internal/users PGStore.SetRole":                       readByID,
	"internal/users PGStore.ListTeams":                     readTeam,
	"internal/users PGStore.TeamMembers":                   readByID, // the members of a team id the manager page was handed
	"internal/users lockTeam":                              readTeam,
	"internal/users PGStore.AddTeamMemberByEmailAsManager": readPerson,
	"internal/users lockPerson":                            readPerson,
	"internal/users PGStore.Role":                          readByID,
	"internal/users PGStore.Account":                       readByID, // returns the kind; admit refuses a team
	"internal/users PGStore.BySub":                         readPerson,
	"internal/users PGStore.ByID":                          readPerson,
	"internal/users PGStore.IDByEmail":                     readPerson,
	"internal/users PGStore.DisabledAmong":                 readByID,
	"internal/users PGStore.CreateClaimInvite":             readPerson,
	"internal/users PGStore.ClaimInviteLive":               readByID,
	"internal/users PGStore.RedeemClaim":                   readByID,
}

const (
	readPerson = "persons only"
	readTeam   = "teams on purpose"
	readByID   = "one row by an id it was handed"
)

var readsUsers = regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+(public\.)?users\b`)

func TestEveryUsersReadIsLedgered(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	type site struct{ body string }
	found := map[string]site{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, src, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				hit := false
				ast.Inspect(fd, func(n ast.Node) bool {
					if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && readsUsers.MatchString(bl.Value) {
						hit = true
					}
					// A read whose only users-table SQL is the shared PersonIDs
					// subquery spells no "FROM users" of its own: count it too.
					if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "PersonIDs" {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "users" {
							hit = true
						}
					}
					return true
				})
				if hit {
					found[filepath.ToSlash(rel)+" "+funcName(fd)] = site{body: string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Positive control: the walk sees the reads it must see.
	if len(found) < 25 {
		t.Fatalf("found %d functions reading users; the walk is not reading the module", len(found))
	}
	var extra, missing []string
	for k, s := range found {
		class, ok := usersReads[k]
		if !ok {
			extra = append(extra, k)
			continue
		}
		inUsers := strings.HasPrefix(k, "internal/users ")
		switch class {
		case readPerson:
			if inUsers && !strings.Contains(s.body, "kind = 'person'") || !inUsers && !strings.Contains(s.body, "users.PersonIDs") {
				t.Errorf("%s is ledgered persons-only but does not filter through %s", k, map[bool]string{true: "kind = 'person'", false: "users.PersonIDs"}[inUsers])
			}
		case readTeam:
			if !strings.Contains(s.body, "kind = 'team'") {
				t.Errorf("%s is ledgered teams-only but does not filter kind = 'team'", k)
			}
		}
	}
	for k := range usersReads {
		if _, ok := found[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		t.Errorf("users-table reads not in usersReads (say whether each can yield a team):\n  %s", strings.Join(extra, "\n  "))
	}
	if len(missing) > 0 {
		t.Errorf("usersReads lists reads that no longer exist:\n  %s", strings.Join(missing, "\n  "))
	}
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}
