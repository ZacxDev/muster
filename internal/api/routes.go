// Package api will hold muster's HTTP surface.
//
// Today it holds exactly one thing: the single place a route may be
// registered. That is not a placeholder for want of anything better to write —
// it is the premise the route-drift gate in routes_golden_test.go rests on. The
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
// 🔴 IT IS EMPTY, NOT ABSENT, AND THE DIFFERENCE IS THE POINT. muster has no
// HTTP server yet; the routes arrive when the code carve does. An empty
// chokepoint that already exists means the first route anyone adds has an
// obvious and enforced place to go — and means the gate, the golden and the
// partition manifest are all in place and exercised BEFORE the traffic they
// describe, rather than being written afterwards against a surface that is
// already wrong.
//
// WHEN YOU ADD THE FIRST ROUTE, three things move together:
//
//   - register it here (or in a helper this function calls — the requirement
//     is that the registration is reachable from this one call, not that it is
//     literally in this body);
//   - flip `routePhase` in routes_golden_test.go from `pre-traffic` to
//     `carving`, which turns the drift gate on;
//   - regenerate testdata/routes.golden and read the diff.
//
// Skip the second and the gate's own pre-traffic control will fail and tell
// you so. That is deliberate: a route landing while the gate still believes
// there are none is the one failure mode this whole file exists to make loud.
func RegisterRoutes(mux Mux) {}
