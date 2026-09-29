package api

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// AN INLINE SCRIPT'S URL IS A ROUTE CLAIM, AND NOTHING HAD EVER CHECKED ONE.
//
// 🔴 THE MEASUREMENT. The shell's resync fallback called
// `htmx.ajax('GET', <url>, { target: <selector> })` with a url this service does
// not register and a selector no document renders — both inherited from the
// service muster was extracted from. The url answered the mux's own plain-text
// 404 and the selector resolved to null.
//
// 🔴 WHY NEITHER HALF COULD BE SEEN FROM EITHER SIDE. internal/ui cannot know
// which routes exist — it renders markup, and the markup was well formed.
// internal/api cannot see an inline script — its tests drive routes, and this
// url is never requested by one. The defect lives in the seam, so the guard has
// to as well: this file renders the real document and reads the real route set.
//
// 🔴 AND IT WAS UNREACHABLE, WHICH IS WHY IT SURVIVED. The fallback runs only
// when `new CustomEvent(...)` throws, which no browser htmx 2 supports does. A
// branch nothing executes is a branch no amount of driving the app can find.
// ---------------------------------------------------------------------------

// htmxAjaxCall matches an `htmx.ajax('<METHOD>', '<url>', { target: '<sel>'…` in
// an inline script.
//
// ⚠ A TEXT MATCH, NOT A JS PARSE, and the positive control below is what makes
// that adequate: a call spelled in any other shape produces NO match, which is
// indistinguishable from a document that makes no such call — so the count is
// asserted before anything is concluded from it.
var htmxAjaxCall = regexp.MustCompile(`htmx\.ajax\(\s*'([A-Z]+)'\s*,\s*'([^']+)'\s*,\s*\{[^}]*target:\s*'#([^']+)'`)

// TestEveryHTMXAjaxCallInTheShellNamesARouteAndATargetThatExist is the seam
// guard.
//
// 🔴 IT CHECKS BOTH ENDS OF EACH CALL, AND EITHER ALONE WOULD HAVE PASSED THE
// SHIPPED DEFECT ONLY HALFWAY. A url that resolves into a target that does not
// fetches correctly and swaps nowhere; a target that exists behind a url that
// does not leaves the element untouched and raises a failure toast. The
// measured residue was wrong in both, which is the ordinary case when a call is
// copied out of another application.
func TestEveryHTMXAjaxCallInTheShellNamesARouteAndATargetThatExist(t *testing.T) {
	var buf bytes.Buffer
	if err := ui.RenderPage(&buf, "tasks", ui.Features{GitHub: true}); err != nil {
		t.Fatalf("rendering the shell: %v", err)
	}
	shell := buf.String()

	calls := htmxAjaxCall.FindAllStringSubmatch(shell, -1)
	// 🔴 POSITIVE CONTROL. Zero matches is what this test reports for a shell that
	// makes no such call AND for a regexp that has stopped matching the shape the
	// script is written in. The two are indistinguishable from the count alone, so
	// a zero fails here rather than passing silently.
	if len(calls) == 0 {
		t.Fatalf("no htmx.ajax(...) call was found in the shell. Either the shell no "+
			"longer makes one — in which case delete this guard — or the matcher has "+
			"stopped recognising the shape it is written in, in which case every "+
			"assertion below is vacuous.\n    matcher: %s", htmxAjaxCall.String())
	}

	registered := make(map[string]bool)
	rec := &muxRecorder{}
	RegisterRoutes(rec)
	for _, p := range rec.patterns {
		registered[p] = true
	}
	// 🔴 POSITIVE CONTROL ON THE ROUTE SET TOO. An empty recorder would make every
	// url below "unregistered", which fails loudly — but a recorder that captured
	// the wrong THING could still be non-empty, so a route this package certainly
	// serves is checked by name.
	if !registered["GET /ui/tasks"] {
		t.Fatalf("the recorder produced %d pattern(s) and none of them is the task-list "+
			"partial, so it is not recording what this test reads it as", len(rec.patterns))
	}

	for _, c := range calls {
		method, url, target := c[1], c[2], c[3]
		t.Run(method+" "+url, func(t *testing.T) {
			// Strip any query string: the route table keys on the path.
			path := url
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			if !registered[method+" "+path] {
				t.Errorf("the shell's inline script fetches %s %s, which this service does "+
					"not register. htmx answers that with whatever the mux does for an "+
					"unrouted path — here a plain-text 404 — and the failure listener turns "+
					"it into a toast on a page the operator did not ask anything of.",
					method, path)
			}
			if !strings.Contains(shell, `id="`+target+`"`) {
				t.Errorf("the shell's inline script swaps into #%s, which no element in the "+
					"shell carries. The fetch succeeds and the swap lands nowhere, which is "+
					"the silent half of the same defect.", target)
			}
		})
	}
}
