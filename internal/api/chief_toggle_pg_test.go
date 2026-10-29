package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// TestTurningTheChiefOnCreatesItAndARefusedCreateLeavesItOff: on real
// Postgres (row-level security, the stored switch, the chief's reserved slug)
// with an agent provisioner, a member who never had a chief:
//
//   - reads OFF on the Account page, and home is the Agents list;
//   - turning it on with no model credential is refused, says why, and stores
//     NOTHING (it stays off, no agent);
//   - with a key, turning it on creates THEIR chief, stores on, and home
//     opens it;
//   - turning it off stores off, sends home back to the Agents list, and keeps
//     the agent;
//   - the operator's switch is untouched throughout.
//
// Red on the base tree: POST /account/chief did not exist.
func TestTurningTheChiefOnCreatesItAndARefusedCreateLeavesItOff(t *testing.T) {
	w := newIsoWorld(t)
	b := cred{sessionUser: w.b.uid}
	slug := agents.ChiefNameFor(w.b.uid)
	t.Cleanup(func() {
		_, _ = w.pool.Exec(w.sys, `DELETE FROM agents WHERE name = $1`, slug)
		_, _ = w.pool.Exec(w.sys, `DELETE FROM chief_preferences WHERE owner_id = $1`, w.b.uid)
	})
	state := func() string {
		body := w.do(w.h, "GET", "/account", "", b).Body.String()
		const attr = `id="chief-state" data-state="`
		i := strings.Index(body, attr)
		if i < 0 {
			t.Fatalf("B's Account page has no chief state:\n%s", truncate(body, 600))
		}
		rest := body[i+len(attr):]
		return rest[:strings.Index(rest, `"`)]
	}
	stored := func(owner int64) (bool, bool) {
		var on bool
		err := w.pool.QueryRow(w.sys, `SELECT enabled FROM chief_preferences WHERE owner_id = $1`, owner).Scan(&on)
		return err == nil, on
	}
	// The fixture's session requests are htmx's, so home answers HX-Redirect.
	home := func() string { return w.do(w.h, "GET", "/", "", b).Header().Get("HX-Redirect") }
	agentCount := func() int {
		var n int
		if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM agents WHERE name = $1`, slug).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	_, opBefore := stored(authz.OperatorUserID)
	opChoseBefore, _ := stored(authz.OperatorUserID)

	if got := state(); got != "off" {
		t.Fatalf("a member with no chief: Account reads %q, want off", got)
	}
	if got := home(); got != "/agents" {
		t.Fatalf("a member with no chief: GET / → %q", got)
	}

	rec := w.do(w.h, "POST", accountChiefPath, "on=1", b)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account?chief=refused#chief" {
		t.Fatalf("on with no credential: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if chose, _ := stored(w.b.uid); chose {
		t.Error("a refused create stored a choice")
	}
	if n := agentCount(); n != 0 {
		t.Errorf("a refused create left %d chief agent(s)", n)
	}
	if got := state(); got != "off" {
		t.Errorf("after a refused create Account reads %q, want off", got)
	}

	bctx := authz.WithPrincipal(t.Context(), authz.Principal{UserID: w.b.uid, Role: authz.RoleMember, Via: authz.ViaSession})
	if err := w.creds.PutOpenRouter(bctx, "sk-or-v1-iso-member-B-api-key-0004444"); err != nil {
		t.Fatal(err)
	}
	rec = w.do(w.h, "POST", accountChiefPath, "on=1", b)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account?chief=saved#chief" {
		t.Fatalf("on: %d → %q %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if chose, on := stored(w.b.uid); !chose || !on {
		t.Fatalf("on stored chose=%v on=%v", chose, on)
	}
	if n := agentCount(); n != 1 {
		t.Fatalf("on created %d chief agent(s), want 1", n)
	}
	if got := home(); !strings.HasPrefix(got, "/agents/"+slug) {
		t.Errorf("on: GET / → %q, want B's chief", got)
	}
	if got := state(); got != "on" {
		t.Errorf("on: Account reads %q", got)
	}

	if rec := w.do(w.h, "POST", accountChiefPath, "on=0", b); rec.Code != http.StatusSeeOther {
		t.Fatalf("off: %d %s", rec.Code, rec.Body)
	}
	if chose, on := stored(w.b.uid); !chose || on {
		t.Fatalf("off stored chose=%v on=%v", chose, on)
	}
	if got := home(); got != "/agents" {
		t.Errorf("off: GET / → %q", got)
	}
	if n := agentCount(); n != 1 {
		t.Errorf("off removed the chief agent (%d left)", n)
	}
	if chose, on := stored(authz.OperatorUserID); chose != opChoseBefore || on != opBefore {
		t.Errorf("B's switch moved the operator's: chose=%v on=%v", chose, on)
	}
}

// TestAnExistingChiefStaysOnOnPostgres: the owner's live shape, on real
// Postgres — a chief agent made before the switch existed (labelled chief,
// no stored choice) is ON: home opens it and Account reads on.
func TestAnExistingChiefStaysOnOnPostgres(t *testing.T) {
	w := newIsoWorld(t)
	b := cred{sessionUser: w.b.uid}
	name := "iso-existing-chief-" + w.suffix
	if _, err := w.pool.Exec(w.sys, `INSERT INTO agents (owner_id, name, namespace, display_name, status, kicked_off)
		VALUES ($1, $2, $3, 'chief', 'running', true)`, w.b.uid, name, "devpod-"+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.pool.Exec(w.sys, `DELETE FROM agents WHERE name = $1`, name) })
	if got := w.do(w.h, "GET", "/", "", b).Header().Get("HX-Redirect"); !strings.HasPrefix(got, "/agents/"+name) {
		t.Errorf("GET / → %q, want the existing chief %q", got, name)
	}
	body := w.do(w.h, "GET", "/account", "", b).Body.String()
	if !strings.Contains(body, `id="chief-state" data-state="on"`) {
		t.Errorf("Account does not read on:\n%s", truncate(body, 800))
	}
}
