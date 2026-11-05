package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestAFeatureOffRouteIsReportedAsDisabledNotAsVersionSkew: `muster route
// reply` against a server whose session routing is off. The server's typed 404
// ({"error":"feature_disabled"}) prints "feature session-routing is disabled on
// this server — enable with: …" and exits 12; a server that does not have the
// route at all (the mux's text/plain 404) still gets the version-skew message
// and exit 7; an ordinary JSON 404 is still "not found" (exit 4).
func TestAFeatureOffRouteIsReportedAsDisabledNotAsVersionSkew(t *testing.T) {
	const enable = "set EXAMPLE_ROUTING=1 in .env and restart (docs/features.md#session-routing)"
	for _, c := range []struct {
		name    string
		set     func(h *harness)
		code    int
		want    []string
		mustNot []string
	}{
		{"typed feature-off 404", func(h *harness) {
			h.json("POST /api/routes/rt_0123456789abcdef01234567/reply", http.StatusNotFound, `{"error":"feature_disabled","feature":"session-routing",`+
				`"state":"off","enable":"`+enable+`","docs":"docs/features.md#session-routing"}`)
		}, exitFeatureDisabled,
			[]string{"feature session-routing is disabled on this server — enable with: " + enable},
			[]string{"predate", "not found on this server"}},
		{"misconfigured, with its reason", func(h *harness) {
			h.json("POST /api/routes/rt_0123456789abcdef01234567/reply", http.StatusNotFound, `{"error":"feature_disabled","feature":"session-routing",`+
				`"state":"misconfigured","reason":"plugins are off","enable":"`+enable+`"}`)
		}, exitFeatureDisabled,
			[]string{"feature session-routing is misconfigured on this server (plugins are off) — enable with: " + enable}, nil},
		{"the mux's own 404 (no such route)", func(h *harness) {}, exitRouteAbsent,
			[]string{"not found on this server; it may predate the route"}, []string{"is disabled"}},
		{"a resource 404", func(h *harness) {
			h.json("POST /api/routes/rt_0123456789abcdef01234567/reply", http.StatusNotFound, `{"error":"no such route"}`)
		}, exitNotFound, []string{"not found — no such route"}, []string{"is disabled", "predate"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.set(h)
			got := h.runCLI("route", "reply", "rt_0123456789abcdef01234567", "--body", "an answer")
			if got.code != c.code {
				t.Fatalf("exit %d, want %d (stderr %q)", got.code, c.code, got.stderr)
			}
			for _, w := range c.want {
				if !strings.Contains(got.stderr, w) {
					t.Errorf("stderr %q lacks %q", got.stderr, w)
				}
			}
			for _, w := range c.mustNot {
				if strings.Contains(got.stderr, w) {
					t.Errorf("stderr %q says %q", got.stderr, w)
				}
			}
		})
	}
}
