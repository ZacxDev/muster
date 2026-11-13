package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// backgroundLoops is EVERY loop startBackgroundLoops starts, each through
// a.loop (api.Supervise), with where its per-owner work gets its owners. It
// fails when the set grows OR shrinks, so a new loop must be added here — and
// so must say where its owners come from — before it ships (#654 round 1:
// a per-owner sweep met a team and its bare goroutine killed the process).
//
//   - usersOwners: the loop enumerates owners from the users table, through
//     chiefnudge.PG.Owners or focus.PGStore.Owners (the focus REFRESHER and
//     the focus RETENTION sweep both act as each owner focus.Owners names),
//     which filter with users.PersonIDs (TestAPerOwnerSweepNeverActsAsATeam
//     runs two of them against a real team; TestOwnersNeverNamesATeam covers
//     focus.Owners; TestEveryUsersReadIsLedgered pins the queries).
//   - rowOwners: the loop acts as the owner of each row it handles; it reads
//     no owner list from users (TestEveryUsersReadIsLedgered: those two are
//     the only users-table owner lists in the module).
//   - noOwner: the loop acts as System or as no user.
//   - configOwner: the owner an officer's MUSTER_OFFICERS entry names. A team
//     id there is NOT refused at boot yet (team officers are phase 3): its
//     first pass panics in authz.WithPrincipal, and api.Supervise contains it.
//
// Whatever the class, a panic is contained by api.Supervise.
var backgroundLoops = map[string]string{
	"a.srv.RunChiefNudgeSweep":       usersOwners,
	"a.srv.RunSessionStateSweep":     usersOwners,
	"a.srv.RunFocusRefresh":          usersOwners,
	"a.kickoff.Run":                  rowOwners,
	"a.status.Run":                   rowOwners,
	"a.officers.Run":                 configOwner,
	"a.srv.RunOfficerAttentionSweep": rowOwners,
	"a.srv.RunOfficerScheduler":      rowOwners,
	"a.srv.RunAskSweep":              rowOwners,
	"a.srv.RunJobReaper":             rowOwners,
	"a.srv.RunTaskReap":              rowOwners,
	"a.srv.RunSendQueue":             rowOwners,
	"a.srv.RunRouteTimeouts":         rowOwners,
	"a.srv.RunAttentionSweep":        rowOwners,
	"a.srv.RunHostDispatchSweep":     rowOwners,
	"a.srv.RunClaimSweep":            rowOwners,
	"a.srv.RunAgentWatchdog":         rowOwners,
	"a.memo.Run":                     noOwner,
	"a.ccUsage.Run":                  noOwner,
	"a.officerPools.Run":             noOwner,
	"a.srv.RunSuggestSweep":          noOwner,
	"a.srv.RunTaskRetention":         noOwner,
	"a.srv.RunFocusRetention":        usersOwners,
	"a.srv.RunAskLogSweep":           noOwner,
}

const (
	usersOwners = "owners from the users table, persons only"
	rowOwners   = "owners of the rows it handles"
	noOwner     = "System or no user"
	configOwner = "the owner MUSTER_OFFICERS names"
)

// TestEveryBackgroundLoopIsSupervised reads main.go: startBackgroundLoops has
// NO bare `go` statement, and the names it hands a.loop are exactly
// backgroundLoops. The positive control is that the walk found the function
// and at least 20 loops in it.
func TestEveryBackgroundLoopIsSupervised(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "startBackgroundLoops" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("main.go has no startBackgroundLoops; the ledger is reading nothing")
	}
	var names []string
	ast.Inspect(fn, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GoStmt:
			t.Errorf("startBackgroundLoops has a bare `go` statement : start it with a.loop, which survives a panic")
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "loop" && len(x.Args) == 3 {
				if lit, ok := x.Args[1].(*ast.BasicLit); ok {
					name, _ := strconv.Unquote(lit.Value)
					names = append(names, name)
				}
			}
		}
		return true
	})
	sort.Strings(names)
	var want []string
	for n := range backgroundLoops {
		want = append(want, n)
	}
	sort.Strings(want)
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Errorf("startBackgroundLoops starts:\n  %s\nthe ledger lists:\n  %s\nA new loop is added to backgroundLoops with where its owners come from; a missing one stopped being supervised.",
			strings.Join(names, "\n  "), strings.Join(want, "\n  "))
	}
	if len(names) < 20 {
		t.Fatalf("found %d supervised loops; the walk is not reading startBackgroundLoops", len(names))
	}
}
