package main

import (
	"strings"
	"testing"
)

// The round-2 cases for MUSTER_AGENT_CREATE_TOKEN beside the tenant pair rule
// (tenant_leak_test.go holds round 1's leak matrix and its fixtures).

// TestAnExportedAgentCreateTokenRefusesOnlyAgentCreate (round 2, F1): on a
// host with more than one tenant, an exported agent-create token with no
// environment pair makes the token unusable, not the CLI. whoami still works
// with the selected tenant's own pair and sends no agent-create token
// anywhere; agent create refuses with exit 2, the pair rule's text and the
// tenant-aware advice, and sends nothing.
func TestAnExportedAgentCreateTokenRefusesOnlyAgentCreate(t *testing.T) {
	f := newLeakFixture(t)
	serveCreate(f)
	f.addWork()
	env := map[string]string{"MUSTER_AGENT_CREATE_TOKEN": defaultCreateTok}
	got := f.run(env, "--tenant", "work", "whoami")
	if got.code != exitOK {
		t.Fatalf("whoami exit = %d, stderr=%q; an exported agent-create token must not stop other verbs", got.code, got.stderr)
	}
	if a := authsTo(f.work); len(a) != 1 || a[0] != "Bearer "+workTok {
		t.Errorf("work received %v, want its own MUSTER_TOKEN once", a)
	}
	for _, h := range []*harness{f.dflt, f.work, f.alien} {
		for _, r := range h.requests() {
			if strings.Contains(r.auth, defaultCreateTok) {
				t.Errorf("🔴 %s %s carried the agent-create token", r.method, r.uri)
			}
		}
	}
	before := f.total()
	got = f.run(env, append([]string{"--tenant", "work"}, agentCreateArgs...)...)
	if got.code != exitUsage || !strings.Contains(got.stderr, pairRefusalText) ||
		!strings.Contains(got.stderr, "put MUSTER_AGENT_CREATE_TOKEN in that tenant's file") {
		t.Fatalf("agent create: exit %d stderr=%q; want 2 with the pair refusal and the tenant advice", got.code, got.stderr)
	}
	if n := f.total() - before; n != 0 {
		t.Errorf("🔴 agent create sent %d request(s)", n)
	}
}

// TestTheNoTokenAdviceIsTenantAware: with no agent-create token at all, a
// multi-tenant host's refusal says where to put one; a one-tenant host's does
// not (exporting it is enough there).
func TestTheNoTokenAdviceIsTenantAware(t *testing.T) {
	f := newLeakFixture(t)
	got := f.run(map[string]string{}, agentCreateArgs...)
	if got.code != exitUsage || !strings.Contains(got.stderr, "needs a token of scope agent-create") ||
		strings.Contains(got.stderr, "more than one tenant") {
		t.Fatalf("one tenant: exit %d stderr=%q; want 2 without tenant advice", got.code, got.stderr)
	}
	f.addWork()
	got = f.run(map[string]string{}, append([]string{"--tenant", "work"}, agentCreateArgs...)...)
	if got.code != exitUsage || !strings.Contains(got.stderr, "On a host with more than one tenant, put MUSTER_AGENT_CREATE_TOKEN in that tenant's file") {
		t.Fatalf("two tenants: exit %d stderr=%q; want 2 with tenant advice", got.code, got.stderr)
	}
	if n := f.total(); n != 0 {
		t.Errorf("%d request(s) sent", n)
	}
}

// TestOneTenantAgentCreateTokenPrecedence (round 2, G1): on the one-tenant
// path the file's agent-create token is used when nothing is exported, and an
// exported one beats it — the token's own order (file, then environment).
// The two tokens differ, so either half deleted, or the order swapped, moves
// the one Authorization header default receives.
func TestOneTenantAgentCreateTokenPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"the file's token", map[string]string{}, defaultCreateTok},
		{"the environment beats the file", map[string]string{"MUSTER_AGENT_CREATE_TOKEN": foreignCreateTok}, foreignCreateTok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLeakFixture(t)
			serveCreate(f)
			f.write(".muster/muster.env", "MUSTER_API_URL="+f.dflt.srv.URL+"\nMUSTER_TOKEN="+defaultTok+
				"\nMUSTER_AGENT_CREATE_TOKEN="+defaultCreateTok+"\n", 0o600)
			got := f.run(tc.env, agentCreateArgs...)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			if a := createAuths(f.dflt); len(a) != 1 || a[0] != "Bearer "+tc.want {
				t.Errorf("default received %v, want %q once", a, tc.want)
			}
		})
	}
}
