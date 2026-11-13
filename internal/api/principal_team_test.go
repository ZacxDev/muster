package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// teamUsers is a users store in which user `team` is a TEAM (users.kind =
// 'team') and everyone else a person: user 1 admin, the rest members.
type teamUsers struct{ team int64 }

func (u teamUsers) Role(_ context.Context, id int64) (authz.Role, error) {
	if id == authz.OperatorUserID && id != u.team {
		return authz.RoleAdmin, nil
	}
	return authz.RoleMember, nil
}

func (u teamUsers) Account(ctx context.Context, id int64) (users.Account, error) {
	r, _ := u.Role(ctx, id)
	return users.Account{Role: r, Team: id == u.team}, nil
}

// TestNoDoorAdmitsATeam is the ledger over every authenticated entry point:
// each route in routes.golden, through its REAL wrapper, with EACH door's
// credential (routeVias, which TestEveryRouteIsClassifiedForItsPrincipal pins
// to the golden — so a new route or door is walked here without anyone
// remembering to add it). Every door's credential resolves to user 1 in this
// harness; here user 1 is a TEAM, so every request must be refused with the
// team refusal and reach no handler. The CONTROL is the same request with
// user 1 a person: it reaches admit's hand-off, so each refusal is a fact
// about the kind, not about a malformed request.
func TestNoDoorAdmitsATeam(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	vias := make([]string, 0, len(routeVias))
	for v := range routeVias {
		vias = append(vias, string(v))
	}
	sort.Strings(vias)
	refused := 0
	for _, v := range vias {
		via := authz.Via(v)
		for _, pattern := range routeVias[via] {
			method, path := concretePath(pattern)
			t.Run(string(via)+" "+pattern, func(t *testing.T) {
				// Control: a person reaches the handler.
				s.ext.Users = teamUsers{team: -1}
				*seen = nil
				req := httptest.NewRequest(method, path, nil)
				credential(s, req, via)
				if aborted, code := serve(t, h, req); !aborted || len(*seen) != 1 {
					t.Fatalf("control: user 1 as a person did not reach the handler (status %d)", code)
				}
				// User 1 is a team: refused before any handler.
				s.ext.Users = teamUsers{team: authz.OperatorUserID}
				*seen = nil
				req = httptest.NewRequest(method, path, nil)
				credential(s, req, via)
				rec := httptest.NewRecorder()
				func() {
					defer func() {
						if r := recover(); r == errProbeAbort { //nolint:errorlint // identity of the sentinel
							t.Fatalf("a team's %s credential reached the handler", via)
						} else if r != nil {
							t.Fatalf("the request panicked instead of being refused: %v", r)
						}
					}()
					h.ServeHTTP(rec, req)
				}()
				if len(*seen) != 0 {
					t.Fatalf("a team's %s credential reached the handler as %v", via, *seen)
				}
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), TeamSignInRefusal) {
					t.Fatalf("a team's %s credential: %d %q; want 403 with the team refusal", via, rec.Code, rec.Body.String())
				}
				refused++
			})
		}
	}
	// Positive control on the walk: the golden's non-public routes were driven.
	if nonPublic := len(readGolden(t)) - len(publicRoutes); refused < nonPublic || nonPublic < 90 {
		t.Fatalf("refused %d route/door pairs; the golden has %d non-public routes", refused, nonPublic)
	}
}

// publicRouteTeamGuard says, for EVERY route that no muster door admits
// (publicRoutes), why a team cannot use it. It is pinned to publicRoutes in
// both directions, so a new unauthenticated entry point cannot ship without
// someone stating, here, how it keeps a team out.
var publicRouteTeamGuard = map[string]string{
	// No identity at all: nothing for a team to be.
	"GET /apple-touch-icon-precomposed.png": guardNoIdentity,
	"GET /apple-touch-icon.png":             guardNoIdentity,
	"GET /favicon.ico":                      guardNoIdentity,
	"GET /health":                           guardNoIdentity,
	"GET /login":                            guardNoIdentity,
	"GET /manifest.webmanifest":             guardNoIdentity,
	"GET /.well-known/assetlinks.json":      guardNoIdentity,
	"GET /api/device/web":                   guardNoIdentity,
	"GET /api/instance":                     guardNoIdentity,
	"GET /brand/logo.png":                   guardNoIdentity,
	"GET /brand/icon-192.png":               guardNoIdentity,
	"GET /brand/icon-512.png":               guardNoIdentity,
	"GET /metrics":                          guardNoIdentity,
	"GET /readyz":                           guardNoIdentity,
	"GET /static/":                          guardNoIdentity,
	"GET /sw.js":                            guardNoIdentity,
	"POST /logout":                          guardNoIdentity,
	"/llm/":                                 guardNoIdentity,
	"GET /auth/claim":                       guardNoIdentity,
	"GET /invite":                           guardNoIdentity,
	"GET /auth/reset":                       guardNoIdentity,
	// Sign-in doors: each yields a session for a user found by GoTrue
	// subject or email, which never names a team (users.BySub, IDByEmail,
	// CreateClaimInvite and the users_team_no_login CHECK:
	// TestATeamRowIsNeverASignInIdentity) — and the session then passes admit.
	"POST /login":                      guardSignIn,
	"POST /auth/claim":                 guardSignIn,
	"POST /invite":                     guardSignIn,
	"POST /auth/reset":                 guardSignIn,
	"POST /auth/passkey/options":       guardSignIn,
	"POST /auth/passkey/verify":        guardSignIn,
	"POST /plugin/v1/pair":             guardMintedByASession,
	"GET /plugin/v1/pair/{request_id}": guardMintedByASession,
	"POST /api/device/pair":            guardMintedByASession,
	"GET /f/{id}":                      "an HMAC over one upload's id; it names no user",
	// The mirror plugin's own doors: the token lookup refuses a token whose
	// owner is a team (mirror.PGStore.Authenticate:
	// TestATeamOwnedMirrorTokenIsRefused).
	"GET /mirror/connect": guardMirrorToken,
	"POST /mirror/ask":    guardMirrorToken,
}

const (
	guardNoIdentity       = "no identity: serves the same thing to everyone, or only starts a sign-in"
	guardSignIn           = "a sign-in that finds persons only"
	guardMintedByASession = "its single-use code is minted by a signed-in session, which admit refuses to a team"
	guardMirrorToken      = "the mirror token lookup refuses a team owner"
)

func TestEveryPublicRouteSaysHowATeamIsKeptOut(t *testing.T) {
	listed := map[string]bool{}
	for _, r := range publicRoutes {
		listed[r] = true
		if publicRouteTeamGuard[r] == "" {
			t.Errorf("public route %s has no entry in publicRouteTeamGuard: say how it keeps a team out", r)
		}
	}
	for r := range publicRouteTeamGuard {
		if !listed[r] {
			t.Errorf("publicRouteTeamGuard lists %s, which is not a public route", r)
		}
	}
	if len(publicRoutes) < 20 {
		t.Fatalf("publicRoutes has %d entries; the ledger is reading nothing", len(publicRoutes))
	}
}
