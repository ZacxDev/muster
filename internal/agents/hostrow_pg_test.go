package agents

import (
	"strings"
	"testing"
)

// TestAHostRowIsNeverReapable: a host officer instance's row (RunsOnHost,
// migration 0125) is excluded by the stopped-agent reaper's predicate ITSELF —
// even forced through `provisioning` → `running` → `stopped` (so
// first_started_at is set, the predicate's other gate) and aged past the
// window, it is neither listed nor claimable. The control: an otherwise
// identical row without RunsOnHost is listed and claimed.
func TestAHostRowIsNeverReapable(t *testing.T) {
	f := newReapFixture(t)
	control := f.agent(t, "reap-control", StatusStopped, "")
	host, err := f.store.Create(f.ctx, Agent{Name: "reap-host-" + f.stamp, Status: StatusStopped, RunsOnHost: "host-alpha"})
	if err != nil {
		t.Fatalf("create a host row: %v", err)
	}
	t.Cleanup(func() { cleanupAgent(f.ctx, f.store.pool, host.ID) })
	if host.RunsOnHost != "host-alpha" || host.Provisioned() {
		t.Fatalf("the host row reads back as %+v", host)
	}
	for _, st := range []string{StatusProvisioning, StatusRunning, StatusStopped} {
		if err := f.store.UpdateStatus(f.ctx, host.ID, st, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	f.age(t, host.ID, 15)
	f.age(t, control.ID, 15)
	got := f.listed(t, host.ID, control.ID)
	if _, ok := got[host.ID]; ok {
		t.Fatal("a host row is listed as reapable")
	}
	if _, ok := got[control.ID]; !ok {
		t.Fatalf("control: the provisioned row is not listed (%v)", got)
	}
	if f.claim(t, host.ID) {
		t.Fatal("a host row was claimed for reaping")
	}
}

// TestAHostRowHasNoClusterShape: agents_runs_on_host_check refuses a host row
// with a namespace, a Claude account or a worker pool — the shapes a
// provisioner acts on — and a malformed host name.
func TestAHostRowHasNoClusterShape(t *testing.T) {
	f := newReapFixture(t)
	for name, a := range map[string]Agent{
		"namespace": {Namespace: "ns-x"},
		"account":   {Kind: KindClaudeCode, CCAccount: "acct"},
		"pool":      {Kind: KindClaudeCode, CCAccount: "acct", WorkerPool: "reviewers"},
		"bad host":  {RunsOnHost: "Host_Alpha"},
	} {
		a.Name = "hostshape-" + strings.ReplaceAll(name, " ", "-") + "-" + f.stamp
		a.Status = StatusStopped
		if a.RunsOnHost == "" {
			a.RunsOnHost = "host-alpha"
		}
		got, err := f.store.Create(f.ctx, a)
		if err == nil {
			cleanupAgent(f.ctx, f.store.pool, got.ID)
			t.Errorf("%s: a host row was inserted", name)
		} else if !strings.Contains(err.Error(), "agents_runs_on_host_check") {
			t.Errorf("%s: refused by %v, want agents_runs_on_host_check", name, err)
		}
	}
}
