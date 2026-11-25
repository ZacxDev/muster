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
//   - readEnabledPerson: readPerson whose SQL must also drop a DISABLED owner
//     — a credential honoured on every use with no sign-in behind it (mirror,
//     flow runner, plugin tokens), a per-owner loop that must skip a disabled
//     account (focus, chiefnudge), or a lookup by name that must not resolve
//     to one (plugin pairing) — through EnabledPersonIDs (#681: a disabled
//     person's mirror tokens kept connecting because Authenticate filtered
//     through PersonIDs alone). ⚠ apitokens and devices Authenticate stay
//     readPerson: they SELECT disabled_at and refuse a disabled owner in Go
//     with their own error (ErrUserDisabled, ownerRefusal), so a disabled
//     owner is not "no such token" there.
//   - readEnabledPersonOrTeam: readEnabledPerson WIDENED BY TEAMS, through
//     EnabledPersonOrTeamIDs — a credential a TEAM may own (a team host's
//     mirror token, team sharing 2b.2). Its person arm is EnabledPersonIDs
//     itself (TestTheTeamArmKeepsThePersonRule), so a disabled person stays
//     out.
//   - readTeam: it reads teams on purpose (teams.go).
//   - readByID: it reads one row by an id it was handed (a principal being
//     built, a row being locked, a disabled check) and returns no new user.
var usersReads = map[string]string{
	"internal/apitokens PGStore.Authenticate":              readPerson,
	"internal/apitokens PGStore.Create":                    readByID,
	"internal/chiefnudge PG.Owners":                        readEnabledPerson,
	"internal/chiefnudge PG.TeamHostOwners":                readTeam, // the session-state pass's team arm (2b.3): teams holding an unrevoked mirror token
	"internal/devices PGStore.Redeem":                      readByID,
	"internal/devices PGStore.Authenticate":                readPerson,
	"internal/signinreq PGStore.Redeem":                    readPerson, // the approver of an app sign-in: a person (PersonIDs), checked live
	"internal/flows PGStore.AuthenticateRunner":            readEnabledPerson,
	"internal/focus PGStore.Owners":                        readEnabledPerson,
	"internal/morning PG.Owners":                           readEnabledPerson,       // the morning report's owners: a disabled account gets no report
	"internal/mirror PGStore.Authenticate":                 readEnabledPersonOrTeam, // a team host's token (2b.2) or an enabled person's; Team says which
	"internal/mirror PGStore.TokenLive":                    readEnabledPersonOrTeam, // a live connection's recheck: Authenticate's rule
	"internal/mirror PGStore.CreateTeamToken":              readTeam,                // the CLI's --team mint: the owner must name a team
	"internal/users PGStore.TeamRotateReason":              readByID,                // the name of the person an audit row of the team names, for the rotate line
	"internal/officers/builtin Store.UsersExist":           readPerson,
	"internal/plugins PGStore.CreatePairRequest":           readEnabledPerson,
	"internal/plugins PGStore.Authenticate":                readEnabledPersonOrTeam, // a team viewer plugin (2b.3b) or an enabled person's; OwnerTeam says which
	"internal/push PGStore.ForOwner":                       readPerson,
	"internal/users emailTaken":                            readByID, // uniqueness over every row; a team has no email
	"internal/users PGStore.RedeemInvite":                  readByID, // a count by GoTrue subject; a team has none
	"internal/users PGStore.ListUsers":                     readPerson,
	"internal/users PGStore.HadStrongFactor":               readByID, // the enrolment gate's sticky tier (0188), for the signed-in person
	"internal/users guardLastAdmin":                        readByID, // admins only; a team is never an admin (CHECK)
	"internal/users PGStore.SetRole":                       readByID,
	"internal/users PGStore.ListTeams":                     readTeam,
	"internal/users PGStore.TeamNames":                     readTeam,   // the names of the caller's own teams (task board groups, Owner select)
	"internal/users PGStore.TeamMembers":                   readByID,   // the members of a team id the manager page was handed
	"internal/users PGStore.TeamPushTargets":               readPerson, // a team push's recipients: persons (kind = 'person'), and its own SQL drops a disabled one (u.disabled_at IS NULL): EnabledPersonIDs is the outside-package spelling
	"internal/users PGStore.MyTeams":                       readTeam,   // the caller's own teams (Account -> Teams; the disable sweep reads them BEFORE the disable, so no enabled filter)
	"internal/users lockTeam":                              readTeam,
	"internal/users PGStore.HoldTeamRole":                  readTeam, // a member's send fence: the team's row, held FOR KEY SHARE
	"internal/users PGStore.AddTeamMemberByEmailAsManager": readPerson,
	"internal/users lockPerson":                            readPerson,
	"internal/users PGStore.Role":                          readByID,
	"internal/users PGStore.FirstAdmin":                    readPerson, // the server pool's marks owner: the lowest-id enabled admin person (its own SQL drops a disabled one)
	"internal/users PGStore.Account":                       readByID,   // returns the kind; admit refuses a team
	"internal/users PGStore.BySub":                         readPerson,
	"internal/users PGStore.ByID":                          readPerson,
	"internal/users PGStore.IDByEmail":                     readPerson,
	"internal/users PGStore.SessionStateAmong":             readByID,
	"internal/users PGStore.CreateClaimInvite":             readPerson,
	"internal/users PGStore.ClaimInviteLive":               readByID,
	"internal/users PGStore.RedeemClaim":                   readByID,

	// Auto-start (0154): users joined by the plugin's OWN owner_id only to
	// refuse a team's plugin (u.kind <> 'team'); it returns no new user.
	"internal/plugins PGStore.SetAutostart": readByID,
	"internal/plugins PGStore.AutostartOf":  readByID,

	// Server-installed apps (0172, core PR 1h). Both yield a PERSON a server
	// app acts for, never a team: THE gate's serverGrantStanding (a const the
	// function names) holds u.id IN (users.EnabledPersonIDs), and the redeem's
	// re-check of the code's person does the same in its own SQL. A team row
	// (no redeem can make one) is "revoked" / refused.
	"internal/plugins PGStore.LiveServerGrants":    readEnabledPerson,
	"internal/plugins PGStore.RedeemServerHandoff": readEnabledPerson,
}

const (
	readPerson        = "persons only"
	readEnabledPerson = "enabled persons only"
	// readEnabledPersonOrTeam: see usersReads.
	readEnabledPersonOrTeam = "enabled persons, or teams"
	readTeam                = "teams on purpose"
	readByID                = "one row by an id it was handed"
)

var readsUsers = regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+(public\.)?users\b`)

func TestEveryUsersReadIsLedgered(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	type site struct {
		body string
		dir  string
		refs []string // identifiers the function names, for its package's consts
	}
	found := map[string]site{}
	// consts is each package's const declarations' source, by name: a read
	// whose SQL lives in a const (internal/plugins serverGrantStanding) has its
	// person filter there, not in the function's own text.
	consts := map[string]map[string]string{}
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
				if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.CONST {
					for _, sp := range gd.Specs {
						vs := sp.(*ast.ValueSpec)
						for _, n := range vs.Names {
							if consts[rel] == nil {
								consts[rel] = map[string]string{}
							}
							consts[rel][n.Name] = string(src[fset.Position(vs.Pos()).Offset:fset.Position(vs.End()).Offset])
						}
					}
				}
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				var refs []string
				hit := false
				ast.Inspect(fd, func(n ast.Node) bool {
					if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && readsUsers.MatchString(bl.Value) {
						hit = true
					}
					// A read whose only users-table SQL is the shared PersonIDs
					// subquery spells no "FROM users" of its own: count it too.
					if id, ok := n.(*ast.Ident); ok {
						refs = append(refs, id.Name)
					}
					if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "PersonIDs" || sel.Sel.Name == "EnabledPersonIDs" || sel.Sel.Name == "EnabledPersonOrTeamIDs") {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "users" {
							hit = true
						}
					}
					return true
				})
				if hit {
					found[filepath.ToSlash(rel)+" "+funcName(fd)] = site{body: string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset]),
						dir: rel, refs: refs}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Each function's text is read WITH the same-package consts it names,
	// and the consts those name (a const built from others), so a filter
	// spelled in a const counts as the function's own.
	for k, st := range found {
		seen := map[string]bool{}
		queue := append([]string(nil), st.refs...)
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			text, ok := consts[st.dir][n]
			if !ok || seen[n] {
				continue
			}
			seen[n] = true
			st.body += "\n" + text
			for _, w := range regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`).FindAllString(text, -1) {
				queue = append(queue, w)
			}
		}
		found[k] = st
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
		case readEnabledPerson:
			if !strings.Contains(s.body, "users.EnabledPersonIDs") {
				t.Errorf("%s is ledgered enabled-persons-only but does not filter through users.EnabledPersonIDs", k)
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

// TestTheTeamArmKeepsThePersonRule: EnabledPersonOrTeamIDs is the person rule
// (EnabledPersonIDs, disabled clause included) plus a team arm, and nothing
// else — so widening the mirror token's owner set to teams (2b.2) cannot
// re-admit a disabled person by spelling the person rule a second time.
func TestTheTeamArmKeepsThePersonRule(t *testing.T) {
	arm, ok := strings.CutPrefix(EnabledPersonOrTeamIDs, EnabledPersonIDs)
	if !ok {
		t.Fatalf("EnabledPersonOrTeamIDs = %q; want EnabledPersonIDs (%q) plus a team arm", EnabledPersonOrTeamIDs, EnabledPersonIDs)
	}
	if want := " UNION ALL SELECT id FROM public.users WHERE kind = 'team'"; arm != want {
		t.Fatalf("the team arm = %q; want %q", arm, want)
	}
	if !strings.Contains(EnabledPersonIDs, "disabled_at IS NULL") || !strings.Contains(EnabledPersonIDs, "kind = 'person'") {
		t.Fatalf("EnabledPersonIDs lost the person rule: %q", EnabledPersonIDs)
	}
}
