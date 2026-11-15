package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryMirrorRouteHasATeamRule: every /mirror/, /ui/mirror/ and
// /send-queue route the mux registers (routes.golden) has a rule in
// mirrorTeamRoutes, and every rule names a registered route — so a new mirror
// route decides what a member may do with a team host's session through it.
func TestEveryMirrorRouteHasATeamRule(t *testing.T) {
	b, err := os.ReadFile("testdata/routes.golden")
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	var named []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		registered[l] = true
		_, path, _ := strings.Cut(l, " ")
		if strings.HasPrefix(path, "/mirror/") || strings.HasPrefix(path, "/ui/mirror/") || path == "/send-queue" || strings.HasPrefix(path, "/send-queue/") {
			named = append(named, l)
		}
	}
	if len(named) < 40 {
		t.Fatalf("instrument check FAILED: %d mirror routes found in routes.golden", len(named))
	}
	for _, r := range named {
		if _, ok := mirrorTeamRoutes[r]; !ok {
			t.Errorf("UNCLASSIFIED mirror route %s: add it to mirrorTeamRoutes (mirror_team_routes.go) with what a team member may do through it", r)
		}
	}
	for r := range mirrorTeamRoutes {
		if !registered[r] {
			t.Errorf("mirrorTeamRoutes lists %s, which is not a registered route", r)
		}
	}
}

// visibleHostsCallers is EVERY function in internal/api that calls
// Mirror.VisibleHosts — the listing that holds a member's TEAM hosts beside
// their own (team sharing 2b.3) — with why it may. Each acts on an entry it
// picks by the entry's Owner, never its host name alone. It fails when the set
// grows OR shrinks: every other caller reads Hosts, which stays own-only.
var visibleHostsCallers = map[string]string{
	"Server.mirrorEntry":     "a page or socket of one entry: only when mirrorTeamGate aimed the request at a team (mirror.TargetOwner), filtered to that owner's hosts",
	"Server.teamMirrorCards": "the sidebar's and the switcher's team groups: each card carries its Owner, so its links name owner=",
	"Server.teamHostsOf":     "a team episode's Attention row and tap (teamEpisodeShown), and a team preview's hand-off (after teamViewerPlugins' fresh viewer read): filtered to that one owner",
}

// mirrorTargetSetters is EVERY function in internal/api that calls
// mirror.ForOwner — aims a request's mirror calls at another owner's hub
// entries. One: the route gate, after the route's rule and the member's role.
var mirrorTargetSetters = map[string]string{
	"Server.mirrorTeamGate": "the mirror route gate: a mirrorTeamView route, a member at viewer and up",
}

// apiCallers is every non-test function in this package calling sel on
// something whose own selector is recv ("" = any receiver: a package-level
// call like mirror.ForOwner names the package as recv), by "Type.func".
func apiCallers(t *testing.T, recv, sel string) map[string]int {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				rt := fn.Recv.List[0].Type
				if st, ok := rt.(*ast.StarExpr); ok {
					rt = st.X
				}
				if id, ok := rt.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				s, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || s.Sel.Name != sel {
					return true
				}
				switch x := s.X.(type) {
				case *ast.SelectorExpr:
					if x.Sel.Name == recv {
						found[name]++
					}
				case *ast.Ident:
					if x.Name == recv {
						found[name]++
					}
				}
				return true
			})
		}
	}
	return found
}

// ledgerMatches fails t unless found's keys are exactly want's.
func ledgerMatches(t *testing.T, what string, found map[string]int, want map[string]string) {
	t.Helper()
	var extra, gone []string
	for k := range found {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	for k := range want {
		if found[k] == 0 {
			gone = append(gone, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(gone)
	if len(extra) > 0 {
		t.Errorf("%s: unclassified callers (decide whether each may): %v", what, extra)
	}
	if len(gone) > 0 {
		t.Errorf("%s: listed callers that no longer call it: %v", what, gone)
	}
}

// TestEveryVisibleHostsCallerIsClassified is visibleHostsCallers' walk. Its
// positive control: the same walk finds Mirror.Hosts' dozens of callers.
func TestEveryVisibleHostsCallerIsClassified(t *testing.T) {
	if hosts := apiCallers(t, "Mirror", "Hosts"); len(hosts) < 25 {
		t.Fatalf("instrument check FAILED: the walk found %d Mirror.Hosts callers", len(hosts))
	}
	ledgerMatches(t, "Mirror.VisibleHosts", apiCallers(t, "Mirror", "VisibleHosts"), visibleHostsCallers)
}

// TestOnlyTheMirrorTeamGateAimsAMirrorCall is mirrorTargetSetters' walk. Its
// positive control: the same walk finds mirror.ValidSession's callers.
func TestOnlyTheMirrorTeamGateAimsAMirrorCall(t *testing.T) {
	if n := apiCallers(t, "mirror", "ValidSession"); len(n) == 0 {
		t.Fatal("instrument check FAILED: the walk found no mirror.ValidSession caller")
	}
	ledgerMatches(t, "mirror.ForOwner", apiCallers(t, "mirror", "ForOwner"), mirrorTargetSetters)
}
