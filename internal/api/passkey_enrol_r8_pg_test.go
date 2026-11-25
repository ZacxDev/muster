package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
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

// TestATicketDoesNotOutliveARecovery: 🟡 an enrolment ticket lives in this
// process's memory for ten minutes. Admin A proves (a vouched ticket); then
// the recovery's database half runs from the CLI (another process), cutting
// off that session — with this process's user cache still warm. The old
// session's ticket adds no passkey.
func TestATicketDoesNotOutliveARecovery(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	if err := w.s.markStrongFactor(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	grantEnrolTicket(t, w.s, w.fake, a, w.adminAT)
	if c := do(w.edge, "GET", "/account", nil, w.asAdmin()).Code; c != http.StatusOK {
		t.Fatalf("fixture: A's session: %d", c)
	}
	if _, err := users.NewPG(w.pool).ResetFactors(context.Background(), users.Actor{Via: "cli"}, a, nil); err != nil {
		t.Fatal(err)
	}
	o := postJSON(w.edge, accountPasskeysPath+"/options", map[string]any{}, w.asAdmin())
	if o.Code == http.StatusOK {
		ch, _ := jsonBody(t, o)["challenge_id"].(string)
		v := postJSON(w.edge, accountPasskeysPath+"/verify", map[string]any{
			"challenge_id": ch, "credential": gotruetest.Attestation("after-recovery", gotruetest.FlagUPUV)}, w.asAdmin())
		if v.Code == http.StatusOK {
			t.Fatal("a ticket from before the recovery added a passkey on the old session")
		}
	}
	if n := len(w.fake.Passkeys(adminSub(t, w))); n != 0 {
		t.Fatalf("%d passkey(s) on the recovered account", n)
	}
}

// TestAnAdminProofFromACutOffSessionIsNotHonoured: the admin pages' proof is a
// ticket too. Gated admin A proves; A's cutoff is then moved from the CLI
// (another process) with this process's cache warm; the old session's ticket
// promotes no one.
func TestAnAdminProofFromACutOffSessionIsNotHonoured(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	if err := w.s.markStrongFactor(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b, _, _, _ := w.joinMember("cutoff-puppet@invite.test")
	grantEnrolTicket(t, w.s, w.fake, a, w.adminAT)
	if c := do(w.edge, "GET", "/account", nil, w.asAdmin()).Code; c != http.StatusOK {
		t.Fatalf("fixture: A's session: %d", c)
	}
	if _, err := w.pool.Exec(authz.System(context.Background()), `UPDATE users SET sessions_not_before = clock_timestamp() WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if c := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(b, 10)+"/role", url.Values{"role": {"admin"}}, w.asAdmin()).Code; c == http.StatusSeeOther {
		t.Fatal("a proof from a session cut off since promoted B")
	}
	if role := w.roleOf(t, b); role != "member" {
		t.Fatalf("B: %s", role)
	}
}

// TestARowOnlyDisableIsNotOpen: sessionFresh's disabled check is not the
// cutoff check twice. A disable written to the row alone (disabled_at, no
// cutoff moved), with this process's cache still warm, admits the session —
// but grants it no open tier: promoting B is refused.
func TestARowOnlyDisableIsNotOpen(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	mustBeUnmarked(t, w, a)
	b, _, _, _ := w.joinMember("disabled-puppet@invite.test")
	if c := do(w.edge, "GET", "/account", nil, w.asAdmin()).Code; c != http.StatusOK {
		t.Fatalf("fixture: A's session: %d", c)
	}
	if _, err := w.pool.Exec(authz.System(context.Background()), `UPDATE users SET disabled_at = now() WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(authz.System(context.Background()), `UPDATE users SET disabled_at = NULL WHERE id = $1`, a)
	})
	if c := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(b, 10)+"/role", url.Values{"role": {"admin"}}, w.asAdmin()).Code; c == http.StatusSeeOther {
		t.Fatal("a row-only disabled account's cached session was granted the open tier")
	}
	if role := w.roleOf(t, b); role != "member" {
		t.Fatalf("B: %s", role)
	}
}
