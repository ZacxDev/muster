package agents

import "github.com/ZacxDev/muster/internal/provision"

// ComputeStatus reconciles the STORED agent status with the LIVE instance state
// to produce the status shown on the card. The stored status is authoritative
// for error and for the "provisioned but never started" (stopped) case; the live
// phase refines running/provisioning.
//
// 🔴 IT TAKES A provision.Instance, NOT A POD. Upstream this read a `PodInfo`
// built from the Kubernetes API, and switched on that API's own phase spellings
// ("Running", "Pending", "Failed") as bare strings. Both are now the
// provisioner contract's vocabulary, which is what lets this function answer for
// an agent running under any driver — and the [provision.Phase] constants are
// typed, so a misspelling is a compile error rather than a branch that silently
// falls through to the default.
//
// ⚠ inst.Reason IS STILL A DRIVER-DEFINED STRING and the three values matched
// below are the Kubernetes driver's spellings. That is deliberate rather than an
// oversight: the contract does not enumerate reasons, and a driver that reports
// none simply never takes this branch — it falls through to the phase, which
// every driver does set. See [provision.Instance].Reason.
func ComputeStatus(a Agent, inst *provision.Instance) string {
	if a.Status == StatusError {
		return StatusError
	}
	if inst == nil {
		// No live instance. If we provisioned it (it has a group), it is
		// stopped; otherwise it is still pending.
		if a.Status == StatusStopped || a.KickedOff || a.Namespace != "" {
			return StatusStopped
		}
		return a.Status
	}
	if inst.Reason == "CrashLoopBackOff" || inst.Reason == "ImagePullBackOff" || inst.Reason == "ErrImagePull" {
		return StatusError
	}
	switch inst.Phase {
	case provision.PhaseRunning:
		if inst.Ready {
			return StatusRunning
		}
		return StatusProvisioning
	case provision.PhasePending:
		return StatusProvisioning
	case provision.PhaseFailed:
		return StatusError
	case provision.PhaseStopped:
		// 🔴 THIS CASE HAS NO UPSTREAM COUNTERPART AND IS NOT COSMETIC. No
		// Kubernetes pod phase means "deliberately scaled to zero" — upstream a
		// stopped agent simply had no pod, so it reached the inst == nil branch
		// above and answered `stopped`. The provisioner contract reports a
		// scaled-to-zero instance as a PRESENT instance in PhaseStopped
		// ([provision.Instance] is synthesised from the deployment, not from a
		// pod), so without this branch a stopped agent would fall to the
		// default and read `provisioning` forever — a stop permanently
		// misreported as a slow start.
		return StatusStopped
	default:
		// PhaseSucceeded and PhaseUnknown. Upstream's `default` answered
		// provisioning for Kubernetes' "Succeeded" too; the behaviour is
		// preserved rather than improved here, because deciding what a
		// completed agent should read is a product question and this carve is
		// not the place to answer it.
		return StatusProvisioning
	}
}

// InstanceIndex maps an agent NAME to its live instance, for quick
// reconciliation against a stored agent list.
//
// 🔴 THE KEY IS Ref.Name, NOT THE GROUP. Upstream keyed this on the pod's
// namespace, because every agent had one and it matched Agent.Namespace. Group
// is the driver's own scoping unit and [provision.Instance] states it is EMPTY
// where the concept does not apply — so a group-keyed index collapses every
// instance of a non-namespaced driver onto the single key "", and the last one
// wins. Ref.Name is the contract's identity and is always set.
func InstanceIndex(insts []provision.Instance) map[string]*provision.Instance {
	idx := make(map[string]*provision.Instance, len(insts))
	for i := range insts {
		idx[insts[i].Ref.Name] = &insts[i]
	}
	return idx
}
