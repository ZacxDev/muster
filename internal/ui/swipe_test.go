package ui

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// touchListenerBodies returns the source of every document-level touch
// listener appScript registers, keyed by the event name.
//
// ⚠ A TEXT EXTRACT: from the `addEventListener('touch…'` to the `{ passive:
// true });` that closes it. Every touch listener in appScript is registered
// that way; one that is not is MISSED here, which the positive controls below
// turn into a failure rather than a silent pass.
func touchListenerBodies(t *testing.T, js string) map[string]string {
	t.Helper()
	out := map[string]string{}
	rest := js
	for {
		i := strings.Index(rest, "document.addEventListener('touch")
		if i < 0 {
			return out
		}
		rest = rest[i:]
		nameEnd := strings.Index(rest[len("document.addEventListener('"):], "'")
		name := rest[len("document.addEventListener('") : len("document.addEventListener('")+nameEnd]
		end := strings.Index(rest, "{ passive: true });")
		if end < 0 {
			t.Fatalf("the %s listener has no `{ passive: true });` terminator — the extract cannot find its end", name)
		}
		out[name] += rest[:end]
		rest = rest[end:]
	}
}

// addListenerRE is every `.addEventListener(` call, whatever its receiver.
var addListenerRE = regexp.MustCompile(`\.addEventListener\(\s*`)

// onGestureRE is a gesture handler set as a property (`x.ontouchend = …`,
// `x.onpointerup = …`), which addEventListener's ledger would not see.
var onGestureRE = regexp.MustCompile(`\.on(touch|pointer)\w*\s*=[^=]`)

// gestureListeners is every touch* and pointer* listener in js (comments
// already stripped), keyed "<receiver>.<event>", each mapped to the source of
// its whole addEventListener(…) call. What it covers, exactly:
//
//   - every `.addEventListener(` call, on ANY receiver: an identifier receiver
//     is keyed by that identifier (document, list); any other receiver — a
//     call, an index, `x.querySelector(…)` — is keyed "(expr)";
//   - an event name written as a string literal in single or double quotes:
//     one starting "touch" or "pointer" is a ledger key;
//   - an event name that is the parameter of an enclosing
//     `[<literals>].forEach(function (<p>) {` (within 300 bytes before the
//     call) is each of those literals, and each is a key as above;
//   - any other event name that is NOT a literal (a variable, a call, a
//     concatenation) FAILS the test: a computed name could be any gesture;
//   - a gesture handler set as a property, `.on(touch|pointer)…=`, FAILS it.
//
// ⚠ NOT COVERED: a listener registered with no `.addEventListener(` text at
// all (a call through a stored reference to the function, `el['addEventListener']`,
// a template-literal event name), and a gesture library's own API.
func gestureListeners(t *testing.T, js string) map[string]string {
	t.Helper()
	if m := onGestureRE.FindString(js); m != "" {
		t.Errorf("a gesture handler is set as a property (%q): the ledger below cannot see one", m)
	}
	out := map[string]string{}
	for _, m := range addListenerRE.FindAllStringIndex(js, -1) {
		at := m[1]
		var names []string
		if at < len(js) && (js[at] == '\'' || js[at] == '"') {
			close := strings.IndexByte(js[at+1:], js[at])
			if close < 0 {
				t.Fatalf("an unterminated event name at %d", at)
			}
			names = []string{js[at+1 : at+1+close]}
		} else if ns, ok := forEachNames(js, m[0], at); ok {
			names = ns
		} else {
			ctx := js[m[0]:min(len(js), m[0]+60)]
			t.Errorf("an addEventListener with a computed event name (%q): it could register any gesture, and the ledger cannot name it", ctx)
			continue
		}
		recv := "(expr)"
		if i := m[0]; i > 0 && isIdentByte(js[i-1]) {
			j := i
			for j > 0 && isIdentByte(js[j-1]) {
				j--
			}
			recv = js[j:i]
		}
		body := js[m[0]:callEnd(t, js, m[0]+len(".addEventListener"))]
		for _, name := range names {
			if strings.HasPrefix(name, "touch") || strings.HasPrefix(name, "pointer") {
				out[recv+"."+name] += body
			}
		}
	}
	return out
}

// forEachRE is `[<string literals>].forEach(function (<param>) {`.
var forEachRE = regexp.MustCompile(`\[((?:\s*(?:'[^']*'|"[^"]*")\s*,?)+)\]\.forEach\(function\s*\((\w+)\)\s*\{`)

var literalRE = regexp.MustCompile(`'([^']*)'|"([^"]*)"`)

// forEachNames: the call at callAt names its event by an identifier at arg;
// when that identifier is the parameter of the nearest enclosing forEach over
// string literals (within 300 bytes before the call), those literals.
func forEachNames(js string, callAt, arg int) ([]string, bool) {
	id := arg
	for id < len(js) && isIdentByte(js[id]) {
		id++
	}
	if id == arg {
		return nil, false
	}
	param := js[arg:id]
	from := max(0, callAt-300)
	ms := forEachRE.FindAllStringSubmatch(js[from:callAt], -1)
	if len(ms) == 0 || ms[len(ms)-1][2] != param {
		return nil, false
	}
	var out []string
	for _, l := range literalRE.FindAllStringSubmatch(ms[len(ms)-1][1], -1) {
		out = append(out, l[1]+l[2])
	}
	return out, true
}

func isIdentByte(b byte) bool {
	return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// callEnd is the index just past the `)` that closes the call whose `(` is at
// open, string literals skipped (comments are stripped already).
func callEnd(t *testing.T, js string, open int) int {
	t.Helper()
	depth := 0
	for i := open; i < len(js); i++ {
		switch c := js[i]; c {
		case '\'', '"', '`':
			for i++; i < len(js) && js[i] != c; i++ {
				if js[i] == '\\' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	t.Fatalf("the addEventListener call at %d never closes", open)
	return len(js)
}

// TestNoTabSwitchSwipe pins the removal of the tab-switch swipe AND the survival
// of the sidebar's edge-swipe, in the one script both lived in.
//
// 🔴 NARROWED, NOT LIFTED: the switcher dock's own tabs DO change by a swipe
// (switcher_dock.go), and only there. So the ledger below lists EVERY touch
// and pointer listener appScript registers, on any receiver (gestureListeners),
// and fails when the set grows or
// shrinks: the sidebar's two on document, the dock's three on its list (the
// tabpanel inside the modal dialog, marked data-own-swipe), and the scroll
// restore's stop-on-operator-input on window (touchstart and pointerdown, from
// a forEach, components.go: it stops a restore, it is no swipe). None of them names
// the page's tab router. A swipe bound anywhere else — a tabpanel, the
// document, the recents strip — is a new key here, and red.
//
// 🔴 THE TWO HALVES ARE ONE TEST ON PURPOSE. Deleting the touch block wholesale
// would satisfy "no tab switch on swipe" while taking the sidebar gesture with
// it; keeping the block untouched would keep the sidebar and the bug. Each half
// alone is walkable by the other mistake.
//
// The behaviour is pinned in the browser too (recent-agents.spec.ts): a sideways
// swipe on the recents strip leaves the tab alone, and an edge-swipe still opens
// the sidebar.
func TestNoTabSwitchSwipe(t *testing.T) {
	forEachFeatureShape(t, func(t *testing.T, shape string, feat Features) {
		js := mustStripJSComments(t, jsSource(t, appScript(feat)))
		bodies := touchListenerBodies(t, js)

		// THE LEDGER: every touch and pointer listener in the script, by
		// receiver (gestureListeners says exactly what it can see).
		all := gestureListeners(t, js)
		var keys []string
		for k := range all {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if got, want := strings.Join(keys, " "), "document.touchend document.touchstart list.touchcancel list.touchend list.touchstart window.pointerdown window.touchstart"; got != want {
			t.Errorf("%s: appScript's touch listeners = %q\nwant %q (the sidebar's on document, the dock's on its list, the scroll restore's stop-on-input on window, nothing else)", shape, got, want)
		}
		for k, body := range all {
			if strings.HasPrefix(k, "list.") && strings.Contains(body, "document.") {
				t.Errorf("%s: the dock's %s listener reaches the document:\n%s", shape, k, body)
			}
			// The dock's bodies join the removal check below: none may reach
			// the page's tab router either.
			if strings.HasPrefix(k, "list.") {
				bodies["dock "+k] = body
			}
			// Every ledger body joins the removal check below.
			bodies["ledger "+k] = body
		}

		// POSITIVE CONTROLS: both listeners were found and carry real code.
		for _, ev := range []string{"touchstart", "touchend"} {
			if len(bodies[ev]) < 40 {
				t.Fatalf("%s: no %s listener extracted (%q) — nothing below is a claim about the gesture code",
					shape, ev, bodies[ev])
			}
		}

		// THE REMOVAL. No touch listener reaches the tab router, and nothing arms on
		// a tabpanel.
		for ev, body := range bodies {
			for _, gone := range []string{"switchTab", "TABS", "tabFromPath", "PANEL_SELECTOR", "tabpanel", "#panel-"} {
				if strings.Contains(body, gone) {
					t.Errorf("%s: the %s listener still names %q — a horizontal swipe can switch the tab "+
						"again (swiping the recent-agents strip switched the parent tab)", shape, ev, gone)
				}
			}
		}
		if strings.Contains(js, "PANEL_SELECTOR") {
			t.Errorf("%s: appScript still emits PANEL_SELECTOR, whose only consumer was the tab-switch swipe", shape)
		}

		// THE SURVIVOR. The sidebar's edge-swipe arms at the edge and opens/closes.
		if !strings.Contains(bodies["touchstart"], "ownsSideways(t)") || !strings.Contains(js, "data-own-swipe") {
			t.Errorf("%s: the edge-swipe no longer stands down for a touch that began on something that "+
				"swipes sideways itself (the recent-agents strip starts inside the edge zone)", shape)
		}
		if !strings.Contains(bodies["touchstart"], "SB_EDGE") {
			t.Errorf("%s: touchstart no longer arms the sidebar's open gesture at the left edge (SB_EDGE)", shape)
		}
		for _, want := range []string{"setOpen(true)", "setOpen(false)"} {
			if !strings.Contains(bodies["touchend"], want) {
				t.Errorf("%s: touchend no longer calls %s — the mobile sidebar's swipe is gone", shape, want)
			}
		}
	})
}
