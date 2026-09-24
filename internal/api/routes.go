// This file holds the single place a route may be registered. That is not
// bookkeeping — it is the premise the route-drift gate in
// routes_golden_test.go rests on. The
// gate hands a recorder to RegisterRoutes and diffs what comes back against a
// checked-in golden file. That recording is a claim about production only for
// as long as RegisterRoutes is the ONLY registration site in the module; the
// moment a second one exists, the golden describes a subset and says nothing
// about the rest.
//
// So the chokepoint ships before the routes do, and the gate checks the
// premise rather than assuming it.
package api

import "net/http"

// Mux is the narrow slice of *http.ServeMux that RegisterRoutes uses.
//
// It exists so the route gate can hand RegisterRoutes a recorder instead of a
// real mux and get back the set of patterns production would register. The
// compile-time assertion below is what makes the two interchangeable — without
// it, "the recorder and the mux are the same shape" would be a comment rather
// than a fact the compiler enforces.
type Mux interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// A real *http.ServeMux must satisfy Mux, or the gate is measuring a shape
// production does not use.
var _ Mux = (*http.ServeMux)(nil)

// RegisterRoutes registers every muster HTTP route on mux.
//
// 🔴 IT TAKES NO SERVER, AND THAT IS WHAT MAKES THE ROUTE GOLDEN A CLAIM ABOUT
// PRODUCTION RATHER THAN ABOUT A FIXTURE.
//
// The body registers against a ZERO-VALUE Server. That is only sound because
// registration in this package is unconditional — no `if dep == nil { return }`
// anywhere in the call tree, see the package doc in server.go — so a bare
// server and a fully-wired one register the identical set. Handler() calls the
// same registerAll against the real server.
//
// The alternative, which upstream has, is a registrar that branches on which
// dependencies are present. Then "which routes exist" is a function of the
// fixture the recorder was handed, and a fixture missing one field produces a
// golden that is short by a whole block while reading as complete. That is not
// hypothetical: it cost that project seven routes, with two separate guards
// green over the hole.
//
// TestRegistrationIsIndependentOfDependencies is the mechanical check. It is a
// RELATIONSHIP guard — it records from a bare server AND from one with every
// dependency supplied and demands set equality — not a count, because a count
// passes whether or not the two agree.
func RegisterRoutes(mux Mux) {
	(&Server{}).registerAll(mux)
}
