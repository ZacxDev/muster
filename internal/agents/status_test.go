package agents

import (
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

func TestComputeStatus(t *testing.T) {
	tests := []struct {
		name string
		a    Agent
		inst *provision.Instance
		want string
	}{
		// Stored status is authoritative for error, regardless of live state.
		{"stored error wins over a running instance", Agent{Status: StatusError}, &provision.Instance{Phase: provision.PhaseRunning, Ready: true}, StatusError},
		{"stored error, no instance", Agent{Status: StatusError}, nil, StatusError},

		// No live instance: provisioned-but-down cases resolve to stopped.
		{"no instance, explicit stopped", Agent{Status: StatusStopped}, nil, StatusStopped},
		{"no instance, kicked off", Agent{Status: StatusPending, KickedOff: true}, nil, StatusStopped},
		{"no instance, has group", Agent{Status: StatusRunning, Namespace: "agent-x"}, nil, StatusStopped},
		{"no instance, never provisioned pending", Agent{Status: StatusPending}, nil, StatusPending},
		{"no instance, never provisioned provisioning", Agent{Status: StatusProvisioning}, nil, StatusProvisioning},

		// Instance-level failure reasons override the phase.
		{"crashloop", Agent{Status: StatusRunning}, &provision.Instance{Phase: provision.PhaseRunning, Reason: "CrashLoopBackOff"}, StatusError},
		{"imagepullbackoff", Agent{Status: StatusRunning}, &provision.Instance{Phase: provision.PhasePending, Reason: "ImagePullBackOff"}, StatusError},
		{"errimagepull", Agent{Status: StatusRunning}, &provision.Instance{Phase: provision.PhasePending, Reason: "ErrImagePull"}, StatusError},
		{
			// The reason branch must not swallow every reason: a driver that
			// annotates a healthy instance ("scaled to zero", "no pod yet")
			// must not thereby turn it red.
			name: "an unrecognised reason does NOT force error",
			a:    Agent{Status: StatusProvisioning},
			inst: &provision.Instance{Phase: provision.PhaseRunning, Ready: true, Reason: "some driver note"},
			want: StatusRunning,
		},

		// Phase-driven refinement.
		{"running+ready", Agent{Status: StatusProvisioning}, &provision.Instance{Phase: provision.PhaseRunning, Ready: true}, StatusRunning},
		{"running not ready", Agent{Status: StatusProvisioning}, &provision.Instance{Phase: provision.PhaseRunning, Ready: false}, StatusProvisioning},
		{"pending phase", Agent{Status: StatusRunning}, &provision.Instance{Phase: provision.PhasePending}, StatusProvisioning},
		{"failed phase", Agent{Status: StatusRunning}, &provision.Instance{Phase: provision.PhaseFailed}, StatusError},
		{
			// 🔴 THE BRANCH WITH NO UPSTREAM COUNTERPART. A driver reports a
			// deliberately scaled-to-zero instance as PRESENT and stopped, so
			// this must NOT fall through to the default — that would report a
			// stop as a permanently-slow start. The stored status is
			// `running` here on purpose: nothing but the live phase can
			// produce the right answer.
			name: "stopped phase is reported as stopped, not provisioning",
			a:    Agent{Status: StatusRunning, Namespace: "agent-x"},
			inst: &provision.Instance{Phase: provision.PhaseStopped, Reason: "scaled to zero"},
			want: StatusStopped,
		},
		{"unknown phase falls to provisioning", Agent{Status: StatusRunning}, &provision.Instance{Phase: provision.PhaseUnknown}, StatusProvisioning},
		{"succeeded phase falls to provisioning", Agent{Status: StatusRunning}, &provision.Instance{Phase: provision.PhaseSucceeded}, StatusProvisioning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ComputeStatus(tt.a, tt.inst); got != tt.want {
				t.Errorf("ComputeStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInstanceIndex(t *testing.T) {
	// 🔴 THE GROUPS ARE DELIBERATELY IDENTICAL AND THE NAMES DELIBERATELY
	// DISTINCT. Upstream keyed this index on the group (the namespace). A
	// fixture giving each instance its own group cannot see that regression:
	// both keyings produce two entries and both look correct. Sharing one group
	// makes a group-keyed index collapse to a single entry, which the length
	// assertion below then catches.
	insts := []provision.Instance{
		{Ref: provision.Ref{Name: "alpha"}, Group: "agents", InstanceID: "alpha-1"},
		{Ref: provision.Ref{Name: "beta"}, Group: "agents", InstanceID: "beta-1"},
	}
	idx := InstanceIndex(insts)
	if len(idx) != 2 {
		t.Fatalf("InstanceIndex len = %d, want 2 (a group-keyed index collapses these to 1)", len(idx))
	}
	if idx["alpha"] == nil || idx["alpha"].InstanceID != "alpha-1" {
		t.Errorf("index[alpha] = %+v, want the instance alpha-1", idx["alpha"])
	}
	if idx["beta"] == nil || idx["beta"].InstanceID != "beta-1" {
		t.Errorf("index[beta] = %+v, want the instance beta-1", idx["beta"])
	}
	// Each entry must point at a DISTINCT backing element.
	//
	// ⚠ THIS IS AN INVARIANT GUARD, NOT A REGRESSION GUARD, AND THE DIFFERENCE
	// IS WORTH STATING. It descends from a real Go < 1.22 hazard — one loop
	// variable reused across iterations, so `&v` gave every key the same
	// pointer. This module is go 1.25, where the loop variable is per-iteration
	// and that bug is not expressible; a mutation sweep confirmed the
	// once-dangerous shape now behaves identically. What the assertion still
	// catches is a body that indexes a FIXED element (insts[0]), which is why
	// it stays.
	if idx["alpha"] == idx["beta"] {
		t.Error("InstanceIndex entries alias the same Instance")
	}
	if _, ok := idx["missing"]; ok {
		t.Error("InstanceIndex returned an entry for a name not present")
	}
}
