package api

import (
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
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

// TestWhereOfficersCanRunWithholdsEveryReasonFromAMember (F051–F054; audit
// round 1): each place's reason and the command that adds a computer are in
// an ADMIN's ⓘ — the server's host refusal, the "no computers" command and
// each computer's own reason. A member's render of the same options carries
// NONE of it: no reason, no command, no sheet lead, no code row; each of its
// sheets says to ask instead.
func TestWhereOfficersCanRunWithholdsEveryReasonFromAMember(t *testing.T) {
	var noHub, noHosts, hosts runtimeOptions
	noHub.Pod.Reason = "pods need the example agent plane"
	noHub.HostReason = "this muster has no example mirror hub"
	noHosts.Pod.Available = true
	hosts.Pod.Available = true
	hosts.Hosts = []hostOption{
		{Host: "atlas", Reason: "it starts no agent (example spawn is off)"},
		{Host: "nomad", Reason: "its muster-mirror predates officers with no repository (example)"},
		{Host: "vega", Qualifies: true},
	}
	for _, c := range []struct {
		name    string
		opts    runtimeOptions
		secret  []string // the withheld content, whole
		sheets  int
		visible string
	}{
		{"no hub", noHub, []string{noHub.Pod.Reason, noHub.HostReason, "What the server says:"}, 2, "not available on this server."},
		{"no computers", noHosts, []string{"muster-mirror -interactive -agent-spawn", "Run muster-mirror with these settings"}, 1, "No computers connected yet."},
		{"computers", hosts, []string{hosts.Hosts[0].Reason, hosts.Hosts[1].Reason, "What atlas reports:", "What nomad reports:"}, 2, "atlas</span> — can&#39;t start officers"},
	} {
		admin := runtimeAvailabilityHTML("rt", c.opts, true)
		member := runtimeAvailabilityHTML("rt", c.opts, false)
		for _, n := range c.secret {
			if !strings.Contains(admin, n) {
				t.Errorf("%s: CONTROL: the admin's panel has no %q", c.name, n)
			}
			if strings.Contains(member, n) {
				t.Errorf("%s: a member's panel carries %q", c.name, n)
			}
		}
		if strings.Contains(member, "data-info-code") {
			t.Errorf("%s: a member's panel has a code row", c.name)
		}
		if n := strings.Count(member, "data-info-withheld"); n != c.sheets {
			t.Errorf("%s: a member's panel has %d withheld sheet(s), want %d", c.name, n, c.sheets)
		}
		if !strings.Contains(member, c.visible) && !strings.Contains(member, strings.ReplaceAll(c.visible, "&#39;", "'")) {
			t.Errorf("%s: a member's panel does not say %q:\n%s", c.name, c.visible, member)
		}
	}
}

// TestAnOfficerSetInSettingsIsBadgedSo (audit round 1): an officer declared in
// the server's settings (MUSTER_OFFICERS) is read-only too, but it is not
// built in: its badge is "Set in settings".
func TestAnOfficerSetInSettingsIsBadgedSo(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	list := s.officerList(asRole(authz.RoleAdmin), officerScope{Owner: 1, Manage: true},
		[]declaredOfficerView{{ID: "growth:atlas", Owner: 1, ManagedBy: officers.ManagedByEnv, ReadOnly: true, Running: true}}, nil, nil)
	if !strings.Contains(list, `<span class="text-xs text-muted" data-read-only>Set in settings</span>`) || strings.Contains(list, ">Built-in<") {
		t.Errorf("the settings-declared officer's badge:\n%s", list)
	}
}

// TestAnOfficersItemCarriesItsIDForTheChief (audit round 1): an item an
// officer raised says its source in words for people and carries the id as
// officer_id for the chief's attention_list; any other item has none.
func TestAnOfficersItemCarriesItsIDForTheChief(t *testing.T) {
	a := attention.Ask{ID: 5, Owner: 1, Question: "post it?", Asker: attention.Asker{System: true, Label: officerAskLabel("product:acme")}}
	for name, it := range map[string]attentionItem{"ask": askItem(a), "notice": noticeItem(a, nil, nil)} {
		raw, err := json.Marshal(it)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"source":"Product officer (Acme)"`) || !strings.Contains(string(raw), `"officer_id":"product:acme"`) {
			t.Errorf("%s: %s", name, raw)
		}
	}
	other := attention.Ask{ID: 6, Owner: 1, Question: "q", Asker: attention.Asker{System: true, Label: "muster"}}
	if raw, _ := json.Marshal(askItem(other)); strings.Contains(string(raw), "officer_id") {
		t.Errorf("a non-officer item carries officer_id: %s", raw)
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
