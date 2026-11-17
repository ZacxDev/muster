package plugins

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// THE ONE OWNER FILTER (team sharing 2b.3b, #696 round 0, D2): since a team
// viewer plugin exists and 0127 shares `plugins` at operator, a member's
// session READS (and a manager's WRITES) their teams' plugin rows. Every path
// a person's page or a manager's team page reaches filters through
// ownedByPrincipal, in the store — so a new one is filtered by default.

// TestEveryPersonalPluginPathKeepsThePrincipalsOwn: user 2 MANAGES team 9131
// (synthetic ids), which owns a viewer plugin; user 2 owns one too. Under
// user 2's browser session (memberships carried, so row-level security alone
// would show and let them change the team's row) List, Revoke, SetDraftChecks,
// SetDeliverTo and MintHandoff each see user 2's OWN plugin only. CONTROL:
// under the team acting principal the team's plugin is listed and revoked.
//
// 🔴 RED at b26416f4: the store's reads carried no owner filter (the API
// filtered at five open-coded sites), so user 2's session listed, locked and
// revoked the team's plugin here.
func TestEveryPersonalPluginPathKeepsThePrincipalsOwn(t *testing.T) {
	s, _, dsn := pgStore(t)
	const team = int64(9131)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role, kind, display_name) OVERRIDING SYSTEM VALUE VALUES (9131, 'member', 'team', 'filter-team')
		ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (9131, 2, 'manager', 1) ON CONFLICT DO NOTHING`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM plugin_handoffs WHERE owner_id = 9131`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM plugins WHERE owner_id = 9131`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM team_members WHERE team_id = 9131`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE user_id = 9131 OR team_id = 9131`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id = 9131`)
	})
	pats, err := ParseOrigins("https://*.viewer.example.net")
	if err != nil {
		t.Fatal(err)
	}
	viewer := func(ctx context.Context, name, entry string) Plugin {
		t.Helper()
		_, p, err := s.CreatePlugin(ctx, NewPlugin{Name: name, Label: name, Capabilities: []Capability{CapViewer},
			Origin: "https://*.viewer.example.net", EntryURL: entry})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	teamCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaPlugin})
	teamPlugin := viewer(teamCtx, "filter-team-plugin", "https://t.viewer.example.net/e")
	ms, err := authz.NewMemberships([]authz.Membership{{Team: team, Role: authz.TeamManager}})
	if err != nil {
		t.Fatal(err)
	}
	mgr := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession, Teams: ms})
	own := viewer(mgr, "filter-own-plugin", "https://o.viewer.example.net/e")

	list, err := s.List(mgr)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range list {
		names = append(names, p.Name)
	}
	if len(list) != 1 || list[0].ID != own.ID {
		t.Errorf("List as the manager's session: %v; want their own plugin alone", names)
	}
	if _, _, _, err := s.SetDraftChecks(mgr, teamPlugin.ID, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDraftChecks on the team's plugin from the manager's session: %v; want ErrNotFound", err)
	}
	if _, _, err := s.SetDeliverTo(mgr, teamPlugin.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDeliverTo on the team's plugin from the manager's session: %v; want ErrNotFound", err)
	}
	if _, _, err := s.MintHandoff(mgr, teamPlugin.ID, HandoffIn{Target: HandoffTarget{Mirror: &MirrorTarget{Host: "ws", Session: "main", Window: "@1"}},
		Purpose: PurposeView, Origins: pats}, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("MintHandoff of the team's plugin from the manager's session: %v; want ErrNotFound", err)
	}
	if found, err := s.Revoke(mgr, teamPlugin.ID); err != nil || found {
		t.Errorf("Revoke of the team's plugin from the manager's session: found %v, %v; want not found", found, err)
	}
	// CONTROL: the team acting principal reaches the team's own row.
	tl, err := s.List(teamCtx)
	if err != nil || len(tl) != 1 || tl[0].ID != teamPlugin.ID {
		t.Fatalf("CONTROL: List as the team: %+v, %v; want the team's plugin alone", tl, err)
	}
	if found, err := s.Revoke(teamCtx, teamPlugin.ID); err != nil || !found {
		t.Fatalf("CONTROL: Revoke as the team: %v, %v", found, err)
	}
}

// pluginRowReads classes every function declaration of THIS package
// (internal/plugins) whose body spells `FROM plugins`, `UPDATE plugins` or
// `INTO plugins`: "own" functions must carry ownedByPrincipal (the one
// filter); the others say why they read rows a principal does not own, or
// need no filter. It fails when that set grows or shrinks.
//
// 🔴 ITS REACH (#696 round 1): it sees no other package, no `JOIN plugins`,
// and no SQL held in a package-level constant. The reads it does not see
// (internal/api's drafts, routes, retry and unrouted joins; internal/notes'
// ref lookups; internal/users' rotate marks) are not personal plugin pages:
// TestEveryPersonalPluginPathKeepsThePrincipalsOwn pins those by behaviour.
var pluginRowReads = map[string]string{
	"PGStore.List":    "own",
	"PGStore.Revoke":  "own",
	"setCapabilityTx": "own", // SetDraftChecks' and SetChiefExtension's one capability writer
	// 0141: a plugin's chief instructions, stored by its own token and
	// approved by its owner's session: both lock the principal's own plugin.
	"PGStore.PutChiefInstructions":     "own",
	"PGStore.ApproveChiefInstructions": "own",
	"PGStore.SetDeliverTo":             "own",
	"PGStore.MintHandoff":              "own",
	"PGStore.Plugin":                   "by id: a message's own plugin, read where the message is (delivery under System, a session's route)",
	"PGStore.Authenticate":             "the token lookup, under System: how the owner is learned",
	"PGStore.createPluginTx":           "an insert: owner_id is the principal's (the column's default)",
	"PGStore.submit":                   "the plugin door: the token's own plugin id, locked",
	"PGStore.SetChiefSession":          "the plugin door's chief binding of the token's own plugin",
	"PGStore.pairMintable":             "a name's uniqueness among the pairing owner's own (owner_id = $1)",
	"PGStore.FileOutbound":             "an officer's draft names its owner's plugin (owner_id = $1)",
	// O-B round 0: the feature registry's boolean, at boot under System
	// (cmd/muster-server pluginsBoundAtBoot), deployment-wide by design: it
	// answers EXISTS and hands on no row.
	"PGStore.AnyBoundToOfficer": "deployment-wide EXISTS for the feature registry, under System at boot: no row is handed on",
}

func TestEveryPluginRowReadSaysWhoseRowsItReads(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]string{}
	for _, fn := range files {
		if strings.HasSuffix(fn, "_test.go") {
			continue
		}
		src, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, fn, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				rt := fd.Recv.List[0].Type
				if st, ok := rt.(*ast.StarExpr); ok {
					rt = st.X
				}
				if id, ok := rt.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			body := string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset])
			if !strings.Contains(body, "FROM plugins\n") && !strings.Contains(body, "FROM plugins ") && !strings.Contains(body, "UPDATE plugins") &&
				!strings.Contains(body, "INTO plugins") && !strings.Contains(body, "FROM plugins`") {
				continue
			}
			class := "other"
			if strings.Contains(body, "ownedByPrincipal") {
				class = "own"
			}
			found[name] = class
		}
	}
	if len(found) < 6 {
		t.Fatalf("instrument check FAILED: %d plugin row readers found", len(found))
	}
	var names []string
	for n := range found {
		names = append(names, n)
	}
	for n := range pluginRowReads {
		if _, ok := found[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		want, listed := pluginRowReads[n]
		got, ok := found[n]
		switch {
		case !listed:
			t.Errorf("%s reads plugins rows and is not in pluginRowReads: filter it through ownedByPrincipal, or say why not", n)
		case !ok:
			t.Errorf("pluginRowReads lists %s, which no longer reads plugins rows", n)
		case want == "own" && got != "own":
			t.Errorf("%s is classed own but does not filter through ownedByPrincipal", n)
		case want != "own" && got == "own":
			t.Errorf("%s filters through ownedByPrincipal; class it own", n)
		}
	}
}
