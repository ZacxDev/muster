package provision

import (
	"fmt"
	"strings"
)

// Isolation is how well a driver separates an instance from the machine it runs
// on. It is ordered, so a caller can compare.
type Isolation int

const (
	// IsolationNone — the instance runs as the muster process's own user, in its
	// filesystem, with its credentials.
	//
	// 🔴 A DRIVER THAT REPORTS THIS IS A DEVELOPMENT DRIVER AND MUST BE
	// PRESENTED AS ONE. A prompt-injected agent under IsolationNone is a
	// prompt-injected agent with your ssh keys. Any interface that shows
	// instances should say so, on every instance, whenever their driver reports
	// it — not once in a settings page.
	IsolationNone Isolation = iota
	// IsolationProcess — a separate process with some kernel-level confinement,
	// but a shared filesystem and user.
	IsolationProcess
	// IsolationContainer — its own filesystem, process table and user, sharing
	// the host kernel and a flat namespace of container names.
	IsolationContainer
	// IsolationNamespace — a container plus a scoping unit the driver owns
	// entirely, so a teardown can delete the unit rather than enumerate objects.
	IsolationNamespace
)

// String renders the level for logs and for the interface text that has to warn
// about IsolationNone.
func (i Isolation) String() string {
	switch i {
	case IsolationNone:
		return "none"
	case IsolationProcess:
		return "process"
	case IsolationContainer:
		return "container"
	case IsolationNamespace:
		return "namespace"
	default:
		return fmt.Sprintf("isolation(%d)", int(i))
	}
}

// Capabilities is what a driver can actually do.
//
// 🔴 A CAPABILITY NOBODY BRANCHES ON IS NOT A GUARD, IT IS A FIELD. This type
// is only worth its weight because [CheckSpec] and [Grant] refuse on it, and
// because those two are the ONLY places the refusal is spelled — a predicate
// open-coded per driver is a predicate that is wrong in all but one of them, in
// the same direction, and nobody hears the disagreement.
//
// Every field is false-by-default and false means "cannot", so a driver that
// forgets to declare something is treated as unable to do it. That is the safe
// direction: the failure is a refusal a developer sees immediately, not a
// silent downgrade a user discovers later.
type Capabilities struct {
	// Isolation is how well instances are separated. See the constants.
	Isolation Isolation

	// Secrets: the driver has a confidential store distinct from plain
	// environment. False means Spec.Secrets and secret Files are refused rather
	// than written somewhere readable.
	//
	// ⚠ A container daemon is the motivating false case. Its secret support is
	// an orchestrator feature, so a plain-daemon driver has only environment
	// variables and bind mounts, and an environment variable is visible to
	// anything that can read the process table.
	Secrets bool

	// Files: the driver can place Spec.Files natively.
	Files bool

	// Policy: the driver can attach an identity-scoped authorisation policy to
	// an instance — cluster RBAC and its equivalents. See Grant for why a false
	// here must produce a REFUSAL rather than a no-op.
	Policy bool

	// FQDNEgress: the driver can restrict outbound traffic by DNS NAME, not
	// merely by address range.
	//
	// 🔴 THE DISTINCTION IS THE ENTIRE THREAT MODEL, NOT A REFINEMENT. The
	// reason to restrict an agent's egress is exfiltration by a prompt-injected
	// model, and an address-range allowlist does not constrain that: anything
	// reachable is reachable. A driver with Policy true and FQDNEgress false has
	// authorisation but no exfiltration control, and the difference has to be
	// visible to whoever decides what a given agent is allowed to read.
	FQDNEgress bool

	// Persistence: storage that outlives the instance's process.
	Persistence bool

	// Sidecars: more than one process per instance.
	Sidecars bool

	// ResourceLimits: enforceable cpu/memory ceilings.
	ResourceLimits bool

	// Scale: replica counts other than 0 and 1.
	Scale bool

	// Exec: the driver implements Execer against a live instance.
	Exec bool
}

// CheckSpec is THE capability branch. It is the answer to "Capabilities is
// useless unless callers branch on it", and it exists exactly once so there is
// exactly one place to be wrong.
//
// Every driver calls it at the top of Create and Update, before it touches a
// backend. It returns an error wrapping [ErrUnsupported] naming the capability
// and what the spec asked for, so the refusal is legible to whoever wrote the
// spec rather than being a generic rejection.
//
// 🔴 THE REFUSALS ARE NOT PEDANTRY. Each one is a case where honouring part of
// a spec and dropping the rest produces something that LOOKS like it worked:
// secrets written into plain environment, a resource ceiling that is not
// enforced, a workspace that is a tmpfs. Silently degrading any of those is the
// shape the original project's noop driver had, where every method returned nil
// and the interface reported success for work nobody did.
func CheckSpec(caps Capabilities, s Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !caps.Files && len(s.Files) > 0 {
		return fmt.Errorf("%w: driver cannot place files, and the spec carries %d (%s); placing them through Init would mean shell-quoting content this driver has no way to verify",
			ErrUnsupported, len(s.Files), firstPaths(s.Files))
	}
	if !caps.Secrets {
		if len(s.Secrets) > 0 {
			return fmt.Errorf("%w: driver has no confidential store, and the spec carries %d secret(s) (%s); move them to Env only if you accept that they are readable wherever this driver puts environment",
				ErrUnsupported, len(s.Secrets), strings.Join(envNames(s.Secrets), ", "))
		}
		for _, f := range s.Files {
			if f.Secret {
				return fmt.Errorf("%w: driver has no confidential store, and file %q is marked Secret", ErrUnsupported, f.Path)
			}
		}
	}
	if !caps.Persistence && s.Workspace.Persist {
		return fmt.Errorf("%w: driver has no persistent storage, and the spec asks for a persistent workspace at %q", ErrUnsupported, s.Workspace.Path)
	}
	if !caps.ResourceLimits && !s.Resources.IsZero() {
		return fmt.Errorf("%w: driver cannot enforce resource limits, and the spec sets them (cpu %q/%q, memory %q/%q)",
			ErrUnsupported, s.Resources.CPURequest, s.Resources.CPULimit, s.Resources.MemoryRequest, s.Resources.MemoryLimit)
	}
	if !caps.Scale && s.DesiredReplicas() != 1 {
		return fmt.Errorf("%w: driver runs exactly one instance, and the spec asks for %d", ErrUnsupported, s.DesiredReplicas())
	}
	return nil
}

// CheckScale is the same branch for Scale, which does not take a spec.
//
// Replica counts of 0 and 1 are universal — every driver can stop and start one
// instance — so only a count above 1 needs the capability.
func CheckScale(caps Capabilities, replicas int) error {
	if replicas < 0 {
		return fmt.Errorf("%w: replicas %d is negative", ErrInvalidSpec, replicas)
	}
	if replicas > 1 && !caps.Scale {
		return fmt.Errorf("%w: driver runs exactly one instance, and %d were requested", ErrUnsupported, replicas)
	}
	return nil
}

func envNames(env []EnvVar) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		out = append(out, e.Name)
	}
	return out
}

// firstPaths renders at most three file paths, so an error message naming a
// large spec stays readable.
func firstPaths(files []File) string {
	const max = 3
	out := make([]string, 0, max)
	for i, f := range files {
		if i == max {
			out = append(out, fmt.Sprintf("… +%d more", len(files)-max))
			break
		}
		out = append(out, f.Path)
	}
	return strings.Join(out, ", ")
}
