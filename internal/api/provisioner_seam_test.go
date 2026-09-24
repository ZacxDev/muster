package api

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/metrics"
)

// ---------------------------------------------------------------------------
// A NIL Provisioner MUST REFUSE, NOT ANSWER 200 AND DO NOTHING.
//
// 🔴 THE MEASUREMENT THAT PUT THIS FILE HERE. cmd/muster-server leaves
// Extensions.Provisioner nil on purpose (doc_seams.go entry 1) and its boot
// banner asserted, on every start, that "every agent-control route is
// REGISTERED … and each one refuses at request time". Driven against the image
// this tree builds, on the same boot whose banner printed that line, NINE routes
// answered 200 and did nothing:
//
//	POST /agents                      200 + a rendered card; the row sat `provisioning` for ever
//	POST /agents/{id}/start           200 + 3,590 bytes of card HTML
//	POST /agents/{id}/stop            200
//	DELETE /agents/{id}               200; htmx removed the card, the row was still there
//	GET  /agents/{name}/logs/stream   200 + `: connected`, then EOF — reads as "no logs"
//	GET  /agents/{name}/ws            the same shape, through chatTurn
//	POST /chief/provision             200, then SIGSEGV — its goroutine was BARE
//	POST /runbooks/{id}/dispatch      200
//	POST /api/agents/{name}/messages  200
//
// muster_panics_total{source="goroutine"} read 4. The panic is recovered by
// safeGo AFTER the handler has written its 200, so recoverMiddleware's 500 never
// reaches the wire and the operator is told the thing worked.
//
// 🔴 THIS IS THE IDENTICAL OBSERVABLE doc_seams.go ENTRY 1 REFUSES TO WIRE A
// FAKE FOR: "dispatch would report success, no pod would exist, and the agent
// would sit in `provisioning` for ever. That is the 'lies silently' side of the
// line." Wiring nil produced it exactly. The nil is only the honest option this
// entry claims once something refuses, which is api.requireProvisioner.
//
// The two halves below fail for different reasons and neither substitutes for
// the other: the first is a RELATIONSHIP over the source (a tenth call site
// cannot arrive unnoticed), the second is BEHAVIOUR through the real mux (a
// wrapper that is registered but broken cannot pass).
// ---------------------------------------------------------------------------

// provisionerRouteLedger is every route registered behind requireProvisioner.
//
// 🔴 THE TEST FAILS WHEN THIS SET GROWS *OR* SHRINKS, AND THAT SENTENCE WAS
// FALSE FOR A WHOLE REVISION — THIS COMMENT IS WHAT MADE IT UNCHECKABLE.
// Nothing derived the wrapped-route set from the source, so "shrinks" meant
// only "somebody edited this literal": a route could lose its wrapper and its
// ledger line together — two lines, the exact shape of a developer clearing a
// red subtest — and every test in the module stayed green. Measured: unwrap
// `POST /agents` at agents.go and delete its entry here, and the full suite
// passes with the shipped nil-provisioner deployment back to answering 200 over
// a row stuck in `provisioning` for ever. The only signal was the verdict count
// falling by two, and nothing read that.
//
// 🔴 SO THE SET IS DERIVED NOW, BY TestEveryProvisionerRouteIsDerivedNotDeclared
// BELOW, AND THIS LITERAL IS THE CLAIM BEING CHECKED — NOT THE SOURCE OF TRUTH.
// That guard asserts two different things, and the two-line mutant above is
// only caught by the SECOND:
//
//	DERIVED == DECLARED — the routes whose registration expression calls
//	  requireProvisioner are exactly the ones below. This catches a one-sided
//	  edit: an unwrap that leaves the ledger alone, or a delist that leaves the
//	  wrapper alone.
//	AND REACHABILITY DECIDES WHICH ROUTES BELONG — every registered route whose
//	  handler can reach s.ext.Provisioner unguarded through this package's call
//	  graph MUST be wrapped, and every wrapped one must be able to. The ledger
//	  plays no part in this half, which is why a matching two-line edit cannot
//	  satisfy it: `POST /agents` still resolves to handleAgentCreate, which
//	  still reaches Dispatch through createAndDispatchAgent.
//
// A plain "every wrapped route refuses" check would be satisfied by wrapping
// ZERO routes, which is the state this file was written to end.
var provisionerRouteLedger = []string{
	"DELETE /agents/{id}",
	"GET /agents/{name}/logs/stream",
	"GET /agents/{name}/ws",
	"POST /agents",
	"POST /agents/{id}/start",
	"POST /agents/{id}/stop",
	"POST /api/agents/{name}/messages",
	"POST /chief/provision",
	"POST /runbooks/{id}/dispatch",
}

// provisionerCallerLedger is every function in this package that calls a method
// on s.ext.Provisioner WITHOUT first checking it for nil in its own body.
//
// 🔴 IT IS THE SET THE WRAPPER EXISTS FOR, AND IT IS DERIVED FROM THE SOURCE
// RATHER THAN LISTED BY HAND. Each of these panics on a nil interface, so each
// must be reachable only from a route on provisionerRouteLedger. A new handler
// that calls the provisioner lands HERE first — which is the prompt to wrap its
// route, not a silent tenth lie.
//
// ⚠ A TYPE ASSERTION IS NOT A CALL AND IS DELIBERATELY ABSENT. `s.ext.Provisioner
// .(ProfileReapplier)` on a nil interface yields ok=false; it is the one form of
// optional dependency that is safe unguarded, and doc_seams.go entry 3 is about
// exactly that. handleAgentModel and the privilege grant path use it and are not
// on this list.
//
// 🔴 KNOWN LIMIT, FOUND BY A MUTANT THAT SURVIVED THIS GUARD RATHER THAN BY
// REVIEW: THE DETECTION IS FUNCTION-GRANULAR, NOT CALL-SITE-GRANULAR. A function
// counts as guarded if it compares s.ext.Provisioner against nil ANYWHERE in its
// body — so a function that nil-checks on one path and calls unguarded on
// another is classified guarded and never reaches this ledger. The sweep mutant
// that proved it bolted a call onto instanceIndex, which already had a nil
// check, and the guard stayed green. Closing it properly needs dominance
// analysis (does the check dominate the call?), which is a real piece of work
// and out of scope here.
//
// ⚠ WHAT THAT DOES AND DOES NOT COST. The case it cannot see is a MODIFIED
// already-guarded function; the case this ledger exists for — a NEW handler
// reaching the provisioner, which is how a tenth lying route would arrive — is
// caught, and the sweep confirms it with a mutant that adds exactly such a
// function. Read a clean verdict here as "no new unguarded caller", never as
// "every call site is dominated by a check".
var provisionerCallerLedger = []string{
	"(*Server).chatTurn",                  // ChatWithTools + Chat; reached from handleAgentWS
	"(*Server).createAndDispatchAgent",    // Dispatch; reached from handleAgentCreate, handleChiefProvision, dispatchRunbook
	"(*Server).handleAPIAgentSendMessage", // Chat
	"(*Server).handleAgentDelete",         // Destroy
	"(*Server).handleAgentLogsStream",     // StreamLogs
	"(*Server).handleAgentStart",          // Start
	"(*Server).handleAgentStop",           // Stop
	"(*Server).handleChiefProvision",      // Start
}

// unwiredProvisionerServer builds the deployment cmd/muster-server actually
// ships: every dependency EXCEPT Provisioner.
//
// 🔴 THE HOOK TOKEN HAS TO BE ARMED, OR ONE ROUTE WOULD PASS FOR THE WRONG
// REASON. POST /api/agents/{name}/messages sits behind requireArmedHookToken,
// which answers 503 of its OWN when the token is unset. A fixture that left it
// unset would see 503 on that route whether or not requireProvisioner was ever
// registered there — green from a DIFFERENT guard's refusal, and still green
// with the wrapper deleted. Arming the tier and presenting the credential makes
// the only remaining 503 the one under test.
//
// ⚠ testHookToken/testUIPassword are this package's existing fixture
// credentials (chief_threads_auth_test.go). Reused rather than re-minted: a
// second spelling of a shared secret is a second thing to keep in step.
//
// 🔴 IT RETURNS s.Handler(), NOT A BARE MUX, AND THAT IS WHAT MAKES THE
// MUTATION KILL ATTRIBUTABLE. Handler() is the production chain —
// recoverMiddleware included — so a handler that reaches a nil Provisioner
// answers 500 instead of taking the test binary down with it. Against a bare
// mux, deleting requireProvisioner would kill this test with a raw panic: red,
// but red for a reason that is indistinguishable from the fixture being wrong.
// With the real chain the failure is THIS test's own "answered 500, want 503".
func unwiredProvisionerServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{
		UIPassword: testUIPassword,
		HookToken:  testHookToken,
	}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          stubAgentsStore{},
		Privilege:       stubPrivilegeStore{},
		Runbooks:        stubRunbooksStore{},
		SessionLiveness: stubLiveness{},
	})
	return s, s.Handler()
}

// admit presents BOTH credentials this fixture holds, so a route is measured at
// its provisioner door regardless of which auth door it sits behind.
func admit(s *Server, r *http.Request) {
	r.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: auth.SignSession(s.auth.uiSessionSecret(), time.Now(), sessionTTL),
	})
	r.Header.Set("X-Muster-Token", testHookToken)
}

// TestEveryProvisionerRouteRefusesWhenItIsUnwired is the BEHAVIOURAL half.
//
// 🔴 IT DRIVES THE REAL MUX, NOT THE WRAPPER. A unit test of requireProvisioner
// would pass while every route was registered without it — which was the state
// that shipped. The only observable that separates "wrapped" from "not wrapped"
// is what the registered route answers, so that is what is read.
func TestEveryProvisionerRouteRefusesWhenItIsUnwired(t *testing.T) {
	s, h := unwiredProvisionerServer(t)

	for _, route := range provisionerRouteLedger {
		t.Run(route, func(t *testing.T) {
			method, pattern, ok := strings.Cut(route, " ")
			if !ok {
				t.Fatalf("malformed ledger entry %q", route)
			}
			// Substitute concrete values for the wildcards so the request
			// actually matches the registered pattern.
			path := strings.NewReplacer("{id}", "1", "{name}", "chief").Replace(pattern)

			req := httptest.NewRequest(method, path, nil)
			// Past whichever auth door this route sits behind, so what is
			// measured is the PROVISIONER refusal and not the auth one — the two
			// are both 503 on the machine tier, and conflating them would let a
			// route pass this test while never reaching requireProvisioner.
			admit(s, req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s answered %d, want 503.\n"+
					"    A nil Provisioner means this route cannot do its work. Answering "+
					"anything else is the measured failure: 200 with a rendered card over a "+
					"row that never changes, which an operator reads as success.\n"+
					"    body: %s", route, rec.Code, truncate(rec.Body.String(), 300))
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s: 503 body is not JSON (%v), so a client cannot branch on it.\n"+
					"    body: %s", route, err, truncate(rec.Body.String(), 300))
			}
			// 🔴 THE FIELD, NOT THE SENTENCE. A refusal a caller can only detect by
			// matching prose is one a reword silently breaks — and these routes
			// meet ordinary transient 503s too, which this one must be
			// distinguishable from because it can never resolve by retrying.
			if v, ok := body[ProvisionerUnwiredField].(bool); !ok || !v {
				t.Errorf("%s: 503 body carries no %s:true, so a client cannot tell this "+
					"permanent refusal from a transient outage and will retry for ever.\n"+
					"    body: %s", route, ProvisionerUnwiredField, truncate(rec.Body.String(), 300))
			}
		})
	}
}

// TestTheProvisionerRefusalIsBehindItsOwnAuthDoor pins the WRAPPER ORDER.
//
// 🔴 THE ORDER IS A DISCLOSURE BOUNDARY, NOT A STYLE CHOICE. requireProvisioner
// tells the caller which dependencies this build has wired. Registered OUTSIDE
// the auth wrapper it would answer that to anyone who can reach the port, which
// on this service is anyone on the LAN NodePort — turning nine routes into an
// unauthenticated probe of the server's configuration. So an ANONYMOUS request
// must meet the auth refusal, never the provisioner one.
func TestTheProvisionerRefusalIsBehindItsOwnAuthDoor(t *testing.T) {
	_, h := unwiredProvisionerServer(t)

	for _, route := range provisionerRouteLedger {
		t.Run(route, func(t *testing.T) {
			method, pattern, _ := strings.Cut(route, " ")
			path := strings.NewReplacer("{id}", "1", "{name}", "chief").Replace(pattern)
			rec := httptest.NewRecorder()
			// No session cookie, no hook token: an anonymous caller.
			h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))

			if strings.Contains(rec.Body.String(), ProvisionerUnwiredField) {
				t.Fatalf("%s told an ANONYMOUS caller which dependencies this build has "+
					"wired (%d, body contains %s). requireProvisioner must be registered "+
					"INSIDE the route's auth wrapper, not outside it.\n    body: %s",
					route, rec.Code, ProvisionerUnwiredField, truncate(rec.Body.String(), 300))
			}
			if rec.Code == http.StatusOK {
				t.Fatalf("%s answered 200 to an anonymous caller", route)
			}
		})
	}
}

// TestEveryUnguardedProvisionerCallerIsLedgered is the STRUCTURAL half.
//
// 🔴 IT IS WHAT MAKES A TENTH ROUTE IMPOSSIBLE TO ADD QUIETLY. The behavioural
// test above can only check routes somebody remembered to put in the ledger; a
// new handler calling s.ext.Provisioner.Foo() with no wrapper would be invisible
// to it and would answer 200-and-do-nothing exactly as the nine did. This walks
// the package's own AST instead, so the set is DERIVED and the ledger is the
// claim being checked against it.
func TestEveryUnguardedProvisionerCallerIsLedgered(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var guarded, unguarded []string
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			calls, nilChecked := inspectProvisionerUse(fn.Body)
			if !calls {
				return true
			}
			name := funcIdent(fn)
			if nilChecked {
				guarded = append(guarded, name)
			} else {
				unguarded = append(unguarded, name)
			}
			return true
		})
	}

	// 🔴 POSITIVE CONTROLS, BOTH DIRECTIONS, REPORTED AS A PAIR. "0 unledgered"
	// from a walker that parsed nothing is indistinguishable from a clean tree.
	if scanned < 10 {
		t.Fatalf("positive control FAILED: only %d non-test file(s) were parsed in this "+
			"package, which declares far more. The walk is not measuring.", scanned)
	}
	if len(guarded) == 0 {
		t.Fatalf("positive control FAILED: the walk found NO nil-guarded provisioner " +
			"caller, and instanceIndex/dismiss-destroy are both written that way. It " +
			"cannot tell guarded from unguarded, so every name below is unreliable.")
	}
	if len(unguarded) == 0 {
		t.Fatalf("positive control FAILED: the walk found no UNGUARDED provisioner caller " +
			"at all. Either every call site grew a nil check (in which case delete this " +
			"ledger deliberately) or the detection is matching nothing.")
	}

	sort.Strings(unguarded)
	want := append([]string(nil), provisionerCallerLedger...)
	sort.Strings(want)

	if strings.Join(unguarded, "\n") != strings.Join(want, "\n") {
		t.Errorf("the set of functions that call s.ext.Provisioner with NO nil check has "+
			"changed.\n  derived from source: %v\n  ledger:              %v\n"+
			"\n  GREW? A new function reaches the provisioner unguarded. Every route that "+
			"can reach it must be registered behind requireProvisioner and added to "+
			"provisionerRouteLedger — otherwise it answers 200 and does nothing on the "+
			"deployment cmd/muster-server actually ships, which is the failure this file "+
			"records.\n  SHRANK? A call site grew its own nil check or moved. Confirm the "+
			"route still refuses before removing it from the ledger.\n"+
			"\n  Guarded (informational, not checked): %v",
			unguarded, want, guarded)
	}
	t.Logf("%d file(s) parsed; %d unguarded provisioner caller(s), %d guarded",
		scanned, len(unguarded), len(guarded))
}

// inspectProvisionerUse reports whether body CALLS a method on s.ext.Provisioner,
// and whether body also compares s.ext.Provisioner against nil.
//
// ⚠ A TYPE ASSERTION IS NOT A CALL. `s.ext.Provisioner.(ProfileReapplier)` is an
// ast.TypeAssertExpr, is nil-safe, and must not put its function on the
// unguarded list — doing so would demand a wrapper for a branch that simply is
// not taken.
func inspectProvisionerUse(body *ast.BlockStmt) (calls, nilChecked bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			sel, ok := e.Fun.(*ast.SelectorExpr)
			if ok && isProvisionerExpr(sel.X) {
				calls = true
			}
		case *ast.BinaryExpr:
			if e.Op != token.EQL && e.Op != token.NEQ {
				return true
			}
			if isProvisionerExpr(e.X) && isNilIdent(e.Y) {
				nilChecked = true
			}
			if isProvisionerExpr(e.Y) && isNilIdent(e.X) {
				nilChecked = true
			}
		}
		return true
	})
	return calls, nilChecked
}

// isProvisionerExpr reports whether e is the expression `s.ext.Provisioner`.
func isProvisionerExpr(e ast.Expr) bool {
	outer, ok := e.(*ast.SelectorExpr)
	if !ok || outer.Sel.Name != "Provisioner" {
		return false
	}
	mid, ok := outer.X.(*ast.SelectorExpr)
	if !ok || mid.Sel.Name != "ext" {
		return false
	}
	recv, ok := mid.X.(*ast.Ident)
	return ok && recv.Name == "s"
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// funcIdent spells a FuncDecl the way the ledger does: `(*Server).method`.
func funcIdent(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fn.Name.Name
		}
	case *ast.Ident:
		return t.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// routeRegistration is one `mux.HandleFunc(pattern, handler)` site in this
// package's non-test sources.
type routeRegistration struct {
	route    string // the literal pattern, e.g. "POST /agents"
	handler  string // the innermost function the wrappers close over
	wrapped  bool   // the registration expression calls s.requireProvisioner
	computed bool   // the pattern is not a string literal
	file     string
	line     int
}

// TestEveryProvisionerRouteIsDerivedNotDeclared is the half that makes
// provisionerRouteLedger's "GROWS *or* SHRINKS" sentence true.
//
// 🔴 IT EXISTS BECAUSE THAT SENTENCE WAS MEASURED FALSE. The ledger was
// hand-written and nothing derived the wrapped set, so shrinking it was a
// two-line edit — unwrap the route, delete its line — and the whole module
// stayed green while the shipped deployment went back to answering 200 and
// doing nothing. The route ledger sat four lines from a caller ledger that IS
// derived from the source; this closes the asymmetry.
//
// 🔴 THE REACHABILITY HALF IS THE ONE THAT KILLS THE TWO-LINE EDIT, AND IT IS
// WORTH SAYING WHY A derived==declared CHECK ALONE DOES NOT. Unwrap a route and
// delist it and the derived set still equals the declared one — both simply
// shrank by the same entry. What does not change is that the route's handler
// can still reach s.ext.Provisioner with no nil check, and that is the property
// the wrapper exists for. So the wrapper requirement is decided by the CALL
// GRAPH, with the ledger out of the loop entirely.
//
// ⚠ THE REACHABILITY IS FUNCTION-GRANULAR AND INHERITS provisionerCallerLedger's
// KNOWN LIMIT, deliberately: a function that compares s.ext.Provisioner against
// nil ANYWHERE in its body is treated as guarded and stops propagation. That
// under-approximates — a function that checks on one path and calls on another
// hides everything below it — and it is the same approximation the caller
// ledger declares, so the two halves cannot disagree about what "guarded" means.
// Closing it needs dominance analysis; read a clean verdict here as "no
// registered route reaches the provisioner through an unguarded chain", never
// as "every call site is dominated by a check".
func TestEveryProvisionerRouteIsDerivedNotDeclared(t *testing.T) {
	regs, reaches, scanned := scanRouteRegistrations(t)

	// 🔴 POSITIVE CONTROLS, REPORTED AS NUMBERS. A walk that parsed nothing
	// derives an empty set, and an empty set compared against an empty ledger is
	// a green computed from silence. Each of these is a producer: the number has
	// to be able to move.
	if scanned < 10 {
		t.Fatalf("positive control FAILED: only %d non-test file(s) parsed in this "+
			"package, which declares far more. The walk is not measuring.", scanned)
	}
	if len(regs) < 50 {
		t.Fatalf("positive control FAILED: only %d route registration(s) were found, and "+
			"testdata/routes.golden records well over a hundred. The HandleFunc "+
			"detection has stopped matching, so every verdict below is over an empty "+
			"or truncated set.", len(regs))
	}
	reachable := 0
	for _, r := range regs {
		if r.handler != "" && reaches(r.handler) {
			reachable++
		}
	}
	if reachable == 0 {
		t.Fatalf("positive control FAILED: not ONE of the %d registered route(s) was "+
			"found to reach s.ext.Provisioner, and nine of them call it. The call-graph "+
			"walk is inert, so the \"reaches but is not wrapped\" verdict below is a "+
			"silence rather than a result.", len(regs))
	}

	// --- half 1: DERIVED == DECLARED ---------------------------------------
	var derived []string
	for _, r := range regs {
		if r.wrapped && !r.computed {
			derived = append(derived, r.route)
		}
	}
	sort.Strings(derived)
	want := append([]string(nil), provisionerRouteLedger...)
	sort.Strings(want)
	if strings.Join(derived, "\n") != strings.Join(want, "\n") {
		t.Errorf("the set of routes REGISTERED behind requireProvisioner is not the set "+
			"provisionerRouteLedger declares.\n  derived from source: %v\n  ledger:          "+
			"    %v\n"+
			"\n  SHRANK? A route lost its wrapper. On the deployment cmd/muster-server "+
			"ships it is back to answering 200 and doing nothing — a card rendered over a "+
			"row that never changes. Restore the wrapper; do not delete the ledger line to "+
			"match.\n  GREW? A route was wrapped without being recorded. Add it here so "+
			"the behavioural test above actually drives it — a wrapped route missing from "+
			"this list is never requested by any test in this file.",
			derived, want)
	}

	// --- half 2: REACHABILITY DECIDES, NOT THE LEDGER -----------------------
	for _, r := range regs {
		if r.handler == "" {
			continue // a function literal or a value this walk cannot name
		}
		can := reaches(r.handler)
		switch {
		case can && !r.wrapped:
			name := r.route
			if r.computed {
				name = "<computed pattern>"
			}
			t.Errorf("%s (%s:%d) is registered WITHOUT requireProvisioner, and its handler "+
				"%s reaches s.ext.Provisioner with no nil check.\n"+
				"    cmd/muster-server leaves Provisioner nil on purpose (doc_seams.go "+
				"entry 1), so on the deployment this tree ships that route either answers "+
				"200 and does nothing or panics in a goroutine. Wrap it — "+
				"s.requireSession(s.requireProvisioner(h)), auth OUTSIDE — and add it to "+
				"provisionerRouteLedger.\n"+
				"    Deleting the ledger entry does NOT satisfy this check: the ledger is "+
				"not consulted here, the call graph is.",
				name, r.file, r.line, r.handler)
		case !can && r.wrapped:
			t.Errorf("%s (%s:%d) is wrapped in requireProvisioner, but its handler %s was "+
				"NOT found to reach s.ext.Provisioner through this package's call graph.\n"+
				"    Either the wrapper is now unnecessary — in which case remove it AND "+
				"its provisionerRouteLedger line, having first confirmed nothing below it "+
				"calls the provisioner — or the call-graph walk has stopped resolving a "+
				"call it used to resolve, which would make the \"reaches but is not "+
				"wrapped\" half above silently blind. All nine wrapped routes reached when "+
				"this was written; a new failure here is far more likely to be the second.",
				r.route, r.file, r.line, r.handler)
		}
	}
	t.Logf("%d file(s) parsed; %d route registration(s), %d wrapped, %d reaching the provisioner",
		scanned, len(regs), len(derived), reachable)
}

// scanRouteRegistrations parses this package's non-test sources and returns
// every HandleFunc registration, plus a predicate answering whether a named
// function can reach s.ext.Provisioner unguarded through the package call graph.
func scanRouteRegistrations(t *testing.T) (regs []routeRegistration, reaches func(string) bool, scanned int) {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	callees := map[string][]string{}
	direct := map[string]bool{}
	guarded := map[string]bool{}

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			name := funcIdent(fn)
			calls, nilChecked := inspectProvisionerUse(fn.Body)
			if calls && nilChecked {
				guarded[name] = true
			}
			if calls && !nilChecked {
				direct[name] = true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				c, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fu := c.Fun.(type) {
				case *ast.SelectorExpr:
					// 🔴 Fun POSITION ONLY. `s.requireSession(s.handleAgentCreate)`
					// PASSES handleAgentCreate as a value; counting that as a call
					// would make registerAll reach every handler in the package and
					// the reachability verdict would be "everything", which is the
					// same as nothing.
					if id, ok := fu.X.(*ast.Ident); ok && id.Name == "s" {
						callees[name] = append(callees[name], "(*Server)."+fu.Sel.Name)
					}
					if fu.Sel.Name == "HandleFunc" && len(c.Args) == 2 {
						pos := fset.Position(c.Pos())
						r := routeRegistration{
							handler: innermostRouteHandler(c.Args[1]),
							file:    f,
							line:    pos.Line,
						}
						if lit, ok := c.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if s, err := strconv.Unquote(lit.Value); err == nil {
								r.route = s
							} else {
								r.computed = true
							}
						} else {
							// e.g. `"GET /"+tab` in the tab-route loop. Its pattern
							// cannot be compared against the ledger, but it is still
							// held to the reachability rule — a wrapped route must
							// not be able to hide behind a computed pattern.
							r.computed = true
						}
						ast.Inspect(c.Args[1], func(k ast.Node) bool {
							cc, ok := k.(*ast.CallExpr)
							if !ok {
								return true
							}
							if se, ok := cc.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "requireProvisioner" {
								r.wrapped = true
							}
							return true
						})
						regs = append(regs, r)
					}
				case *ast.Ident:
					callees[name] = append(callees[name], fu.Name)
				}
				return true
			})
			return true
		})
	}

	// Memoised DFS. 0 = in progress or proven unreachable, 1 = reaches; an
	// in-progress node reads as unreachable, which terminates recursion on a
	// cycle without claiming a path this walk has not established.
	memo := map[string]int{}
	var walk func(string) bool
	walk = func(n string) bool {
		if v, ok := memo[n]; ok {
			return v == 1
		}
		memo[n] = 0
		if guarded[n] {
			return false // the declared function-granular limit; see the caller
		}
		if direct[n] {
			memo[n] = 1
			return true
		}
		for _, c := range callees[n] {
			if walk(c) {
				memo[n] = 1
				return true
			}
		}
		return false
	}
	return regs, walk, scanned
}

// innermostRouteHandler names the function a registration expression ultimately
// serves: it descends through each wrapper's first argument until it reaches
// something that is not a call.
//
// ⚠ IT RETURNS "" RATHER THAN GUESSING. A function literal or a handler built
// some other way has no name this package's call graph can resolve, and naming
// the wrong one would put a reachability verdict on a function nobody registered.
func innermostRouteHandler(e ast.Expr) string {
	for {
		switch v := e.(type) {
		case *ast.CallExpr:
			if len(v.Args) == 0 {
				return ""
			}
			e = v.Args[0]
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok && id.Name == "s" {
				return "(*Server)." + v.Sel.Name
			}
			return ""
		case *ast.Ident:
			return v.Name
		default:
			return ""
		}
	}
}

// TestNoBackgroundGoroutineInThisPackageIsWrittenBare pins the F1 class.
//
// 🔴 A PANIC IN A GOROUTINE WITH NO recover TERMINATES THE PROCESS, AND
// recoverMiddleware DOES NOT COVER ONE. handleChiefProvision was the single bare
// `go func` among eight background goroutines in this package; two POSTs to that
// route took the container to `exited (2)` with /health connection-refused, on an
// ordinary operator sequence — click once, nothing happens, click again. safeGo
// is the house pattern and this asserts it is the ONLY one.
//
// ⚠ THE ONE PERMITTED SITE IS safeGo's OWN BODY, which is where the recover
// lives. Wrapping it in itself is infinite recursion, so it is exempted by
// LOCATION (the function named safeGo) rather than by a comment a reword could
// remove.
func TestNoBackgroundGoroutineInThisPackageIsWrittenBare(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	type site struct {
		fn   string
		file string
		line int
	}
	var bare []site
	total := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				g, ok := m.(*ast.GoStmt)
				if !ok {
					return true
				}
				total++
				if fn.Name.Name == "safeGo" {
					return true // the recover machinery itself
				}
				if goStmtHasRecover(g) {
					return true
				}
				bare = append(bare, site{fn.Name.Name, f, fset.Position(g.Pos()).Line})
				return true
			})
			return true
		})
	}

	// Positive control: the walk must be able to SEE a go statement at all.
	if total == 0 {
		t.Fatal("positive control FAILED: no `go` statement was found anywhere in this " +
			"package, and safeGo alone contains one. The walk is not measuring, so the " +
			"clean verdict below would be a silence rather than a result.")
	}

	for _, s := range bare {
		t.Errorf("BARE goroutine at %s:%d, in %s.\n"+
			"    A panic here has no recover and terminates the PROCESS — "+
			"recoverMiddleware covers the request goroutine only, and by the time this "+
			"runs the response has usually already been written. Use "+
			"safeGo(s.logger, \"<what>\", func(){...}); its recover logs the panic and "+
			"increments muster_panics_total{source=\"goroutine\"}.",
			s.file, s.line, s.fn)
	}
	t.Logf("%d `go` statement(s) in non-test sources; %d bare", total, len(bare))
}

// goStmtHasRecover reports whether a go statement's own function literal begins
// with a deferred recover, which is what makes a bare one safe.
func goStmtHasRecover(g *ast.GoStmt) bool {
	lit, ok := g.Call.Fun.(*ast.FuncLit)
	if !ok || lit.Body == nil {
		return false
	}
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		ast.Inspect(d, func(m ast.Node) bool {
			if id, ok := m.(*ast.Ident); ok && id.Name == "recover" {
				found = true
			}
			return true
		})
		return true
	})
	return found
}

// ---------------------------------------------------------------------------
// THE PROPERTY CONVERTING goNotify/mirrorToRouter TO safeGo COULD HAVE BROKEN.
// ---------------------------------------------------------------------------

// panickingRouter is a RouterPort whose fan-out methods panic, which is the case
// the conversion exists for. It is built from stubRouter so only the two methods
// under test are overridden.
type panickingRouter struct{ stubRouter }

func (panickingRouter) Notify(context.Context, RouterNotification) error {
	panic("router client exploded mid-notify")
}

func (panickingRouter) PublishEvent(context.Context, string, string) error {
	panic("router client exploded mid-publish")
}

// TestAPanickingFanOutStillReleasesTheShutdownWait is the guard for the ONE
// invariant the safeGo conversion could have silently broken.
//
// 🔴 pushInFlight's CONTRACT IS Add-ON-THE-CALLER, Done-IN-THE-GOROUTINE, AND A
// recover PLACED WRONG WOULD EAT THE Done. safeGo registers its recover OUTSIDE
// fn, so fn's own `defer s.pushInFlight.Done()` unwinds FIRST and the counter is
// released before the panic is caught. Had it been written the other way — the
// recover inside fn, above the defer — a panicking fan-out would leave the
// WaitGroup at 1 and every shutdown would hang to its full pushDrainTimeout,
// with nothing in the log naming why. That is a deadlock, which is strictly
// worse than the crash this conversion removed, so it gets its own assertion
// rather than riding on "the tests still pass".
//
// ⚠ THE PRE-CONVERSION CODE COULD NOT HAVE PASSED THIS TEST AT ALL — it would
// have taken the test binary down with it. That is the measurement: red by
// process death before, green after.
func TestAPanickingFanOutStillReleasesTheShutdownWait(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(os.Stderr, "", 0))
	s.UseRouter(panickingRouter{})

	// A DELTA, not an absolute read: metrics.Panics is a process-global counter
	// that other tests in this package also move, so an absolute threshold would
	// pass on their increments rather than on this test's.
	before := goroutinePanicCount()

	s.goNotify(RouterNotification{Tag: "a-tag"}, nil)
	s.mirrorToRouter("an-event", "some-data")

	done := make(chan struct{})
	go func() {
		s.WaitForPushes()
		close(done)
	}()

	select {
	case <-done:
		// Both fan-outs panicked, both were recovered, and both released their
		// slot. The process is still here to assert that.
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForPushes did not return within 5s after two PANICKING fan-outs.\n" +
			"    pushInFlight was left holding a slot, so every shutdown from now on " +
			"blocks for the full pushDrainTimeout and then exits anyway, logging a " +
			"drain timeout that names nothing. Check that Done() is fn's own defer " +
			"and that safeGo's recover is registered OUTSIDE fn — if the recover is " +
			"inside fn above the defer, it swallows the unwind that would have run it.")
	}

	// 🔴 POSITIVE CONTROL: THE PANICS MUST ACTUALLY HAVE HAPPENED. Without this
	// the test passes just as happily against a router that never panicked — and
	// would keep passing if someone deleted the panic from the stub, which is the
	// "green for the wrong reason" shape. The recovered-panic counter is the one
	// signal that distinguishes them.
	if got := goroutinePanicCount() - before; got < 2 {
		t.Fatalf("positive control FAILED: muster_panics_total{source=\"goroutine\"} rose "+
			"by %v across these two fan-outs, want at least 2. The fan-outs did not "+
			"panic, so the wait returning proves nothing about recovery — this test "+
			"would read exactly the same against a router that works.", got)
	}
}

// goroutinePanicCount reads the recovered-goroutine-panic counter safeGo writes.
func goroutinePanicCount() float64 {
	return testutil.ToFloat64(metrics.Panics.WithLabelValues("goroutine"))
}
