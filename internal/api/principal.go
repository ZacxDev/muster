package api

import (
	"net/http"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// This file is where every authentication wrapper turns "the credential
// checked out" into "this request acts as THIS user" (accounts PR 2/6).
//
// 🔴 admit IS THE ONLY CALL SITE OF authz.WithPrincipal IN THE MODULE, AND
// EVERY require* WRAPPER HANDS ITS ADMITTED REQUEST TO next THROUGH IT. One
// function is one place to get the attach right; a wrapper that called next
// directly would reach its handler with no principal, which under PR 3's
// row-level security is an empty board and a failing insert.
// TestEveryAuthenticatedRouteAttachesAPrincipal drives every route in
// routes.golden through its real wrapper and demands the principal (with the
// right Via) arrive at admit's hand-off; TestOnlyAdmitAttachesAPrincipal pins
// the call-site ledger.
//
// ⚠ NO BEHAVIOUR CHANGES HERE. muster has one user today, so every principal
// names authz.OperatorUserID as admin; nothing reads the principal yet.

// machineUserID is who the SHARED machine tokens (hook, service) act as.
//
// ⚠ A FIXED VALUE TODAY. The accounts plan (§5) makes it MUSTER_MACHINE_USER_ID
// (default 1) once there is a second user to choose between; until then a
// variable would be a knob with exactly one legal setting.
func machineUserID() int64 { return authz.OperatorUserID }

// sessionUserID is who a valid browser session acts as. The password mode has
// exactly one login, the operator's, so it is user 1 by construction; PR 4's
// GoTrue mode resolves it from the token's subject instead.
func sessionUserID() int64 { return authz.OperatorUserID }

// agentOwnerID is who a request authenticated by an AGENT'S OWN token acts as:
// the agent's owner.
//
// 🔴 THIS IS THE ONE FUNCTION PR 3 CHANGES TO `return a.OwnerID`. Every
// per-agent door (requireAgentToken, requireConnectorAgent, requireRelayAgent)
// resolves its row and derives the user here, so the switch from "the
// operator" to "the row's owner" is one line, not three wrappers. Today the
// agents table has no owner column (PR 1 adds it, default 1), so every agent
// is the operator's.
func agentOwnerID(_ agents.Agent) int64 { return authz.OperatorUserID }

// roleOf is the role a user acts with. Today the only user is the operator,
// who is admin; PR 3/4 read users.role.
func roleOf(_ int64) authz.Role { return authz.RoleAdmin }

// principalFor builds the request principal for a user admitted through via.
func principalFor(uid int64, via authz.Via) authz.Principal {
	return authz.Principal{UserID: uid, Role: roleOf(uid), Via: via}
}

// admit attaches p to the request and hands it to next. See the file header:
// it is the only way a wrapper may call next.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, p authz.Principal) {
	r = r.WithContext(authz.WithPrincipal(r.Context(), p))
	if s.principalProbe != nil {
		s.principalProbe(r)
	}
	next(w, r)
}
