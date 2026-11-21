package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE SERVER APP'S DOOR AND ITS STRUCTURAL PINS (core PR 1h; no database).

// pgnServerTok is a server app's token whose row is given EVERY capability —
// a fixture past plugins_server_caps — so a door that read the row instead of
// serverPluginRoutes would serve it everything.
const pgnServerTok = "mpl_SRVSRVSRVSRVSRVSRVSRVSRVSRVSRVSRVSRVSRVSRVS"

// serverDoorPlugins is countingPlugins plus that server token, and a gate
// that knows no subject.
type serverDoorPlugins struct{ *countingPlugins }

func (s serverDoorPlugins) Authenticate(ctx context.Context, tok string) (plugins.Plugin, error) {
	if tok == pgnServerTok {
		s.note(ctx, "Authenticate")
		origin, entry := "https://*.plugin.example.test", "https://app.plugin.example.test/enter"
		label, icon := "Granite Hall", "video"
		return plugins.Plugin{ID: 77, Name: "example-server", Scope: plugins.ScopeServer, Origin: &origin, EntryURL: &entry,
			AppLabel: &label, AppIcon: &icon,
			Capabilities: []plugins.Capability{plugins.CapMessages, plugins.CapViewer, plugins.CapDraftChecks,
				plugins.CapChiefExtension, plugins.CapTasks, plugins.CapApp}}, nil
	}
	return s.countingPlugins.Authenticate(ctx, tok)
}

func (s serverDoorPlugins) LiveServerGrants(ctx context.Context, _ plugins.Plugin, _ []plugins.OriginPattern, subjects []string, _ time.Time) (map[string]plugins.ServerGrant, error) {
	s.note(ctx, "LiveServerGrants")
	out := map[string]plugins.ServerGrant{}
	for _, sub := range subjects {
		out[sub] = plugins.ServerGrant{State: plugins.GrantRevoked}
	}
	return out, nil
}

// TestServerPluginRoutesCoverEveryPluginRoute: serverPluginRoutes is exactly
// the token-taking /plugin/v1 routes of routes.golden (the two pairing routes
// take no token), failing when either side grows or shrinks; and its served
// set is exactly the six the design names.
func TestServerPluginRoutesCoverEveryPluginRoute(t *testing.T) {
	var golden, table, served []string
	for _, r := range goldenPluginRoutes(t) {
		if !strings.Contains(r, "/plugin/v1/pair") {
			golden = append(golden, r)
		}
	}
	for r, s := range serverPluginRoutes {
		table = append(table, r)
		if s.served {
			served = append(served, r)
		}
	}
	sort.Strings(table)
	sort.Strings(served)
	if strings.Join(golden, "\n") != strings.Join(table, "\n") {
		t.Errorf("serverPluginRoutes and routes.golden disagree:\n golden: %v\n  table: %v", golden, table)
	}
	want := []string{"DELETE /plugin/v1/notify/{ref}", "GET /plugin/v1/self", "POST /plugin/v1/handoffs/redeem",
		"POST /plugin/v1/notify", "POST /plugin/v1/owners/check", "POST /plugin/v1/tasks"}
	if strings.Join(served, "\n") != strings.Join(want, "\n") {
		t.Errorf("served to a server app: %v; want %v", served, want)
	}
	for r, s := range serverPluginRoutes {
		if s.actFor != (r == "POST /plugin/v1/notify" || r == "DELETE /plugin/v1/notify/{ref}" || r == "POST /plugin/v1/tasks") {
			t.Errorf("%s: actFor %v; want it only on notify, its withdrawal and tasks", r, s.actFor)
		}
	}
}

// TestTheDoorNotTheRowDecidesWhatAServerTokenIsServed: a server token whose
// row holds every capability is 403 with the server refusal's own body on
// every route the table refuses, and on each the store saw the token lookup
// alone and no handler ran. On an act-for route, a refused subject is the
// one 404 after the lookup and the gate only, and no handler ran.
// Red if: the door reads the row's capabilities instead of the table.
func TestTheDoorNotTheRowDecidesWhatAServerTokenIsServed(t *testing.T) {
	store := serverDoorPlugins{&countingPlugins{}}
	s, h, _ := newPluginsDoorServer(t, store)
	ran := 0
	s.principalProbe = func(*http.Request) { ran++ }
	s.serverNobodyProbe = func(*http.Request) { ran++ }
	refused := 0
	for _, route := range goldenPluginRoutes(t) {
		method, pattern, _ := strings.Cut(route, " ")
		if strings.Contains(pattern, "/plugin/v1/pair") || serverPluginRoutes[route].served {
			continue
		}
		store.reset()
		path := strings.NewReplacer("{id}", "5", "{ref}", "m-5").Replace(pattern)
		rec := pServe(h, method, path, `{}`, pBearer(pgnServerTok))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "this is a server app's token") {
			t.Errorf("%s with an every-capability server token: %d %s; want 403 %q", route, rec.Code, rec.Body.String(), ServerPluginRouteRefusal)
		}
		if got := store.snapshot(); len(got) != 1 || got[0] != "Authenticate as system" {
			t.Errorf("%s: store calls %v; want the token lookup alone", route, got)
		}
		refused++
	}
	if refused < 10 || ran != 0 {
		t.Errorf("refused %d routes, %d reached a handler; want every refused route (at least 10) and none", refused, ran)
	}
	store.reset()
	rec := pServe(h, "POST", "/plugin/v1/notify", knockBody("door", 1), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+pgnServerTok)
		r.Header.Set(MusterActForHeader, "c3ViamVjdC1mb3ItdGVzdA")
	})
	if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != srvNotFound || ran != 0 {
		t.Errorf("act-for an unknown subject: %d %s (handlers ran %d); want the one 404 and none", rec.Code, rec.Body.String(), ran)
	}
	// The gate is asked with NO request principal (it runs its own System
	// read, keyed on the app): "as " with no door.
	if got := store.snapshot(); len(got) != 2 || got[1] != "LiveServerGrants as " {
		t.Errorf("act-for refusal store calls %v; want the lookup and the gate only, the gate with no principal", got)
	}
	// A personal token carrying the header never acts.
	if rec := pServe(h, "POST", "/plugin/v1/messages", pgnMsg, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+pgnTok)
		r.Header.Set(MusterActForHeader, "c3ViamVjdC1mb3ItdGVzdA")
	}); rec.Code != http.StatusBadRequest || ran != 0 {
		t.Errorf("a personal token with the header: %d (handlers ran %d); want 400 and none", rec.Code, ran)
	}
}

// TestRuleWritesAndTokensElsewhereKnowTheServerDoor: authz.IsPluginVia is
// true for both plugin doors and nothing else, and an mpl_ token admitted at
// either door is not refused as "elsewhere" while every other door is.
func TestRuleWritesAndTokensElsewhereKnowTheServerDoor(t *testing.T) {
	s := New(nil, AuthConfig{}, nil)
	for _, via := range []authz.Via{authz.ViaPlugin, authz.ViaServerPlugin, authz.ViaSession, authz.ViaToken, authz.ViaDevice} {
		r, _ := http.NewRequest("POST", "/plugin/v1/notify", nil)
		r.Header.Set("Authorization", "Bearer "+pgnServerTok)
		w := httptest.NewRecorder()
		refusedElsewhere := s.refusePluginTokenElsewhere(w, r, via)
		plugin := via == authz.ViaPlugin || via == authz.ViaServerPlugin
		if authz.IsPluginVia(via) != plugin || refusedElsewhere == plugin {
			t.Errorf("%s: IsPluginVia %v, refused elsewhere %v; want %v and %v", via, authz.IsPluginVia(via), refusedElsewhere, plugin, !plugin)
		}
	}
}

// --- AST ledgers ------------------------------------------------------------------

// moduleFuncs walks every non-test Go file under internal/ and cmd/ and calls
// visit for each node inside each top-level declaration, with the
// declaration's "dir Name" (a method is "dir Recv.Name").
func moduleFuncs(t *testing.T, visit func(site string, n ast.Node)) {
	t.Helper()
	root := filepath.Join("..", "..")
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			for _, decl := range file.Decls {
				name := ""
				switch d := decl.(type) {
				case *ast.FuncDecl:
					name = d.Name.Name
					if d.Recv != nil && len(d.Recv.List) == 1 {
						rt := d.Recv.List[0].Type
						if st, ok := rt.(*ast.StarExpr); ok {
							rt = st.X
						}
						if id, ok := rt.(*ast.Ident); ok {
							name = id.Name + "." + name
						}
					}
				case *ast.GenDecl:
					for _, sp := range d.Specs {
						if vs, ok := sp.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
							name = vs.Names[0].Name
						}
					}
				}
				site := filepath.ToSlash(rel) + " " + name
				ast.Inspect(decl, func(n ast.Node) bool {
					if n != nil {
						visit(site, n)
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func setOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func assertSiteLedger(t *testing.T, what string, got map[string]bool, want []string) {
	t.Helper()
	g := setOf(got)
	sort.Strings(want)
	if len(g) < 3 {
		t.Fatalf("positive control: the walk found %d %s sites; it is not reading the module", len(g), what)
	}
	if strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Errorf("the %s sites changed (decide what a SERVER app does at each new one, then list it):\n got:\n  %s\nwant:\n  %s",
			what, strings.Join(g, "\n  "), strings.Join(want, "\n  "))
	}
}

// thePluginOwnerReadSites is EVERY read of a plugin row's owner outside its
// own package's scan (plugins.Plugin.PersonalOwner, and internal/api's
// messagesOwner, which wraps it for the personal-only paths), and EVERY site
// that names authz.ViaPlugin. The owner field is unexported, so each read
// had to decide what a server app (no owner) does there; this ledger makes a
// NEW site decide too. Fails when the set grows or shrinks.
var thePluginOwnerReadSites = []string{
	// PersonalOwner reads: each says what a server app (no owner) does.
	"internal/api Server.autostartStart#1",       // refused: no owner, no auto-start
	"internal/api Server.handlePluginSelf#1",     // the personal answer; a server app's is serverPluginSelf
	"internal/api Server.liveAppPlugins#1",       // a personal tab (a server app's is its own arm)
	"internal/api Server.requirePluginToken#1",   // the personal door; a server app took the server arm first
	"internal/api messagesOwner#1",               // 0 for a server app, on paths it is never served
	"internal/plugins Deliverer.Deliver#1",       // a message's plugin: messages-only
	"internal/plugins Deliverer.Deliver#2",       // the same message, its owner re-checked
	"internal/plugins PGStore.SetDeliverTo#1",    // refused for a server app
	"internal/plugins PGStore.routeEngagement#1", // messages-only: refused
	"internal/plugins PGStore.routeToOfficer#1",  // messages-only: refused
	"internal/plugins Plugin.PersonalOwner#1",    // the accessor itself
	"internal/plugins personallyOwnedBy#1",       // never true for a server app
	// messagesOwner calls, ONE ENTRY PER CALL: the messages, drafts,
	// task-writes and mail paths, none of which serverPluginRoutes serves a
	// server app.
	"internal/api Server.draftRefused#1",
	"internal/api Server.handlePluginDraftCheck#1",
	"internal/api Server.handlePluginDraftCheck#2",
	"internal/api Server.handlePluginDraftCheck#3",
	"internal/api Server.handlePluginDraftChecksToggle#1",
	"internal/api Server.handlePluginDraftChecksToggle#2",
	"internal/api Server.handlePluginDraftClaim#1",
	"internal/api Server.handlePluginDraftMedia#1",
	"internal/api Server.handlePluginDraftResult#1",
	"internal/api Server.handlePluginDraftResult#2",
	"internal/api Server.handlePluginTaskWriteResult#1",
	"internal/api Server.handlePluginTaskWritesClaim#1",
	"internal/api Server.notifyDraftRefused#1",
	"internal/api Server.notifyDraftRefused#2",
	"internal/api Server.notifyDraftRefused#3",
	"internal/api Server.retryQueuedPluginMessages#1",
	"internal/api Server.retryQueuedPluginMessages#2",
	"internal/api Server.retryQueuedPluginMessages#3",
	"internal/api Server.retryQueuedPluginMessages#4",
	"internal/api Server.retryQueuedPluginMessages#5",
	"internal/api Server.retryQueuedPluginMessages#6",
	"internal/api Server.runMailResume#1",
	"internal/api Server.startMailResume#1",
	// authz.ViaPlugin: the personal (and team) plugin door.
	"internal/api Server.requirePluginToken#via#1",
	"internal/api Server.teamPreviewPrincipal#via#1",
	"internal/api teamAgentDoors#via#1",
	"internal/plugins PGStore.CreatePairRequest#via#1",
	"internal/plugins PGStore.RedeemPair#via#1",
	// authz.IsPluginVia: BOTH plugin doors.
	"internal/api Server.refusePluginTokenElsewhere#isplugin#1",
	"internal/plugins ruleWriter#isplugin#1",
}

// TestThePluginOwnerReadSitesAreALedger walks the module's AST.
func TestThePluginOwnerReadSitesAreALedger(t *testing.T) {
	// 🔴 KEYED PER READ, not per declaration (audit round 1): each read is its
	// declaration, its kind and its ordinal within that declaration, so a
	// SECOND read added to a listed function is a new entry and fails this.
	got, seen := map[string]bool{}, map[string]int{}
	mark := func(key string) {
		seen[key]++
		got[key+"#"+strconv.Itoa(seen[key])] = true
	}
	moduleFuncs(t, func(site string, n ast.Node) {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "PersonalOwner" {
				mark(site)
			}
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "authz" && x.Sel.Name == "ViaPlugin" {
				mark(site + "#via")
			}
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "authz" && x.Sel.Name == "IsPluginVia" {
				mark(site + "#isplugin")
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "messagesOwner" {
				mark(site)
			}
		case *ast.FuncDecl:
			if x.Name.Name == "PersonalOwner" {
				mark(site)
			}
		}
	})
	assertSiteLedger(t, "plugin owner / ViaPlugin", got, append([]string(nil), thePluginOwnerReadSites...))
}

// TestTheServerGrantGateIsOneFunction: the callers of LiveServerGrants are
// exactly the act-for door and the owners/check handler; serverGrantLive and
// its two halves are referenced only inside LiveServerGrants (and the
// constants that compose them); and no other declaration spells the gate's
// clock line. So a second, drifting copy of the gate cannot appear. The
// behavioural table (TestTheGrantGatesCloseOneAtATimeOnBothConsumers) is the
// other half: a structural check alone would type-check past a wrong
// argument.
func TestTheServerGrantGateIsOneFunction(t *testing.T) {
	callers, refs, clock := map[string]bool{}, map[string]bool{}, map[string]bool{}
	moduleFuncs(t, func(site string, n ast.Node) {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "LiveServerGrants" && !strings.HasSuffix(site, "Store") {
				callers[site] = true
			}
		case *ast.Ident:
			switch x.Name {
			case "serverGrantLive", "serverGrantInTime", "serverGrantStanding":
				refs[site+" "+x.Name] = true
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING && strings.Contains(x.Value, "act_until >") {
				clock[site] = true
			}
		}
	})
	delete(callers, "internal/plugins PGStore.LiveServerGrants") // the definition's own name
	// The two DECIDERS (act-for and owners/check), and the two DISPLAYS that
	// ask the gate rather than re-spell it: an admin's live-grant count and a
	// person's "can act for you until" (core PR 1h.2).
	if got, want := setOf(callers), []string{"internal/api Server.admitServerPlugin", "internal/api Server.handlePluginOwnersCheck",
		"internal/plugins PGStore.AccountServerApps", "internal/plugins PGStore.liveGrantCount"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("callers of LiveServerGrants: %v; want %v", got, want)
	}
	wantRefs := []string{
		"internal/plugins PGStore.LiveServerGrants serverGrantLive",
		"internal/plugins PGStore.LiveServerGrants serverGrantStanding",
		"internal/plugins serverGrantInTime serverGrantInTime",
		"internal/plugins serverGrantLive serverGrantInTime",
		"internal/plugins serverGrantLive serverGrantLive",
		"internal/plugins serverGrantLive serverGrantStanding",
		"internal/plugins serverGrantStanding serverGrantStanding",
	}
	if got := setOf(refs); strings.Join(got, "\n") != strings.Join(wantRefs, "\n") {
		t.Errorf("references to the gate's constants:\n got %v\nwant %v", got, wantRefs)
	}
	if got := setOf(clock); strings.Join(got, ",") != "internal/plugins serverGrantInTime" {
		t.Errorf("declarations spelling the grant's clock (act_until >): %v; want only serverGrantInTime", got)
	}
}

// TestAServerAppReachesNoAutostart: auto-start is reached from one place,
// handlePluginTask, and only AFTER its server-app early return, so no route
// serverPluginRoutes serves can reach it (a server row also cannot hold
// `messages`, plugins_server_caps: internal/plugins
// TestAServerRowCannotHoldMessages).
func TestAServerAppReachesNoAutostart(t *testing.T) {
	callers := map[string]bool{}
	var serverReturn, autostart token.Pos
	moduleFuncs(t, func(site string, n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "autostartPluginTask":
				callers[site] = true
				if site == "internal/api Server.handlePluginTask" {
					autostart = call.Pos()
				}
			case "handleServerAppTask":
				if site == "internal/api Server.handlePluginTask" {
					serverReturn = call.Pos()
				}
			}
		}
	})
	if got := setOf(callers); strings.Join(got, ",") != "internal/api Server.handlePluginTask" {
		t.Errorf("callers of autostartPluginTask: %v; want handlePluginTask alone", got)
	}
	if serverReturn == token.NoPos || autostart == token.NoPos || serverReturn > autostart {
		t.Errorf("handlePluginTask: the server-app branch (%v) must come before the auto-start call (%v)", serverReturn, autostart)
	}
}

// TestAServerMintWithNoSignInTimeIsASignInAgain: the store's ErrSignInUnknown
// is the mint's 401, and nothing is minted.
func TestAServerMintWithNoSignInTimeIsASignInAgain(t *testing.T) {
	s, h, _ := newPluginsDoorServer(t, noSignInPlugins{&countingPlugins{}})
	req := httptest.NewRequest("POST", pluginHandoffsPath, strings.NewReader(`{"plugin":77,"target":{"app":{}}}`))
	req.Host = "muster.test"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", sessionCookieFor(s))
	req.Header.Set("Origin", "http://muster.test")
	r := httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != http.StatusUnauthorized || !strings.Contains(r.Body.String(), "records no sign-in time") {
		t.Errorf("a mint with no sign-in time: %d %s; want 401 sign in again", r.Code, r.Body.String())
	}
}

type noSignInPlugins struct{ *countingPlugins }

func (noSignInPlugins) MintHandoff(context.Context, int64, plugins.HandoffIn, time.Time) (string, plugins.Plugin, error) {
	return "", plugins.Plugin{}, plugins.ErrSignInUnknown
}

// TestAServerAppMayNotRouteToTheChiefWhateverItsRowSays: route_to_chief and a
// reference are messages powers; a server app is refused both even with a row
// that holds `messages` past its CHECK, and a personal messages token is not
// (the control).
func TestAServerAppMayNotRouteToTheChiefWhateverItsRowSays(t *testing.T) {
	all := []plugins.Capability{plugins.CapMessages, plugins.CapTasks, plugins.CapApp}
	server := plugins.Plugin{Scope: plugins.ScopeServer, Capabilities: all}
	personal := plugins.Plugin{Capabilities: all}.OwnedBy(5)
	for name, in := range map[string]pluginTaskIn{
		"route_to_chief": {RouteToChief: true},
		"reference":      {Reference: &pluginRefIn{}},
	} {
		if pluginTasksOnlyRefusal(server, in) == "" {
			t.Errorf("a server app with every capability may send %s", name)
		}
		if pluginTasksOnlyRefusal(personal, in) != "" {
			t.Errorf("CONTROL a personal messages token is refused %s", name)
		}
	}
}

// TestServerTaskLimitForgetsIdleKeys (audit round 1): a key whose writes all
// rolled out of the hour is pruned on the next reserve, as appNotifyLimit
// prunes, so the maps hold only who wrote in the last hour.
func TestServerTaskLimitForgetsIdleKeys(t *testing.T) {
	var l serverTaskLimit
	t0 := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for owner := int64(1); owner <= 50; owner++ {
		if _, ok := l.reserve(7, owner, t0); !ok {
			t.Fatal("a first write was refused")
		}
	}
	if len(l.person) != 50 || len(l.app) != 1 {
		t.Fatalf("CONTROL: %d person keys and %d app keys; want 50 and 1", len(l.person), len(l.app))
	}
	if _, ok := l.reserve(8, 99, t0.Add(serverTaskWindow)); !ok {
		t.Fatal("a write an hour later was refused")
	}
	if len(l.person) != 1 || len(l.app) != 1 {
		t.Errorf("after an hour: %d person keys and %d app keys; want only the new writer's 1 and 1", len(l.person), len(l.app))
	}
	l.refund(8, 99, t0.Add(serverTaskWindow))
	if len(l.person) != 0 || len(l.app) != 0 {
		t.Errorf("after the refund: %d person keys and %d app keys; want none", len(l.person), len(l.app))
	}
}
