package k8s_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// PROBES: TELLING A SERVING INSTANCE FROM A CRASHLOOPING ONE.
//
// 🔴 WHAT THIS DRIVER RENDERED BEFORE: NOTHING. No startupProbe, no
// readinessProbe, no livenessProbe on any container — measured by reading the
// renderer, and visible live as a provisioned agent that exited 78 on every
// start, restarted seven times, and reported 0/1 for ever with nothing in the
// cluster timing it out and nothing in muster saying why. The crash was loud in
// `kubectl logs` and invisible in every place a person looks first.
// ---------------------------------------------------------------------------

// probedSpec is a spec that declares a health signal on its gateway port.
func probedSpec(name string) provision.Spec {
	s := richSpec(name)
	s.Health = provision.Health{HTTPGetPath: "/", PortName: provision.DefaultPortName}
	return s
}

// TestTheRenderedContainerCarriesAStartupAndALivenessProbe is the probe half's
// driver-side guard.
//
// 🔴 IT ASSERTS THE TIMINGS, NOT JUST THE PRESENCE, BECAUSE THE TWO PROBES ARE
// THE SAME REQUEST AND ONLY THE BUDGETS DISTINGUISH THEM. A renderer that built
// one *corev1.Probe and assigned it to both fields compiles, applies, and
// produces a pod whose liveness budget is the startup one (305s to notice a dead
// gateway) or whose startup budget is the liveness one (90s to boot, after which
// the kubelet kills a container that was still starting). Neither says anything
// in a log.
//
// ⚠ THE NUMBERS ARE SPELLED AS LITERALS HERE RATHER THAN READ FROM THE DRIVER'S
// CONSTANTS, deliberately: the constants are unexported, and an assertion that
// compared the render against the same constant it rendered from would pass for
// any value at all — including zero, which Kubernetes defaults to 3 failures at
// 10s and would kill a slow-booting agent.
func TestTheRenderedContainerCarriesAStartupAndALivenessProbe(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		d, cs := newDriver(t, m, nil)
		ctx := context.Background()
		spec := probedSpec("probed")
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		dep, err := cs.AppsV1().Deployments(m.ns("probed")).Get(ctx, "probed", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get Deployment: %v", err)
		}
		if n := len(dep.Spec.Template.Spec.Containers); n != 1 {
			t.Fatalf("instrument check FAILED: %d containers, want 1", n)
		}
		c := dep.Spec.Template.Spec.Containers[0]

		if c.StartupProbe == nil {
			t.Fatal("no startupProbe. A container whose gateway never starts reports 0/1 for " +
				"ever with nothing timing it out, which is the measured defect.")
		}
		if c.LivenessProbe == nil {
			t.Fatal("no livenessProbe. A gateway that stops answering is never restarted and " +
				"the pod stays Running and Ready.")
		}

		// ⚠ THERE IS DELIBERATELY NO SEPARATE "THE TWO PROBES ARE NOT THE SAME
		// OBJECT" ASSERTION, AND ONE WAS WRITTEN AND THEN DELETED. It compared the
		// two probes' budgets to each other and fired when they matched. Measured
		// against the mutant it was written for — livenessProbe building on
		// startupProbe's return value rather than on a fresh healthProbe — it DID
		// NOT FIRE: the shared object still gets liveness's period and threshold
		// written over it, so only initialDelaySeconds leaked, and the per-probe
		// literal below is what caught it (`liveness probe initialDelaySeconds = 5,
		// want 0`). Worse, it CANNOT be the sole detector of anything: the six
		// literals below pin both budgets on both probes to different values, so any
		// render in which the budgets coincide also fails at least one of them. An
		// assertion that can never be the thing that catches a defect reads as
		// coverage and provides none, which is worse than its absence.
		wantPort := intstr.FromInt32(8421) // richSpec's gateway port
		for _, p := range []struct {
			name             string
			probe            *corev1.Probe
			initialDelay     int32
			period           int32
			failureThreshold int32
		}{
			{"startup", c.StartupProbe, 5, 10, 30},
			{"liveness", c.LivenessProbe, 0, 30, 3},
		} {
			if p.probe.HTTPGet == nil {
				t.Errorf("%s probe is not an HTTP GET", p.name)
				continue
			}
			if p.probe.HTTPGet.Path != "/" {
				t.Errorf("%s probe path = %q, want %q (the spec's Health.HTTPGetPath)",
					p.name, p.probe.HTTPGet.Path, "/")
			}
			// 🔴 THE PORT IS THE ONE THE SPEC'S HEALTH NAMES, AND richSpec DECLARES
			// TWO PORTS SO THIS CANNOT PASS BY LUCK. Its metrics port is 9102; a
			// renderer that probed "the first port" or "the only port" would land on
			// 8421 by accident in a single-port fixture and on the wrong one here.
			if p.probe.HTTPGet.Port != wantPort {
				t.Errorf("%s probe port = %v, want %v (the port Health names, not the first "+
					"port declared)", p.name, p.probe.HTTPGet.Port, wantPort)
			}
			if p.probe.InitialDelaySeconds != p.initialDelay {
				t.Errorf("%s probe initialDelaySeconds = %d, want %d",
					p.name, p.probe.InitialDelaySeconds, p.initialDelay)
			}
			if p.probe.PeriodSeconds != p.period {
				t.Errorf("%s probe periodSeconds = %d, want %d", p.name, p.probe.PeriodSeconds, p.period)
			}
			if p.probe.FailureThreshold != p.failureThreshold {
				t.Errorf("%s probe failureThreshold = %d, want %d",
					p.name, p.probe.FailureThreshold, p.failureThreshold)
			}
		}

		// ⚠ AND NO READINESS PROBE, WHICH IS A DECISION AND IS PINNED AS ONE. The
		// startup probe already gates the container's first Ready; a readiness probe
		// on the identical request would additionally pull a single-replica agent out
		// of its own Service on one slow response, mid-turn. render.go carries the
		// argument and the condition under which to re-argue it.
		if c.ReadinessProbe != nil {
			t.Errorf("a readinessProbe was rendered: %+v.\n"+
				"    render.go argues this one OUT for a single-replica Deployment. If that "+
				"argument has changed, change it there and here together — this assertion is "+
				"what makes the absence a decision rather than an omission.", c.ReadinessProbe)
		}

		// 🔴 AND THE INIT CONTAINER MUST NOT CARRY THEM. It runs to completion and is
		// not supervised, so the apiserver ignores a startup/liveness pair there —
		// attaching them would read as coverage and provide none. renderInitContainer
		// copies Env, EnvFrom and VolumeMounts off the main container, so "it inherits
		// whatever the main container has" is the live hazard rather than a theory.
		for _, ic := range dep.Spec.Template.Spec.InitContainers {
			if ic.StartupProbe != nil || ic.LivenessProbe != nil || ic.ReadinessProbe != nil {
				t.Errorf("init container %q carries a probe", ic.Name)
			}
		}
	})
}

// TestASpecWithNoHealthSignalRendersNoProbes is the other direction.
//
// 🔴 IT IS NOT SYMMETRY: IT IS THE GUARANTEE THAT THIS CHANGE CANNOT BREAK A
// DRIVER CONSUMER THAT DECLARES NO HEALTH. provision.Spec.Health is new and
// optional, so every existing caller passes the zero value — and a renderer that
// probed unconditionally would attach a GET / to a port that may not serve HTTP
// at all, turning a working instance into a crash loop. The zero value must
// produce exactly the pre-change pod.
func TestASpecWithNoHealthSignalRendersNoProbes(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		d, cs := newDriver(t, m, nil)
		ctx := context.Background()
		// richSpec, NOT probedSpec: identical in every other field, so the only
		// difference between this case and the one above is Health.
		if err := d.Create(ctx, richSpec("unprobed")); err != nil {
			t.Fatalf("Create: %v", err)
		}
		dep, err := cs.AppsV1().Deployments(m.ns("unprobed")).Get(ctx, "unprobed", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get Deployment: %v", err)
		}
		c := dep.Spec.Template.Spec.Containers[0]
		if c.StartupProbe != nil || c.LivenessProbe != nil || c.ReadinessProbe != nil {
			t.Errorf("a spec declaring NO health signal was probed anyway "+
				"(startup=%v liveness=%v readiness=%v). An image that serves nothing on its "+
				"declared port would now crashloop under the liveness probe.",
				c.StartupProbe != nil, c.LivenessProbe != nil, c.ReadinessProbe != nil)
		}

		// 🔴 AND THE CASE THAT REACHES healthProbe'S IsZero GUARD RATHER THAN ITS
		// port-lookup FALLBACK. Measured during the mutation sweep: deleting the
		// IsZero check SURVIVED the case above, because a zero Health has PortName
		// "" and richSpec declares no port by that name — so the port lookup
		// returned 0 and the function answered nil for the other reason. An
		// UNNAMED port makes the lookup match "" and the guard the only thing
		// standing between a zero Health and a probe on a port nothing said serves
		// HTTP. provision.Spec.Validate permits an unnamed port, so this is a
		// reachable spec and not a contrivance.
		d2, cs2 := newDriver(t, m, nil)
		unnamed := richSpec("unnamed-port")
		unnamed.Ports = append(unnamed.Ports, provision.Port{Port: 7000})
		if err := d2.Create(ctx, unnamed); err != nil {
			t.Fatalf("Create (unnamed port): %v", err)
		}
		dep2, err := cs2.AppsV1().Deployments(m.ns("unnamed-port")).Get(ctx, "unnamed-port", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get Deployment (unnamed port): %v", err)
		}
		if p := dep2.Spec.Template.Spec.Containers[0].StartupProbe; p != nil {
			t.Errorf("a spec with NO health signal and an UNNAMED port was probed on %v. The "+
				"zero Health's empty PortName matched that port, so only the IsZero guard "+
				"prevents a probe against a port nothing declared as a health endpoint.",
				p.HTTPGet.Port)
		}
	})
}
