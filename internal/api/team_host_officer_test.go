package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/users"
)

// Team sharing 2b.5 (team officers on team hosts), over the host-spawn fakes.

const (
	// thoTeam is a team; thoPerson a member of it whose PERSONAL host has the
	// team host's name. Distinct from every id the code or the other fixtures
	// name.
	thoTeam   int64 = 4417
	thoPerson int64 = 4423
)

// teamHostOfficerRig is the server with ONE host officer, owned by the TEAM
// thoTeam, on hostOffHost; the hub lists hostOffHost (online, spawnable,
// closing windows) as hostsOwner's — the team's own host, or the member's
// personal host of the same name — declaring connectors, and EnableOfficers'
// error.
func teamHostOfficerRig(t *testing.T, hostsOwner int64, connectors ...string) (*hostOfficerRig, error) {
	t.Helper()
	hq := hqtest.New(map[string]string{offBTok: offB})
	t.Cleanup(hq.Close)
	logs := &bytes.Buffer{}
	r := &hostOfficerRig{m: newHostOfficerMirrorFor(hostsOwner), hq: hq, spawns: officers.NewMemHostSpawnStore(), logs: logs, clock: time.Now()}
	r.spawns.Now = r.now
	hq.Now = r.now
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(logs, "", 0))
	r.store = &officerAgents{}
	s.UseExtensions(Extensions{Mirror: r.m, SessionLiveness: stubLiveness{}, Agents: r.store, Users: tsUsers{
		thoTeam:   users.Account{Role: authz.RoleMember, Team: true},
		thoPerson: users.Account{Role: authz.RoleMember},
	}})
	r.host = hostOfficer(hq.BaseURL())
	r.host.Owner = thoTeam
	r.host.Connectors = connectors
	rec, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{r.host}, Now: r.now, HostSpawns: r.spawns})
	r.s, r.rec = s, rec
	return r, err
}

// newTeamHostOfficerRig is teamHostOfficerRig with no connectors, which boot
// must admit.
func newTeamHostOfficerRig(t *testing.T, hostsOwner int64) *hostOfficerRig {
	t.Helper()
	r, err := teamHostOfficerRig(t, hostsOwner)
	if err != nil {
		t.Fatalf("a team's host officer at boot: %v; want it admitted (team sharing 2b.5)", err)
	}
	return r
}

// TestBootRefusesATeamHostOfficerWithConnectors (G3, at boot): EnableOfficers
// over MUSTER_OFFICERS refuses a team's host officer that declares
// connectors, with ErrTeamHostOfficerConnectors, and starts no runtime.
// CONTROL: the same officer without connectors boots (newTeamHostOfficerRig).
func TestBootRefusesATeamHostOfficerWithConnectors(t *testing.T) {
	r, err := teamHostOfficerRig(t, thoTeam, "warehouse")
	if !errors.Is(err, officers.ErrTeamHostOfficerConnectors) || r.rec != nil {
		t.Fatalf("boot with a team host officer declaring connectors: %v (runtime %v); want ErrTeamHostOfficerConnectors and none", err, r.rec != nil)
	}
	if c := newTeamHostOfficerRig(t, thoTeam); c.rec == nil {
		t.Fatal("CONTROL: the same officer without connectors did not boot")
	}
}

// TestATeamHostOfficerNeverSpawnsOnAPersonsHost: a TEAM's host officer on
// host-bravo, when the only host-bravo online is a MEMBER's personal host, is
// refused as its host not connected, and nothing is sent to that host — no
// spawn, no row. 🔴 An INVARIANT of the hub key (the runtime acts as the team,
// and the hub keys a host by (owner, name)), stated as one: the base never
// reached it, because it refused the officer at boot. CONTROL: with
// host-bravo the team's own host, the same pass spawns there, as the team (the
// instance's agent row is the team's).
func TestATeamHostOfficerNeverSpawnsOnAPersonsHost(t *testing.T) {
	ctx := context.Background()
	r := newTeamHostOfficerRig(t, thoPerson)
	r.hq.AddItem(offB, "triage.look", "svc:team-host-1")
	if res := r.rec.Check(ctx, offB, true); res.Err == nil || !strings.Contains(res.Err.Error(), "is not connected") {
		t.Fatalf("a team host officer with only a member's same-named host online: %+v; want the host-not-connected error", res)
	}
	if n, _, _ := r.m.counts(); n != 0 || len(r.spawns.All()) != 0 {
		t.Fatalf("%d spawn(s) sent to the member's personal host, %d row(s); want none", n, len(r.spawns.All()))
	}

	// CONTROL: the team's own host.
	c := newTeamHostOfficerRig(t, thoTeam)
	c.hq.AddItem(offB, "triage.look", "svc:team-host-2")
	if res := c.rec.Check(ctx, offB, true); res.Err != nil || len(res.Decision.Start) != 1 {
		t.Fatalf("CONTROL: a team host officer on the team's own host: %+v; want Start [1]", res)
	}
	h := c.waitSpawned(t)
	c.m.mu.Lock()
	call := c.m.spawns[0]
	c.m.mu.Unlock()
	if call.host != hostOffHost || h.Instance != officers.InstanceName(offB, 1) {
		t.Fatalf("CONTROL: the spawn went to %q as %+v; want %s, instance 1", call.host, h, hostOffHost)
	}
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	if len(c.store.created) != 1 || c.store.created[0].OwnerID != thoTeam || c.store.created[0].RunsOnHost != hostOffHost {
		t.Fatalf("CONTROL: the instance's agent row %+v; want one, owned by the team, on %s", c.store.created, hostOffHost)
	}
}
