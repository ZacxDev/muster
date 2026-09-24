package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/router"
)

// ---------------------------------------------------------------------------
// THE READINESS DOOR, DRIVEN THROUGH THE BINARY'S OWN WIRING.
//
// 🔴 api.Extensions.defects DRAWS THE LINE THIS FILE ASSERTS: a dependency that
// DEGRADES VISIBLY is a configuration, and a dependency whose absence makes the
// page STATE A FALSEHOOD is a defect that must not be served with. A nil
// Runbooks store renders an empty Runbooks list, which is TRUE — this server has
// no runbooks. A notes store with no session-liveness probe renders "no
// transcript recorded" over transcripts that may be perfectly alive, with
// nothing logged and nothing 404ing.
//
// The binary is where that line is either respected or quietly crossed, because
// the binary is what decides which fields are nil. buildSessionLiveness makes
// that decision and these tests drive it through /readyz — the door itself —
// rather than asserting on the decision function's return value, which would
// pass just as happily if nothing consumed it.
// ---------------------------------------------------------------------------

// dbConfig is testConfig plus a real database, which is what makes a notes store
// exist — and therefore what makes the defect reachable at all.
func dbConfig(t *testing.T) config {
	t.Helper()
	cfg := testConfig()
	cfg.Database = dbtest.DSN(t)
	return cfg
}

// readyz drives GET /readyz and returns the status code and decoded body.
func readyz(t *testing.T, base string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding /readyz: %v", err)
	}
	return resp.StatusCode, body
}

// TestReadyzRefusesAnUndeclaredMissingRouter is the negative case, and it is the
// one that matters.
//
// 🔴 "THERE IS NO ROUTER" AND "I FORGOT TO CONFIGURE THE ROUTER" ARE THE SAME
// OBSERVABLE AND MUST NOT GET THE SAME BEHAVIOUR. With neither MUSTER_ROUTER_URL
// nor MUSTER_STANDALONE set, this server has a notes store and no way to ask
// whether a transcript exists — so every transcript link on every surface would
// render a confident falsehood. The pod stays OUT of rotation until an operator
// decides which of the two situations they are in.
func TestReadyzRefusesAnUndeclaredMissingRouter(t *testing.T) {
	cfg := dbConfig(t)
	cfg.Standalone = false // nobody declared anything
	cfg.RouterURL = ""

	code, body := readyz(t, startServer(t, cfg))

	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want 503.\n"+
			"  A server with a notes store and no session-liveness probe renders\n"+
			"  \"no transcript recorded\" over live transcripts, silently, everywhere.\n"+
			"  body: %v", code, body)
	}
	// 🔴 THE REASON IS ASSERTED, NOT JUST THE CODE. A 503 for an unrelated cause
	// — an unreachable database, say — would satisfy a status-only assertion and
	// this test would be green for the wrong reason, with the defect check
	// deleted. The detail names the field.
	detail := strings.ToLower(join(body["detail"]))
	if !strings.Contains(detail, "sessionliveness") {
		t.Errorf("/readyz refused, but not for the wiring defect: detail = %v.\n"+
			"  This test would pass on any 503; it is asserting the reason.", body["detail"])
	}
}

// TestReadyzAcceptsADeclaredStandaloneDeployment is the positive control for the
// test above.
//
// 🔴 WITHOUT IT, TestReadyzRefusesAnUndeclaredMissingRouter IS SATISFIED BY A
// SERVER THAT IS NEVER READY FOR ANY REASON. A refusal test alone cannot tell a
// working door from a jammed one. The only difference between these two cases is
// MUSTER_STANDALONE, so this pins that the flag is the thing that moves the
// answer — and that the standalone deployment is genuinely deployable rather
// than a mode nobody can reach.
func TestReadyzAcceptsADeclaredStandaloneDeployment(t *testing.T) {
	cfg := dbConfig(t)
	cfg.Standalone = true
	cfg.RouterURL = ""

	code, body := readyz(t, startServer(t, cfg))

	if code != http.StatusOK {
		t.Fatalf("GET /readyz = %d with %s=1, want 200.\n"+
			"  A deployment that has declared it has no session store is a supported\n"+
			"  posture, not a defect. body: %v", code, envStandalone, body)
	}
	if body["status"] != "ready" {
		t.Errorf("/readyz status = %v, want \"ready\"", body["status"])
	}
}

// TestTheStandaloneProbeReportsAbsenceWithoutAnError pins the one thing that
// makes the standalone probe honest rather than a stub.
//
// 🔴 A FAILED LIVENESS READ IS AN ERROR, NOT A false — see session_liveness.go,
// which refuses to degrade a transient fault into the permanent-looking sentence
// "no transcript recorded". This probe returns the OPPOSITE: no error, and no
// ids present. That is not the same shape and the difference is the whole
// justification for it existing. A deployment with no session store genuinely
// has no transcript for any id; the read did not fail, it succeeded and found
// nothing.
func TestTheStandaloneProbeReportsAbsenceWithoutAnError(t *testing.T) {
	got, err := standaloneSessionLiveness{}.SessionsExisting(
		context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("SessionsExisting returned %v; a standalone deployment's answer is "+
			"\"none of them\", which is not a failure", err)
	}
	if len(got) != 0 {
		t.Errorf("SessionsExisting returned %v, want an empty map — nothing records "+
			"transcripts in a standalone deployment", got)
	}
}

// TestAnUnconfiguredRouterPortNeverReachesTheLivenessField is the guard on the
// third way this wiring can go wrong, and it is the one no amount of reading
// catches.
//
// 🔴 router.Port SATISFIES api.SessionLivenessProbe EVEN WHEN ITS CLIENT IS nil.
// Assigning an unconfigured Port to that field therefore produces a server whose
// defect check PASSES — the field is non-nil — and whose every board read fails
// with router.ErrNotConfigured, because a failed liveness read is an error by
// design. /readyz would say ready and the task board would be a 500. The guard
// is that buildSessionLiveness must not return the Port unless Configured().
//
// ⚠ IT ASSERTS THE RETURNED VALUE'S IDENTITY, NOT MERELY THAT IT IS NON-nil. A
// non-nil check would be satisfied by the very bug it is looking for.
func TestAnUnconfiguredRouterPortNeverReachesTheLivenessField(t *testing.T) {
	unconfigured := router.NewPort(router.Config{}) // no URL, no token, no actor
	if unconfigured.Configured() {
		t.Fatal("positive control FAILED: a router.Port built from an empty Config " +
			"reports Configured(); this test is measuring nothing")
	}

	cases := []struct {
		name string
		cfg  config
		want string // "nil" | "standalone" | "port"
	}{
		{"undeclared, no router", config{}, "nil"},
		{"declared standalone", config{Standalone: true}, "standalone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSessionLiveness(tc.cfg, unconfigured, quietLogger())
			switch tc.want {
			case "nil":
				if got != nil {
					t.Fatalf("got %T, want nil. An unconfigured router.Port assigned here "+
						"makes /readyz say ready and every board read 500.", got)
				}
			case "standalone":
				if _, ok := got.(standaloneSessionLiveness); !ok {
					t.Fatalf("got %T, want standaloneSessionLiveness", got)
				}
			}
		})
	}

	// And the configured case: the port itself, not the standalone declaration.
	configured := router.NewPort(router.Config{
		BaseURL: "https://router.invalid",
		Token:   "a-token-long-enough-to-be-accepted-by-New",
		Actor:   defaultRouterActor,
	})
	if !configured.Configured() {
		t.Fatal("positive control FAILED: a fully specified router.Config produced an " +
			"unconfigured Port")
	}
	// Standalone is set too, to prove the router WINS rather than merely being
	// consulted when nothing else is.
	got := buildSessionLiveness(config{Standalone: true}, configured, quietLogger())
	if _, ok := got.(standaloneSessionLiveness); ok {
		t.Fatal("a configured router lost to the standalone declaration. The router is " +
			"the only thing that can answer the question truthfully when one exists.")
	}
	if _, ok := got.(router.Port); !ok {
		t.Fatalf("got %T, want router.Port", got)
	}
}

// join renders /readyz's `detail` (a JSON array of strings) for matching.
func join(v any) string {
	items, ok := v.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, it := range items {
		b.WriteString(" ")
		if s, ok := it.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}
