package api

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE OFFICERS SCREENS IN PLAIN WORDS (jargon audit, "settings" theme: F047,
// F050–F055, U-24). F048, F049, F056 and F057 landed with #829.

// TestTheOfficerListCallsABuiltInOfficerByName (F047, F050): a built-in
// officer with no name is listed as "Marketing — Acme", badged "Built-in" —
// not as "marketing:acme" with "read-only".
func TestTheOfficerListCallsABuiltInOfficerByName(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	list := s.officerList(asRole(authz.RoleAdmin), officerScope{Owner: 1, Manage: true},
		[]declaredOfficerView{{ID: "marketing:acme", Owner: 1, ManagedBy: managedByDef, ReadOnly: true}}, nil, nil)
	for _, want := range []string{`>Marketing — Acme</a>`, `<span class="text-xs text-muted" data-read-only>Built-in</span>`} {
		if !strings.Contains(list, want) {
			t.Errorf("the officer list has no %s\n%s", want, list)
		}
	}
	for _, gone := range []string{`>marketing:acme</a>`, `>read-only<`} {
		if strings.Contains(list, gone) {
			t.Errorf("the officer list still draws %s", gone)
		}
	}
}

// TestWhereOfficersCanRunIsSaidInWords (F051, F055): on a server that can run
// an officer nowhere, "Where officers can run here" says "muster's own
// servers: not set up.", that your computers are not available, and "nowhere
// yet — connect a computer first" — no pod, provisioner, flag or variable in
// its top layer; the reasons are in an ADMIN's ⓘ, and a member's carries
// none of them.
func TestWhereOfficersCanRunIsSaidInWords(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	admin := s.runtimeAvailability(asRole(authz.RoleAdmin), 1, "officers-runtimes")
	member := s.runtimeAvailability(asRole(authz.RoleMember), 1, "officers-runtimes")
	for role, page := range map[string]string{"admin": admin, "member": member} {
		for _, want := range []string{
			`<span class="font-semibold text-fg">muster&#39;s own servers:</span> <span class="text-fg2">not set up.</span>`,
			`<span class="font-semibold text-fg">Your computers:</span> <span class="text-fg2">not available on this server.</span>`,
			`id="officers-runtimes-auto">nowhere yet — connect a computer first</span>`,
		} {
			if !strings.Contains(page, want) && !strings.Contains(page, strings.ReplaceAll(want, "&#39;", "'")) {
				t.Errorf("%s: no %s\n%s", role, want, page)
			}
		}
		if n := noJargonInTopLayer(t, role, page); n < 5 {
			t.Errorf("CONTROL: %s: the scan read %d text(s)", role, n)
		}
		for _, gone := range []string{"Pods:", "Your machines", "neither a pod"} {
			if strings.Contains(page, gone) {
				t.Errorf("%s: the panel still says %q", role, gone)
			}
		}
	}
	if !strings.Contains(admin, "pods need the Kubernetes agent plane") {
		t.Error("CONTROL: the admin's ⓘ does not hold the server's reason")
	}
	if strings.Contains(member, "Kubernetes") || strings.Contains(member, "provisioner") {
		t.Error("a member's panel carries the server's reason")
	}
}

// TestAComputersReasonIsSaidInWords (F053, F054): the reason a computer
// cannot start an officer, as its readiness gives it (hostReadyRefusal), is
// drawn in words — the refusal itself is the row's ⓘ.
func TestAComputersReasonIsSaidInWords(t *testing.T) {
	for _, c := range []struct {
		name string
		v    mirror.HostView
		want string
	}{
		{"an older muster-mirror", mirror.HostView{Host: "nomad"}, "needs an update to run officers"},
		{"agent spawn off", mirror.HostView{Host: "atlas", SpawnReady: &mirror.SpawnReadiness{Unavailable: "agent spawn is off"}}, "can't start officers"},
		{"no claude", mirror.HostView{Host: "atlas", SpawnReady: &mirror.SpawnReadiness{Scratch: true, Claude: mirror.ClaudeMissing, ClaudeDetail: "not on PATH"}}, "can't find Claude Code"},
		{"not logged in", mirror.HostView{Host: "atlas", SpawnReady: &mirror.SpawnReadiness{Scratch: true, Claude: mirror.ClaudeFound, Login: mirror.LoginNo}}, "needs Claude Code signed in"},
	} {
		why := hostReadyRefusal(c.v, "", true)
		if why == "" {
			t.Fatalf("%s: CONTROL: the fixture is not refused", c.name)
		}
		if got := hostReasonWords(why); got != c.want {
			t.Errorf("%s: %q reads %q, want %q", c.name, why, got, c.want)
		}
	}
}

// TestAnOfficersAskIsShownByWhatItIsFor (U-24): an ask a built-in officer
// raised is shown as from "Product officer (Acme)", never "officer
// product:acme" — and its stored label, the key the officer's page matches
// its asks by, is unchanged.
func TestAnOfficersAskIsShownByWhatItIsFor(t *testing.T) {
	a := attention.Ask{ID: 5, Owner: 1, Asker: attention.Asker{System: true, Label: officerAskLabel("product:acme")}}
	if got := askerLabel(a); got != "Product officer (Acme)" {
		t.Errorf("the ask's source = %q", got)
	}
	if got := raisedByOfficer([]attention.Ask{a}, 1, "product:acme"); len(got) != 1 {
		t.Error("the officer's page no longer finds its ask")
	}
	// A session's own label is shown as it was stored (the control: only an
	// officer's system label is read into words).
	b := attention.Ask{ID: 6, Owner: 1, Asker: attention.Asker{Label: "officer product:acme"}}
	if got := askerLabel(b); got != "officer product:acme" {
		t.Errorf("a non-system ask's label was rewritten: %q", got)
	}
}
