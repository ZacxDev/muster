package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/users"
)

// ROUND 8 (passkey_enrol.go openTier): the open tier is granted only to a
// session that is good by a FRESH read of its account's cutoff, so a cutoff
// moved in another process (the server recovery, a reset on another replica)
// is not undone by this process's user cache. Red at the previous head.

// TestARecoveredAccountIsNotOpenToItsOldSession: 🔴 R8-1, reproduced. Admin A
// (gated) is recovered by `reset-factors` from another process: one commit
// moves A's cutoff and clears its mark, so A reads open. Within the user
// cache's window the attacker's old session of A is still admitted — and must
// not be granted the open tier: promoting a pre-invited B takes no proof from
// an open admin, so it is refused here.
func TestARecoveredAccountIsNotOpenToItsOldSession(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	if err := w.s.markStrongFactor(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b, _, _, _ := w.joinMember("pre-invited-puppet@invite.test")
	// Warm this process's user cache with A's row as it is now.
	if c := do(w.edge, "GET", "/account", nil, w.asAdmin()).Code; c != http.StatusOK {
		t.Fatalf("fixture: A's session: %d", c)
	}
	// The recovery's database half, from "another process": no forgetUser here.
	if _, err := users.NewPG(w.pool).ResetFactors(context.Background(), users.Actor{Via: "cli"}, a, nil); err != nil {
		t.Fatal(err)
	}
	if had, _ := w.s.hadStrongFactor(context.Background(), a); had {
		t.Fatal("fixture: the recovery left A marked")
	}
	promote := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(b, 10)+"/role", url.Values{"role": {"admin"}}, w.asAdmin())
	if promote.Code == http.StatusSeeOther {
		t.Fatal("the recovered account's OLD session promoted B with no proof")
	}
	if role := w.roleOf(t, b); role != "member" {
		t.Fatalf("B after the refused promotion: %s", role)
	}
	// The control: after the refusal, this process has dropped its cached row,
	// so the old session is now refused outright (the cutoff).
	if c := do(w.edge, "GET", "/account", nil, w.asAdmin(), asXHR).Code; c != http.StatusUnauthorized {
		t.Fatalf("control: the old session after the fresh read: %d, want 401", c)
	}
}
