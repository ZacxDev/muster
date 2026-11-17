package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// TestTheFleetReadersSubjectSources: where the fleet's SESSIONS come from —
// the subject sources of the fleet reader (fleet_read.go) and of the digest's
// asks — as a ledger of every store and hub method each source function
// reaches through s.ext. It fails when a source is added (a team's hosts via
// VisibleHosts, a second session list) AND when one is dropped (the agents, or
// the owner's own hosts), so a change of WHO is in the owner's fleet is a
// decision made here, not a side effect. The behaviour is
// TestTheDigestIsTheOwnersOwnFleet (another owner's and a team's sessions
// absent).
//
// 🔴 Mirror.Hosts IS THE OWNER'S OWN HOSTS ONLY; VisibleHosts ADDS THE TEAMS'.
// A reference to VisibleHosts anywhere in the fleet's files fails too.
func TestTheFleetReadersSubjectSources(t *testing.T) {
	want := map[string][]string{
		// The sessions: the owner's agents (filtered to their own rows), the
		// windows of the owner's own hosts, and each window's state.
		"readFleet":          {"Agents.List", "Mirror.AgentState"},
		"mirrorFocusEntries": {"Mirror.Hosts"},
		// The digest's asks: the chief's own open cards.
		"chiefOpenCards": {"Attention.Open"},
		// What each session carries (not who is in the fleet, but pinned so
		// a new read is a decision too).
		"attachFleetEpisodes": {"Episodes.OpenEpisodes"},
		"attachFleetInFlight": {"SendQueue.List"},
		"attachFleetPRs":      {"FleetPRStates", "PRSessions"},
	}
	// readFleet's own helpers: the source functions it calls on s.
	wantCalls := []string{"attachFleetEpisodes", "attachFleetInFlight", "attachFleetPRs",
		"focusIndex", "instanceIndex", "mirrorFocusEntries", "now"}

	fset := token.NewFileSet()
	got := map[string][]string{}
	var calls []string
	visible := 0
	files := 0
	for _, path := range []string{"fleet_read.go", "fleet_digest.go", "focus.go", "focus_tools.go"} {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files++
		fleetFile := path == "fleet_read.go" || path == "fleet_digest.go"
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			_, tracked := want[name]
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "VisibleHosts" && fleetFile {
					visible++
				}
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if tracked {
					// s.ext.<Field>.<Method> and a bare s.ext.<Field>.
					if inner, ok := sel.X.(*ast.SelectorExpr); ok {
						if ext, ok := inner.X.(*ast.SelectorExpr); ok && ext.Sel.Name == "ext" {
							got[name] = append(got[name], inner.Sel.Name+"."+sel.Sel.Name)
							return false
						}
					}
					if sel.Sel.Name != "ext" {
						if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "ext" {
							got[name] = append(got[name], sel.Sel.Name)
						}
					}
				}
				if name == "readFleet" && path == "fleet_read.go" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "s" && sel.Sel.Name != "ext" && sel.Sel.Name != "logger" {
						calls = append(calls, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	if files != 4 {
		t.Fatalf("instrument: parsed %d files", files)
	}
	for name, w := range want {
		// A bare s.ext.<Field> (its nil check) is the same source as a method
		// called on it: only a field used with no method call stands alone.
		var g []string
		all := dedupSorted(got[name])
		for _, e := range all {
			if !strings.Contains(e, ".") && slices.ContainsFunc(all, func(o string) bool { return strings.HasPrefix(o, e+".") }) {
				continue
			}
			g = append(g, e)
		}
		if g == nil {
			g = []string{}
		}
		if !slices.Equal(g, w) {
			t.Errorf("%s reaches %v through s.ext; want exactly %v.\nA new source of the owner's sessions is a decision: "+
				"put it here with why, and keep a team's sessions out (Mirror.Hosts, never VisibleHosts).", name, g, w)
		}
	}
	if c := dedupSorted(calls); !slices.Equal(c, wantCalls) {
		t.Errorf("readFleet calls %v on s; want exactly %v", c, wantCalls)
	}
	if visible != 0 {
		t.Errorf("the fleet's files name VisibleHosts %d time(s): a team's hosts must never be in an owner's fleet", visible)
	}
}

func dedupSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	out = slices.Compact(out)
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	if out == nil {
		out = []string{}
	}
	return out
}
