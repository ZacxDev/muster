package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/jobs"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// A POOL MEMBER STORED AS running WHOSE INSTANCE HAS NO READY POD IS NOT CHOSEN
// (jobs.go poolMembersNotReady), AND agents_list SAYS WHAT ITS INSTANCE SAYS.
// ---------------------------------------------------------------------------

// liveInstances is a provisioner whose instances are computed per read by
// state (agent name → its instance; absent: no instance at all). fail, when
// set, fails every read.
type liveInstances struct {
	stubProvisioner
	mu    sync.Mutex
	state func(name string) (provision.Instance, bool)
	fail  error
	reads int
}

func (p *liveInstances) read(names []string) ([]provision.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if p.fail != nil {
		return nil, p.fail
	}
	var out []provision.Instance
	for _, n := range names {
		if in, ok := p.state(n); ok {
			in.Ref.Name = n
			out = append(out, in)
		}
	}
	return out, nil
}

func (p *liveInstances) Instances(context.Context) ([]provision.Instance, error) {
	return p.read([]string{"rev-a", "rev-b", "keen-fox", "mellow-claw", "mine-w1"})
}

func (p *liveInstances) InstancesOf(_ context.Context, ags []agents.Agent) ([]provision.Instance, error) {
	names := make([]string, 0, len(ags))
	for _, a := range ags {
		names = append(names, a.Name)
	}
	return p.read(names)
}

func (p *liveInstances) readCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

var (
	readyInstance        = provision.Instance{Phase: provision.PhaseRunning, Ready: true}
	scaledToZeroInstance = provision.Instance{Phase: provision.PhaseStopped, Reason: "scaled to zero"}
	notReadyInstance     = provision.Instance{Phase: provision.PhaseRunning, Ready: false}
)

// fixed is a liveInstances state from a map.
func fixed(m map[string]provision.Instance) func(string) (provision.Instance, bool) {
	return func(n string) (provision.Instance, bool) { in, ok := m[n]; return in, ok }
}

// TestAPoolJobSkipsAMemberWhoseDeploymentHasNoPod is the outage: rev-a — first
// by name, so it wins every other rule — is stored running but its deployment
// is scaled to zero. The job goes to rev-b, whose pod is ready, instead of
// being placed on rev-a and failing at exec ("has no running pod to exec
// into"). The live read is of the pool's members.
func TestAPoolJobSkipsAMemberWhoseDeploymentHasNoPod(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.ext.Kinds = &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}}
	prov := &liveInstances{state: fixed(map[string]provision.Instance{"rev-a": scaledToZeroInstance, "rev-b": readyInstance})}
	f.s.ext.Provisioner = prov
	f.gw.reply = "done"
	out := f.await(t, f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "review"}))
	if out["status"] != jobs.StatusSucceeded || out["agent"] != "rev-b" {
		t.Fatalf("job = %v on %v (%v), want succeeded on rev-b: rev-a has no pod", out["status"], out["agent"], out["error"])
	}
	if prov.readCount() == 0 {
		t.Fatal("the members' instances were never read")
	}
}

// TestAPoolWithNoReadyMemberIsA503: rev-a scaled to zero, rev-b's pod not
// ready — every member stored running — is refused with 503 (what a caller
// reads as "no worker can take it": retry, an outage, not a failed job),
// naming each member's live status, and records nothing.
func TestAPoolWithNoReadyMemberIsA503(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.ext.Kinds = &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}}
	f.s.ext.Provisioner = &liveInstances{state: fixed(map[string]provision.Instance{"rev-a": scaledToZeroInstance, "rev-b": notReadyInstance})}
	code, out := f.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": "reviewers", "prompt": "x"}, true)
	const want = `no worker in pool "reviewers" can take the job: rev-a (no ready pod: its instance reads stopped); ` +
		`rev-b (no ready pod: its instance reads provisioning)`
	if code != http.StatusServiceUnavailable || out["error"] != want {
		t.Fatalf("no ready member = %d %v, want 503 %q", code, out, want)
	}
	if n := f.store.count(); n != 0 {
		t.Fatalf("%d jobs recorded, want 0", n)
	}
	// A member with no instance at all (its deployment gone) is not ready either.
	f.s.ext.Provisioner = &liveInstances{state: fixed(map[string]provision.Instance{"rev-a": scaledToZeroInstance})}
	code, out = f.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": "reviewers", "prompt": "x"}, true)
	const wantGone = `no worker in pool "reviewers" can take the job: rev-a (no ready pod: its instance reads stopped); ` +
		`rev-b (no ready pod: it has no instance)`
	if code != http.StatusServiceUnavailable || out["error"] != wantGone {
		t.Fatalf("rev-b with no instance = %d %v, want 503 %q", code, out, wantGone)
	}
}

// TestAPoolMemberIsChosenOnItsStoredStatusWhenTheLiveReadFails is an
// INVARIANT GUARD (the base behaviour, kept): a failed instance read marks
// nobody, so the stored status decides and rev-a, first by name, is chosen —
// a provisioner blip does not refuse the whole pool.
func TestAPoolMemberIsChosenOnItsStoredStatusWhenTheLiveReadFails(t *testing.T) {
	f := newJobFixture(t, testServiceToken)
	f.s.ext.Kinds = &poolKinds{states: map[string]ccpool.AccountState{"alpha": {}, "beta": {}}}
	f.s.ext.Provisioner = &liveInstances{state: fixed(nil), fail: errors.New("apiserver unreachable")}
	f.gw.reply = "done"
	out := f.await(t, f.submit(t, map[string]any{"worker_pool": "reviewers", "prompt": "review"}))
	if out["status"] != jobs.StatusSucceeded || out["agent"] != "rev-a" {
		t.Fatalf("job = %v on %v (%v), want succeeded on rev-a (stored status)", out["status"], out["agent"], out["error"])
	}
}

// TestAWorkerFailedOverForTheJobIsTakenWhileItsPodRestarts: both workers sit
// on the rate-limited devin with ready pods, so the submission fails keen-fox
// over to sara — which restarts its pod, so its instance now reads not ready.
// The job must still go to keen-fox (runJob waits for the replacement pod), not
// be refused because the move it was made for is in flight.
func TestAWorkerFailedOverForTheJobIsTakenWhileItsPodRestarts(t *testing.T) {
	r := newIncidentRig(t)
	r.markLimited(t, "devin")
	r.s.ext.Provisioner = &liveInstances{state: func(n string) (provision.Instance, bool) {
		if n == "keen-fox" {
			if _, moving := r.svc.Rebinding(keenFoxID); moving {
				return notReadyInstance, true
			}
		}
		return readyInstance, true
	}}
	code, out := r.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": incidentPool, "prompt": "review"}, true)
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/jobs to the pool = %d %v, want 202 on the worker failed over for it", code, out)
	}
	done := r.await(t, int64(out["job_id"].(float64)))
	if done["agent"] != "keen-fox" || done["cc_account"] != "sara" {
		t.Fatalf("job ran on %v/%v (%v), want keen-fox/sara", done["agent"], done["cc_account"], done["error"])
	}
}

// TestAgentsListReportsTheLiveStatus: an agent stored running whose deployment
// is scaled to zero reads stopped in agents_list, and one whose pod is not
// ready reads provisioning; with the read failed, the stored status.
func TestAgentsListReportsTheLiveStatus(t *testing.T) {
	const owner = int64(7)
	mine := agents.Agent{ID: 101, Name: "mine-w1", OwnerID: owner, Kind: agents.KindClaudeCode, WorkerPool: "shared-pool",
		Status: agents.StatusRunning}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: leakyAgents{rows: []agents.Agent{mine}}})
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaMirrorHost})
	c := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: "sess-1"}}
	for _, tc := range []struct {
		name string
		prov *liveInstances
		want string
	}{
		{"scaled to zero", &liveInstances{state: fixed(map[string]provision.Instance{"mine-w1": scaledToZeroInstance})}, agents.StatusStopped},
		{"pod not ready", &liveInstances{state: fixed(map[string]provision.Instance{"mine-w1": notReadyInstance})}, agents.StatusProvisioning},
		{"ready", &liveInstances{state: fixed(map[string]provision.Instance{"mine-w1": readyInstance})}, agents.StatusRunning},
		{"read failed", &liveInstances{state: fixed(nil), fail: errors.New("apiserver unreachable")}, agents.StatusRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.ext.Provisioner = tc.prov
			text := s.toolAgentsList(ctx, c, `{"worker_pool":"shared-pool"}`)
			var out agentsListOut
			if err := json.Unmarshal([]byte(text), &out); err != nil || len(out.Agents) != 1 {
				t.Fatalf("agents_list answered %s", text)
			}
			if got := out.Agents[0].Status; got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}
