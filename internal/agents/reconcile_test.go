package agents

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// readyAt builds a live instance in the shape the decision table cares about.
func instance(id string, ready bool, phase provision.Phase, restarts int32) *provision.Instance {
	return &provision.Instance{
		Ref:        provision.Ref{Name: "a"},
		Group:      "agent-a",
		InstanceID: id,
		Phase:      phase,
		Ready:      ready,
		Restarts:   restarts,
	}
}

func TestDecideReconcile(t *testing.T) {
	now := time.Date(2000, 6, 7, 12, 0, 0, 0, time.UTC)
	young := now.Add(-1 * time.Minute)                        // dwell < stuck timeout
	old := now.Add(-(ProvisioningStuckTimeout + time.Minute)) // dwell > stuck timeout
	readyInst := instance("a-1", true, provision.PhaseRunning, 0)
	pendingInst := instance("a-1", false, provision.PhasePending, 0)
	crashInst := &provision.Instance{
		Ref: provision.Ref{Name: "a"}, InstanceID: "a-1",
		Phase: provision.PhaseRunning, Reason: "CrashLoopBackOff",
	}

	cases := []struct {
		name       string
		agent      Agent
		inst       *provision.Instance
		wantAction ReconcileAction
		wantStatus string
	}{
		{
			name:       "error status is sticky",
			agent:      Agent{Status: StatusError, UpdatedAt: old},
			inst:       readyInst,
			wantAction: ActionNone,
		},
		{
			name:       "ready instance, not kicked off, young → retry kickoff",
			agent:      Agent{Status: StatusProvisioning, KickedOff: false, UpdatedAt: young, Namespace: "agent-a"},
			inst:       readyInst,
			wantAction: ActionRetryKickoff,
		},
		{
			name:       "ready instance, not kicked off, stuck → error",
			agent:      Agent{Status: StatusProvisioning, KickedOff: false, UpdatedAt: old, Namespace: "agent-a"},
			inst:       readyInst,
			wantAction: ActionError,
		},
		{
			name:       "ready instance, kicked off, status differs → persist running",
			agent:      Agent{Status: StatusProvisioning, KickedOff: true, UpdatedAt: young, Namespace: "agent-a"},
			inst:       readyInst,
			wantAction: ActionPersistStatus,
			wantStatus: StatusRunning,
		},
		{
			// A healthy kicked-off instance whose stored status is still
			// provisioning, even with an old dwell: the !a.KickedOff guard must
			// let this reach ComputeStatus → running, NOT error it.
			name:       "stored provisioning, kicked off, ready instance, old dwell → persist running (not errored)",
			agent:      Agent{Status: StatusProvisioning, KickedOff: true, UpdatedAt: old, Namespace: "agent-a"},
			inst:       readyInst,
			wantAction: ActionPersistStatus,
			wantStatus: StatusRunning,
		},
		{
			name:       "ready instance, kicked off, already running → none",
			agent:      Agent{Status: StatusRunning, KickedOff: true, UpdatedAt: young, Namespace: "agent-a"},
			inst:       readyInst,
			wantAction: ActionNone,
		},
		{
			name:       "provisioning, no instance, young → none (clock must keep running)",
			agent:      Agent{Status: StatusProvisioning, UpdatedAt: young},
			inst:       nil,
			wantAction: ActionNone,
		},
		{
			// The real production shape: a dispatched agent has its group set,
			// so a nil snapshot collapses to "stopped" via ComputeStatus. The
			// stuck guard must key on the stored provisioning status plus
			// dwell, not on the computed status.
			name:       "provisioning, no instance, group set, not kicked off, stuck → error",
			agent:      Agent{Status: StatusProvisioning, KickedOff: false, UpdatedAt: old, Namespace: "agent-stuck"},
			inst:       nil,
			wantAction: ActionError,
		},
		{
			name:       "provisioning, no instance, no group, stuck → error",
			agent:      Agent{Status: StatusProvisioning, KickedOff: false, UpdatedAt: old},
			inst:       nil,
			wantAction: ActionError,
		},
		{
			name:       "provisioning, pending instance, stuck → error",
			agent:      Agent{Status: StatusProvisioning, UpdatedAt: old, Namespace: "agent-a"},
			inst:       pendingInst,
			wantAction: ActionError,
		},
		{
			name:       "running agent, instance gone → persist stopped",
			agent:      Agent{Status: StatusRunning, KickedOff: true, UpdatedAt: young, Namespace: "agent-a"},
			inst:       nil,
			wantAction: ActionPersistStatus,
			wantStatus: StatusStopped,
		},
		{
			name:       "crashlooping instance → persist error",
			agent:      Agent{Status: StatusRunning, KickedOff: true, UpdatedAt: young, Namespace: "agent-a"},
			inst:       crashInst,
			wantAction: ActionPersistStatus,
			wantStatus: StatusError,
		},
		{
			// 🔴 NO UPSTREAM COUNTERPART: a driver reports a deliberately
			// scaled-to-zero instance as PRESENT, in PhaseStopped, rather than
			// as an absent one. Without ComputeStatus' stopped branch this
			// lands on ActionPersistStatus/provisioning — a stop reported as a
			// start that never finishes.
			name:       "scaled-to-zero instance → persist stopped",
			agent:      Agent{Status: StatusRunning, KickedOff: true, UpdatedAt: young, Namespace: "agent-a"},
			inst:       &provision.Instance{Ref: provision.Ref{Name: "a"}, Phase: provision.PhaseStopped, Reason: "scaled to zero"},
			wantAction: ActionPersistStatus,
			wantStatus: StatusStopped,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, status := DecideReconcile(tc.agent, tc.inst, now)
			if action != tc.wantAction {
				t.Errorf("action = %s, want %s", action, tc.wantAction)
			}
			if tc.wantAction == ActionPersistStatus && status != tc.wantStatus {
				t.Errorf("newStatus = %q, want %q", status, tc.wantStatus)
			}
		})
	}
}

// TestDecideReconcileKickoffLost drives the recovery half of the table: a
// kickoff that WAS delivered, to a recipient that has since gone.
func TestDecideReconcileKickoffLost(t *testing.T) {
	now := time.Date(2000, 8, 12, 12, 0, 0, 0, time.UTC)
	young := now.Add(-time.Minute)
	base := Agent{
		Status: StatusRunning, KickedOff: true, Namespace: "agent-a", UpdatedAt: young,
		KickoffPod: "a-1", KickoffRestarts: 0, KickoffAttempts: 1,
	}
	withAttempts := func(a Agent, n int) Agent { a.KickoffAttempts = n; return a }

	cases := []struct {
		name       string
		agent      Agent
		inst       *provision.Instance
		wantAction ReconcileAction
	}{
		{
			name:       "same instance, same restart count → left alone",
			agent:      base,
			inst:       instance("a-1", true, provision.PhaseRunning, 0),
			wantAction: ActionNone,
		},
		{
			name:       "same instance, restart count grew → resend",
			agent:      base,
			inst:       instance("a-1", true, provision.PhaseRunning, 1),
			wantAction: ActionResendKickoff,
		},
		{
			name:       "instance replaced (restart count back to 0) → resend",
			agent:      base,
			inst:       instance("a-2", true, provision.PhaseRunning, 0),
			wantAction: ActionResendKickoff,
		},
		{
			name:       "restarted but not ready yet → wait, do not resend into a dead runtime",
			agent:      base,
			inst:       instance("a-1", false, provision.PhaseRunning, 1),
			wantAction: ActionPersistStatus, // running → provisioning
		},
		{
			name:       "restart budget spent → visible failure",
			agent:      withAttempts(base, MaxKickoffAttempts),
			inst:       instance("a-1", true, provision.PhaseRunning, 3),
			wantAction: ActionErrorKickoffLost,
		},
		{
			name:       "one attempt below the budget → still resend",
			agent:      withAttempts(base, MaxKickoffAttempts-1),
			inst:       instance("a-1", true, provision.PhaseRunning, 3),
			wantAction: ActionResendKickoff,
		},
		{
			name: "legacy agent with no stamp → never resent",
			agent: func() Agent {
				a := base
				a.KickoffPod = ""
				return a
			}(),
			inst:       instance("a-9", true, provision.PhaseRunning, 5),
			wantAction: ActionNone,
		},
		{
			name: "never-kicked-off agent keeps the ORIGINAL retry path",
			agent: func() Agent {
				a := base
				a.KickedOff = false
				a.Status = StatusProvisioning
				a.KickoffPod = ""
				return a
			}(),
			inst:       instance("a-1", true, provision.PhaseRunning, 0),
			wantAction: ActionRetryKickoff,
		},
		{
			name: "errored agent stays sticky even with a lost kickoff",
			agent: func() Agent {
				a := base
				a.Status = StatusError
				return a
			}(),
			inst:       instance("a-2", true, provision.PhaseRunning, 0),
			wantAction: ActionNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := DecideReconcile(tc.agent, tc.inst, now)
			if got != tc.wantAction {
				t.Errorf("DecideReconcile action = %s, want %s", got, tc.wantAction)
			}
		})
	}
}

// TestDescribeKickoffLoss pins the operator-facing text, because the capacity
// finding it carries is the whole reason the field exists.
//
// 🔴 THE ASSERTIONS ARE ON THE WHOLE STRING, NOT ON A SUBSTRING. Upstream's only
// coverage of this function was a `strings.Contains(msg, "OOMKilled")` inside a
// reconcile-tick test — a guard a message reading merely "OOMKilled" would
// satisfy while saying nothing about WHICH of the two loss shapes occurred.
func TestDescribeKickoffLoss(t *testing.T) {
	a := Agent{KickoffPod: "a-1", KickoffRestarts: 2}
	cases := []struct {
		name string
		inst *provision.Instance
		want string
	}{
		{
			name: "no instance at all",
			inst: nil,
			want: "instance gone",
		},
		{
			// Same ID, higher restart count: an in-place restart. The two
			// counts must BOTH appear and must not be swapped, so they are
			// deliberately different values.
			name: "restarted in place",
			inst: &provision.Instance{InstanceID: "a-1", Restarts: 5},
			want: "instance restarted 2→5",
		},
		{
			name: "restarted in place, with a termination reason",
			inst: &provision.Instance{InstanceID: "a-1", Restarts: 5, RestartReason: "OOMKilled"},
			want: "instance restarted 2→5, last termination OOMKilled",
		},
		{
			// A different ID wins over the count: this is a replacement, not a
			// restart, and reporting a count movement here would be wrong even
			// though one also happened.
			name: "replaced by a differently-named instance",
			inst: &provision.Instance{InstanceID: "a-2", Restarts: 0},
			want: "instance replaced (a-1 → a-2)",
		},
		{
			name: "replaced, with a termination reason",
			inst: &provision.Instance{InstanceID: "a-2", Restarts: 0, RestartReason: "Evicted"},
			want: "instance replaced (a-1 → a-2), last termination Evicted",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DescribeKickoffLoss(a, c.inst); got != c.want {
				t.Errorf("DescribeKickoffLoss() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestKickoffErrorSuffix(t *testing.T) {
	if got := KickoffErrorSuffix(Agent{}); got != "" {
		t.Errorf("no recorded error should add nothing, got %q", got)
	}
	if got := KickoffErrorSuffix(Agent{KickoffError: "unexpected EOF"}); got != "; last send error: unexpected EOF" {
		t.Errorf("KickoffErrorSuffix = %q", got)
	}
}

// TestReconcileActionString pins the names. The table's actions are compared by
// VALUE everywhere, and iota means inserting one silently renumbers the rest —
// so the mapping from value to name is worth an assertion of its own.
func TestReconcileActionString(t *testing.T) {
	want := map[ReconcileAction]string{
		ActionNone:             "none",
		ActionPersistStatus:    "persist-status",
		ActionRetryKickoff:     "retry-kickoff",
		ActionResendKickoff:    "resend-kickoff",
		ActionError:            "error",
		ActionErrorKickoffLost: "error-kickoff-lost",
	}
	if len(want) != 6 {
		t.Fatalf("the ledger holds %d actions; if one was added, add its name here too", len(want))
	}
	for a, s := range want {
		if got := a.String(); got != s {
			t.Errorf("ReconcileAction(%d).String() = %q, want %q", int(a), got, s)
		}
	}
	if got := ReconcileAction(99).String(); got != "ReconcileAction(99)" {
		t.Errorf("unknown action String() = %q", got)
	}
}
