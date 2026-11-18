package api

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/push"
)

// TestEveryDeviceActionIsARouteAndARow is the design's server change 26, the
// native counterpart of the service worker's "no button POSTs to a 404" test:
//   - every action id payload v2 can name is in deviceActionRoutes;
//   - every id there names a route the mux registers (routes.golden), and every
//     registered /api/device/actions/* route is named by exactly one id;
//   - each id and its route are a row of the app's table in
//     docs/design/android.md ("Action ids"), exact strings.
//
// It fails when either set grows or shrinks.
func TestEveryDeviceActionIsARouteAndARow(t *testing.T) {
	golden := map[string]bool{}
	var registered []string
	for _, r := range readGolden(t) {
		golden[r] = true
		if strings.HasPrefix(r, "POST "+deviceActionsPrefix+"/") {
			registered = append(registered, r)
		}
	}
	if len(registered) == 0 {
		t.Fatal("positive control: the golden lists no signed action route — the scan sees nothing")
	}
	var ledger []string
	for id, route := range deviceActionRoutes {
		if !golden[route] {
			t.Errorf("%s names %q, which the mux does not register", id, route)
		}
		ledger = append(ledger, route)
	}
	sort.Strings(ledger)
	sort.Strings(registered)
	if strings.Join(ledger, "\n") != strings.Join(registered, "\n") {
		t.Errorf("registered signed routes:\n%s\nthe ledger names:\n%s", strings.Join(registered, "\n"), strings.Join(ledger, "\n"))
	}

	// What payload v2 can offer: every button of an ask with options and
	// suggestions.
	p := push.Payload{Type: "attention", Title: "x", AskActions: true, Ask: &push.V2Ask{ID: 7, Options: []string{"a"}, Suggestions: []string{"s"}}}
	seen := map[string]bool{}
	for _, a := range p.V2().Actions {
		seen[a.ID] = true
		if _, ok := deviceActionRoutes[a.ID]; !ok {
			t.Errorf("payload v2 offers %q, which no route serves", a.ID)
		}
	}
	if len(seen) != len(deviceActionRoutes) {
		t.Errorf("payload v2 offers %v; the ledger serves %d ids — an id served and never offered", seen, len(deviceActionRoutes))
	}

	doc, err := os.ReadFile("../../docs/design/android.md")
	if err != nil {
		t.Fatal(err)
	}
	for id, route := range deviceActionRoutes {
		path := strings.TrimPrefix(route, "POST ")
		row := regexp.MustCompile("(?m)^\\| `" + regexp.QuoteMeta(id) + "` \\| `" + regexp.QuoteMeta(path) + "` \\|$")
		if !row.Match(doc) {
			t.Errorf("docs/design/android.md's action-id table has no row `%s` → `%s`", id, path)
		}
	}
}
