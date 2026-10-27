package ui

import (
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestNoCardCarriesTheRemovedSearchBoxsAttributes: the search box is gone
// (the session switcher's palette superseded it), and with it the two
// attributes only it read — the free-text `data-filter` haystack and the
// `data-rank-title` its rank read — on both card kinds, focus and all. The
// fields the host tabs read stay (the control). Red at origin/main: every card
// carried both.
func TestNoCardCarriesTheRemovedSearchBoxsAttributes(t *testing.T) {
	focus := &FocusView{Title: "Refactoring the widget pipeline", Description: "slow", UpdatedAt: time.Now()}
	cards := renderNode(t, AgentsCardsWithMirrors(
		[]AgentCardView{{ID: 7, Name: "keen-wren", DisplayName: "Fix The Chip", Status: "running", Focus: focus}},
		[]MirrorCardView{{Host: "Tower", Session: "Scratch2", Window: "@7", Label: "Gold:8", Online: true, Focus: focus}}))
	for _, gone := range []string{" data-filter=", " data-rank-title="} {
		if strings.Contains(cards, gone) {
			t.Errorf("a card still carries%s, which only the removed search box read", gone)
		}
	}
	if strings.Count(cards, ` data-f-host="`) != 2 {
		t.Errorf("control: both cards should still carry data-f-host:\n%s", cards)
	}
}

// TestTheFilterBarHasNoSearchBox: the Agents panel draws no search input and
// no clear button (operator ask: "Hide the existing search input because the
// new command palette UX supersedes it"); the count and the "Nothing matches"
// line the host tabs and chips still use are in the panel, OUTSIDE
// #agents-list (a refresh never touches them). Red at origin/main: the box was
// drawn.
func TestTheFilterBarHasNoSearchBox(t *testing.T) {
	panel := renderNode(t, AgentsPanel(true))
	for _, gone := range []string{`id="agents-search"`, `id="agents-search-clear"`, `role="searchbox"`, "Search by name"} {
		if strings.Contains(panel, gone) {
			t.Errorf("the Agents panel still draws %s", gone)
		}
	}
	list := strings.Index(panel, `id="agents-list"`)
	for _, id := range []string{"agents-hosts", "agents-chips", "agents-count", "agents-nomatch"} {
		i := strings.Index(panel, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("the Agents panel has no #%s", id)
		}
		if i > list {
			t.Errorf("#%s is drawn after #agents-list opens — inside or below what a refresh re-renders", id)
		}
	}
}

// TestTheFilterHidesWithStylesheetRulesAndCountsWithTheSameSelector pins the
// two decisions the script is written around, where a rewrite would lose them:
// the hiding is a stylesheet rule over the cards' attributes (so the list's own
// refresh cannot undo it), and the count is taken with the SAME selector (so
// what is counted is what is shown).
func TestTheFilterHidesWithStylesheetRulesAndCountsWithTheSameSelector(t *testing.T) {
	page := renderString(t, Page("agents", featuresAllOn))
	// The rules are the head script's (agents_filter_head.go), which the body
	// script uses: the two are read as one.
	script := scriptWith(t, page, "__musterAgentsRules =") + scriptWith(t, page, "__musterAgentsFilter")
	for name, want := range map[string]string{
		"the host rule hides a card of another host": `' > article:not(' + h + '){display:none !important}'`,
		"the chips' rule hides what they do not ask": `r.push('#agents-list > article:not(' + cs + '){display:none !important}')`,
		"the count uses the same selector":           `list.querySelectorAll(':scope > article:not([data-held])' + curSel)`,
		"the selector is the tab and the chips":      `curSel = (host ? sel({ attr: 'data-f-host', value: host, exact: true }) : '') + cs;`,
		"a held arrival is hidden and uncounted":     `'#agents-list > article[data-held]{display:none !important}'`,
		"it re-counts when the list settles":         `if (e.target && e.target.id === 'agents-list') settled();`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s: the filter script no longer contains %s", name, want)
		}
	}
	if strings.Count(page, "__musterAgentsFilter = true") != 1 {
		t.Error("the filter script is not rendered exactly once on the shell")
	}
	// It never marks a card itself: nothing for a morph to undo.
	if strings.Contains(script, ".hidden = true") || strings.Contains(script, "setAttribute('hidden'") {
		t.Error("the filter script hides cards one by one; a list refresh would un-hide them")
	}
}

var dataField = regexp.MustCompile(` data-f-([a-z][a-z0-9_]*)="([^"]*)"`)

// fieldsOf is every data-f-<field> attribute on a card's own <article> tag.
func fieldsOf(t *testing.T, card string) map[string]string {
	t.Helper()
	end := strings.Index(card, ">")
	if end < 0 {
		t.Fatalf("not a tag: %s", card)
	}
	out := map[string]string{}
	for _, m := range dataField.FindAllStringSubmatch(card[:end], -1) {
		if _, dup := out[m[1]]; dup {
			t.Errorf("data-f-%s is written twice: %s", m[1], card[:end])
		}
		out[m[1]] = m[2]
	}
	return out
}

func sameFields(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: data-f-%s = %q, want %q", what, k, got[k], w)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected data-f-%s = %q", what, k, v)
		}
	}
}

// TestEveryCardCarriesEachFieldInItsOwnAttribute: a card carries ONE
// attribute per field, so the host tab can ask about the host alone. Every
// fixture value is pairwise distinct (and none is the literal "cluster"), so a
// field filled from another field fails here by name.
//
// 🔴 A DISPATCHED POD AGENT'S HOST IS THE LITERAL "cluster": muster has no
// cluster name to put there, and naming it after a machine would merge "my
// tmux on that machine" with "pods on that cluster" — the very cut host:
// exists to make.
func TestEveryCardCarriesEachFieldInItsOwnAttribute(t *testing.T) {
	agent := renderNode(t, agentCard(AgentCardView{ID: 7, Name: "keen-wren", DisplayName: "#12 · Fix The Chip", Repo: "Acme/Widgets",
		Kind: agents.KindClaudeCode, CCAccount: "Alpha", Status: "running", Namespace: "Pods-Ns"}))
	sameFields(t, "a dispatched agent", fieldsOf(t, agent), map[string]string{
		"host": "cluster", "type": "agent", "name": "#12 · fix the chip keen-wren", "status": "running",
		"kind": "claude code", "repo": "acme/widgets", "account": "alpha", "ns": "pods-ns",
	})

	win := renderNode(t, mirrorCard(MirrorCardView{Host: "Tower", Session: "Scratch2", Window: "@7", Label: "Gold:8", Title: "Ignored:8",
		Kind: "opencode", Online: true, Interactive: true}))
	sameFields(t, "a mirrored window", fieldsOf(t, win), map[string]string{
		"host": "tower", "type": "mirrored", "name": "gold:8", "session": "scratch2", "status": "running",
		"kind": "opencode", "mode": "interactive",
	})

	// Offline, Claude Code, no label: its name is its title, it is stopped and
	// offline, and it is filed under Claude Code whatever its command is.
	cc := renderNode(t, mirrorCard(MirrorCardView{Host: "dock", Session: "solo", Window: "@2", Title: "solo:3", Kind: "node", Claude: true}))
	sameFields(t, "an offline Claude Code window", fieldsOf(t, cc), map[string]string{
		"host": "dock", "type": "mirrored", "name": "solo:3", "session": "solo", "status": "stopped",
		"kind": "claude code", "state": "offline",
	})
}

// TestEveryFieldIsARegisteredSafeIdentifier: the attribute NAME in a selector
// is built from the field name, so the set is closed (the registry) and every
// name is a plain lower-case identifier.
func TestEveryFieldIsARegisteredSafeIdentifier(t *testing.T) {
	ident := regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	seen := map[string]bool{}
	for _, f := range cardFieldRegistry {
		if !ident.MatchString(f.Key) {
			t.Errorf("field %q is not a safe CSS identifier", f.Key)
		}
		if seen[f.Key] {
			t.Errorf("field %q is registered twice", f.Key)
		}
		seen[f.Key] = true
	}
	for _, want := range []string{"host", "status", "kind", "repo", "name", "account", "session", "mode", "state", "type", "ns"} {
		if !seen[want] {
			t.Errorf("field %q is not registered", want)
		}
	}
	// Both card kinds carry the fields every card has, whatever optional ones
	// (repo, account, kind…) are empty. (An agent row always has a status.)
	for what, card := range map[string]string{
		"agent":  renderNode(t, agentCard(AgentCardView{ID: 1, Name: "a", Status: "pending"})),
		"mirror": renderNode(t, mirrorCard(MirrorCardView{Host: "h", Session: "s"})),
	} {
		got := fieldsOf(t, card)
		for _, k := range []string{"host", "type", "name", "status"} {
			if got[k] == "" {
				t.Errorf("a bare %s card has no data-f-%s: %v", what, k, got)
			}
		}
		for k := range got {
			if !seen[k] {
				t.Errorf("a %s card writes unregistered data-f-%s", what, k)
			}
		}
	}
}

// 🔴 ONE NAMESPACE, EACH NAME DECLARED ONCE, at the top level of
// agentsFilterScript's IIFE (its two-space-indented `function NAME(` and
// every name in each two-space-indented `var a = …, b = …;` list). Hoisting
// makes a repeat silent: a later `function` replaces an earlier one, two `var`s
// are one variable, and a `var` beside a `function` of the same name shares its
// binding — rebinding it when the `var` has an initializer and that line runs
// (a bare `var x;` leaves the function in place, and is still flagged: one
// name, two declarations). Each bit us: a second `function place` hid every card; a second
// `var pending` broke ?q= links. Names declared inside a nested function are
// local and not checked here.
func TestTheFilterScriptDeclaresEachNameOnce(t *testing.T) {
	decls := topLevelDeclarations(renderString(t, agentsFilterScript()))
	// Positive control: names the parse must find, including a SECOND name of
	// a comma list and both kinds.
	for _, want := range []string{"function apply", "var chosenHost", "var heldOrder", "var liveOrder", "function arrange"} {
		kind, name, _ := strings.Cut(want, " ")
		found := false
		for _, k := range decls[name] {
			found = found || k == kind
		}
		if !found {
			t.Fatalf("control: %q not found — the parse no longer reads the script (got %d names)", want, len(decls))
		}
	}
	for name, kinds := range decls {
		if len(kinds) > 1 {
			t.Errorf("%s is declared %d times at the top level of agentsFilterScript (%s); hoisting makes them one binding", name, len(kinds), strings.Join(kinds, ", "))
		}
	}
}

// topLevelDeclarations maps each name declared at the IIFE's top level of
// script to the kinds of its declarations ("function", "var"). A `var`
// statement is split on the commas at bracket depth 0 outside strings, so
// `var a = f(x, y), b = [1, 2];` declares a and b.
func topLevelDeclarations(script string) map[string][]string {
	out := map[string][]string{}
	for _, m := range regexp.MustCompile(`(?m)^  function (\w+)\s*\(`).FindAllStringSubmatch(script, -1) {
		out[m[1]] = append(out[m[1]], "function")
	}
	ident := regexp.MustCompile(`^\s*([A-Za-z_$][\w$]*)`)
	for _, loc := range regexp.MustCompile(`(?m)^  var `).FindAllStringIndex(script, -1) {
		rest := script[loc[1]:]
		depth, start := 0, 0
		var quote byte
		var parts []string
	scan:
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			switch {
			case quote != 0:
				if c == '\\' {
					i++
				} else if c == quote {
					quote = 0
				}
			case c == '\'' || c == '"' || c == '`':
				quote = c
			case c == '(' || c == '[' || c == '{':
				depth++
			case c == ')' || c == ']' || c == '}':
				depth--
			case c == ',' && depth == 0:
				parts = append(parts, rest[start:i])
				start = i + 1
			case c == ';' && depth == 0:
				parts = append(parts, rest[start:i])
				break scan
			}
		}
		for _, p := range parts {
			if m := ident.FindStringSubmatch(p); m != nil {
				out[m[1]] = append(out[m[1]], "var")
			}
		}
	}
	return out
}

// TestTheHostTabsAreOrderedByOneListStatedInGo: the operator's ask "the host
// tabs in the order all, <their first host>, laptop, then cluster". The order
// is ONE list (hostTabOrder) — the hosts the operator named
// (HostPreferences.Order), every other host, the cluster last — and the
// script is handed THAT list and sorts by a host's place in it before its
// label, rather than carrying a copy of its own. With nothing named, the
// cluster is still last. (Its behaviour in a browser, with hosts registered in
// another order, is host-names.spec.ts.) Red before: the script sorted the
// tabs by label alone, so "cluster" came first.
func TestTheHostTabsAreOrderedByOneListStatedInGo(t *testing.T) {
	if podHost != "cluster" {
		t.Fatalf("instrument check FAILED: podHost = %q", podHost)
	}
	sortRule := `var rx = hostRank(x.v), ry = hostRank(y.v);
      if (rx !== ry) return rx - ry;`
	for name, c := range map[string]struct {
		prefs HostPreferences
		order string
	}{
		"nothing named": {HostPreferences{}, `["*","cluster"]`},
		"two named":     {HostPreferences{Order: []string{"Tower", "laptop"}}, `["tower","laptop","*","cluster"]`},
	} {
		withHostPrefs(t, c.prefs)
		// The list travels on the script's tag (agentsHostOrderAttr); the
		// static code reads it from there. Both halves are asserted: the tag
		// carries THIS order, and the code takes HOST_ORDER from the tag.
		script := html.UnescapeString(renderNode(t, agentsFilterScript()))
		for _, want := range []string{agentsHostOrderAttr + `="` + c.order + `"`,
			`var HOST_ORDER = JSON.parse(TAG.getAttribute('` + agentsHostOrderAttr + `'));`, sortRule} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: the filter script lacks %q", name, want)
			}
		}
		if c.prefs.Order != nil && strings.Count(script, `"tower"`) != 1 {
			t.Errorf("%s: the script names tower %d times; the order must be stated once", name, strings.Count(script, `"tower"`))
		}
	}
}

// TestTheRulesTableIsRebuiltForAnotherBuildsVersion: the window-wide rules
// table is defined when absent OR built from other values (its version is a
// hash of the table's shape, chip selectors, ids and aliases), and the bound filter
// script reads the window's table at each call rather than keeping the one it
// started with. (The browser case is agents-list-ux.spec.ts.) Red before: the
// table was defined only when absent, and the script captured it.
func TestTheRulesTableIsRebuiltForAnotherBuildsVersion(t *testing.T) {
	v := agentsRulesVersion()
	if len(v) != 12 {
		t.Fatalf("version %q", v)
	}
	for _, want := range []string{
		`if (!window.__musterAgentsRules || window.__musterAgentsRules.v !== '` + v + `') (function () {`,
		`window.__musterAgentsRules = { v: '` + v + `', `,
	} {
		if !strings.Contains(agentsRulesJS, want) {
			t.Errorf("the rules script lacks %q", want)
		}
	}
	script := renderNode(t, agentsFilterScript())
	if strings.Contains(script, "var R = window.__musterAgentsRules;") ||
		!strings.Contains(script, "var r = window.__musterAgentsRules; return r[k].apply(r, arguments);") {
		t.Error("the filter script keeps the table it was bound with")
	}
	if !strings.Contains(script, "if (chips === null || chipsV !== v) {") {
		t.Error("the stored chips are not re-read when the table changes")
	}
	// The version moves with what the table is built from.
	was := chipAliases
	t.Cleanup(func() { chipAliases = was })
	chipAliases = map[string]string{"ask": "waiting"}
	if agentsRulesVersion() == v {
		t.Error("the version does not change with the aliases")
	}
}
