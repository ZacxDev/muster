package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// ---------------------------------------------------------------------------
// THE SEAM doc_seams.go ENTRY 1 BLOCKER (1) NAMED, AND THE TEST IT ASKED FOR.
//
// 🔴 ITS CLOSING CONDITION, QUOTED: "agentspec.Build declares a port (and the
// driver renders a Service), verified by a test that resolves an endpoint through
// a REAL driver — a fake clientset is enough — rather than through a stub
// resolver. Every test in this module today uses a stub, which is exactly why
// this shipped: both sides were tested and the SEAM was not."
//
// 🔴 SO THIS FILE IS DELIBERATELY NOT IN EITHER PACKAGE IT TESTS. internal/agentspec's
// suite drives Build and the noop driver; internal/provision/k8s's drives the driver
// against hand-written specs; internal/agentgateway's drives chat against a
// fixedResolver STUB. All three were green while an agent this binary provisions had
// no address, because no fixture ever held BOTH surfaces at once. cmd/muster-server is
// where they meet — provisioner.go builds the spec config AND constructs the driver
// over one client — so the combined state is only constructible here.
//
// ⚠ WHAT IT STILL CANNOT SEE, STATED SO NOBODY READS IT AS MORE THAN IT IS.
// k8s.io/client-go/kubernetes/fake is not an apiserver: it validates no name, admits
// no defaulting, runs no controller and resolves no DNS. So this proves the driver
// RESOLVES an address and the Service it names EXISTS as an object — it does not
// prove the address is dialable, and it cannot. The live commands for that claim are
// in this file's closing-condition comment on
// TestAnAgentThisBinaryProvisionsResolvesAnEndpoint.
// ---------------------------------------------------------------------------

// reachabilityDriver builds the REAL Kubernetes driver from the REAL config
// mapping, over a fake clientset.
//
// 🔴 IT GOES THROUGH k8sDriverConfig RATHER THAN WRITING A k8s.Config LITERAL, for
// the reason that function's own header gives: the mapping is where the mistakes
// are. A literal here would be this test deciding what the driver is configured
// with, and the endpoint template is one of the mapped fields — so a literal would
// pass over a k8sDriverConfig that stopped passing EndpointTemplate through.
func reachabilityDriver(t *testing.T, cfg config) (*k8sdriver.Driver, *fake.Clientset) {
	t.Helper()
	dc := k8sDriverConfig(cfg, log.New(&strings.Builder{}, "", 0))
	cs := fake.NewClientset()
	dc.Client = cs
	d, err := k8sdriver.New(dc)
	if err != nil {
		t.Fatalf("k8sdriver.New over the mapped config: %v", err)
	}
	return d, cs
}

// reachabilityAgent is an agent row with the fields Build reads. The token is
// present because buildSecrets omits the secret entirely without one, and an
// endpoint test over a spec missing a field is a weaker spec than the one a
// dispatch builds.
func reachabilityAgent() agents.Agent {
	return agents.Agent{
		ID:         4291,
		Name:       "harbour-kestrel",
		HooksToken: "fixture-per-agent-token-9f31c7",
	}
}

// TestAnAgentThisBinaryProvisionsResolvesAnEndpoint is the closing condition for
// doc_seams.go entry 1's first blocker.
//
// 🔴 RED AT THE PARENT COMMIT, AND THAT IS THE POINT RATHER THAN A FOOTNOTE. With
// agentspec.Build setting Ports nil, Endpoint answered
// `provision: instance declares no reachable endpoint: "harbour-kestrel" declares
// no port` here — the exact string the boot banner used to print — and the Service
// lookup below answered NotFound because renderService returns nil for a portless
// spec. Both assertions were red; both are green now.
//
// 🔴 CLOSING CONDITION A REVIEWER CAN RUN WITHOUT A CLUSTER:
//
//	go test ./cmd/muster-server/ -run TestAnAgentThisBinaryProvisionsResolvesAnEndpoint -v
//
// and, for the matrix, the same command at the parent commit — it fails there with
// provision.ErrNoEndpoint.
//
// ⚠ AND THE HALF THAT NEEDS A CLUSTER, NAMED HONESTLY BECAUSE NO UNIT TEST COVERS
// IT. That the resolved address is DIALABLE, and that a turn against it is accepted,
// is not provable against a fake clientset: there is no kube-dns and nothing
// listening. The commands are, from a pod that can reach the agent's namespace:
//
//	kubectl -n <agent-namespace> get svc <agent-name> -o jsonpath='{.spec.ports[*].name}{"\n"}{.spec.ports[*].port}'
//	# on the Deployment, NOT the pod template — see k8s.AnnotationPort
//	kubectl -n <agent-namespace> get deploy <agent-name> -o jsonpath='{.metadata.annotations.muster\.dev/port}'
//	kubectl -n <agent-namespace> run probe --rm -it --image=curlimages/curl --restart=Never -- \
//	  curl -sS -o /dev/null -w '%{http_code}\n' http://<agent-name>.<agent-namespace>.svc:<port>/v1/responses
//
// The Makefile's `test-liveenv` target already owns the turn-level version of that
// claim (internal/agentgateway/liveruntime_test.go), and it is the right place for
// it — not this file.
//
// 🔴 WHAT THIS ONE DOES *NOT* COVER: BLOCKER (2), THE CREDENTIAL — which is fixed
// in the same change and guarded by its OWN test, not by this one. doc_seams.go
// entry 1 recorded that the two are ORDERED ("while (1) holds the observable is
// ErrNoEndpoint and NO 401 is reachable"), so an address that resolves is exactly
// what makes a credential error reachable. This test asserts nothing about the
// bearer, and a green run here says nothing about whether a turn authenticates:
// TestTheProvisionedContainerCanDeriveTheBearerMusterSends is that claim, and the
// live turn neither of them can make is still owed.
func TestAnAgentThisBinaryProvisionsResolvesAnEndpoint(t *testing.T) {
	cfg := provisionerTestConfig(provisionerK8s)
	d, cs := reachabilityDriver(t, cfg)
	ctx := context.Background()

	a := reachabilityAgent()
	spec, err := agentspec.Build(a, agentSpecConfig(cfg), agentspec.Options{
		Instructions: "# Instructions\nbody\n",
	})
	if err != nil {
		t.Fatalf("agentspec.Build over this binary's own spec config: %v", err)
	}

	// 🔴 THE SPEC COMES FROM agentSpecConfig(cfg), NOT FROM A LITERAL, so the hop
	// this binary owns is inside the measurement. A test that built an
	// agentspec.Config by hand would stay green if agentSpecConfig stopped passing
	// the port through — and that function's own header already records a gap of
	// exactly that shape for the resource fields.
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ep, err := d.Endpoint(ctx, agents.RefOf(a))
	if err != nil {
		t.Fatalf("Endpoint for an agent THIS BINARY'S spec config built: %v\n"+
			"    This is doc_seams.go entry 1 blocker (1) reopening. internal/agentgateway\n"+
			"    resolves an endpoint BEFORE it builds a request, so this error is the whole\n"+
			"    observable of a chat turn against such an agent.", err)
	}
	if errors.Is(err, provision.ErrNoEndpoint) {
		t.Fatal("unreachable: the Fatalf above already returned")
	}

	// Pinned as LITERALS, not re-derived from the spec: an assertion reading
	// spec.Ports[0].Port and spec.Ref.Name back out is satisfied by a driver that
	// echoes whatever it was handed, including one that echoed the wrong field.
	if ep.Port != 18789 {
		t.Errorf("resolved port %d, want 18789 (agentspec.DefaultGatewayPort, which an unset "+
			"MUSTER_AGENT_GATEWAY_PORT resolves to)", ep.Port)
	}
	if ep.Host != "harbour-kestrel.devpod-harbour-kestrel.svc" {
		t.Errorf("resolved host %q, want %q — k8s.DefaultEndpointTemplate over the instance's "+
			"own name and namespace", ep.Host, "harbour-kestrel.devpod-harbour-kestrel.svc")
	}
	if ep.Scheme != "http" {
		t.Errorf("resolved scheme %q, want %q", ep.Scheme, "http")
	}
	// The URL agentgateway actually dials. agents.ResponsesURL appends
	// /v1/responses to this, so a wrong scheme/host/port here is a wrong request
	// line there.
	if got := ep.URL(); got != "http://harbour-kestrel.devpod-harbour-kestrel.svc:18789" {
		t.Errorf("Endpoint.URL() = %q", got)
	}

	// 🔴 AND THE SERVICE THE TEMPLATE NAMES MUST EXIST, WHICH IS A SEPARATE CLAIM
	// FROM THE ADDRESS RESOLVING. renderService returns nil for a spec with no
	// ports, and apply SWEEPS an object a render function declines to produce — so
	// before this change the resolved host (had one resolved) would have named a
	// Service that was never created. A resolvable name pointing at nothing is the
	// failure mode that reads as DNS rather than as a spec.
	svc, err := cs.CoreV1().Services("devpod-harbour-kestrel").Get(ctx, "harbour-kestrel", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the Service the endpoint template names does not exist: %v\n"+
			"    k8s renderService returns nil when the spec declares no ports, and apply\n"+
			"    sweeps what a render function declines to produce.", err)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("the Service declares %d port(s), want 1: %+v", len(svc.Spec.Ports), svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].Port != 18789 {
		t.Errorf("the Service publishes port %d, want 18789", svc.Spec.Ports[0].Port)
	}
	if svc.Spec.Ports[0].Name != provision.DefaultPortName {
		t.Errorf("the Service's port is named %q, want %q", svc.Spec.Ports[0].Name, provision.DefaultPortName)
	}
}

// TestTheRecipesNameTheAnnotationEndpointActuallyReads pins a RELATIONSHIP between
// runnable prose and a rendered object.
//
// 🔴 THE DEFECT IT EXISTS FOR: `muster.dev/port` is written on the DEPLOYMENT's
// ObjectMeta, and SIX sites said `.spec.template.metadata.annotations` — wrong in the
// same direction at every one, which is what a predicate open-coded at N places does.
// (The breakdown of those six is in k8s.AnnotationPort's own comment and is
// deliberately not restated here: an earlier revision of this line said "five
// comments plus two recipes", which is seven, and disagreed with that comment about
// the same event. One place for the fact, including the fact's arithmetic.) The
// runnable consequence is
// the one that cost something: the recipe sat directly under "if these two are empty,
// STOP: the instance was not provisioned by this binary", and for a CORRECTLY
// provisioned instance it prints empty. The next person to run the one proof this
// work still owes would have been told to stop, with precisely the wrong diagnosis.
//
// 🔴 SO THE GUARD IS NOT A SPELLING CHECK ON THE COMMENT. It renders a real object
// through the real driver, measures BOTH paths off it, and then requires every
// jsonpath in the tree that names this annotation to be the one that is non-empty.
// The two measurements are each other's control: if the Deployment path were also
// empty the render is broken, and if the pod-template path were also populated the
// recipe could not be wrong.
//
// ⚠ WHAT IT CANNOT SEE — AND TWO OF THESE HAVE THE SAME SILENTLY-EMPTY SYMPTOM THE
// GUARD EXISTS FOR, WHICH IS WHY THEY ARE LISTED RATHER THAN LEFT IMPLIED:
//
//   - a recipe written in prose, or addressing the annotation some other way
//     (`-o yaml | grep`, a Go client). Different symptom: those fail visibly.
//   - 🔴 a jsonpath with the dot UNESCAPED — `{.metadata.annotations.muster.dev/port}`.
//     The regex requires the escaped `muster\.dev` form, and kubectl reads the
//     unescaped one as nested fields and prints EMPTY rather than erroring. Same
//     wrong diagnosis, invisible to this guard.
//   - 🔴 a recipe in a file that is not `.go` or `.md` — a Makefile target, a shell
//     script, a chart's README. The walk covers those two extensions only.
//
// The per-file requirement below is what limits the first two: a recipe reworded into
// an unmatched form makes its file's count drop to zero, and that is an error. It does
// nothing for the third — a recipe in a new file type is simply not looked at.
func TestTheRecipesNameTheAnnotationEndpointActuallyReads(t *testing.T) {
	cfg := provisionerTestConfig(provisionerK8s)
	d, cs := reachabilityDriver(t, cfg)
	ctx := context.Background()

	a := reachabilityAgent()
	spec, err := agentspec.Build(a, agentSpecConfig(cfg), agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build: %v", err)
	}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dep, err := cs.AppsV1().Deployments("devpod-harbour-kestrel").
		Get(ctx, "harbour-kestrel", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}

	onDeployment := dep.Annotations[k8sdriver.AnnotationPort]
	onPodTemplate := dep.Spec.Template.Annotations[k8sdriver.AnnotationPort]

	// The path Driver.Endpoint reads. Non-empty is this test's positive control:
	// an empty value here would make the comparison below vacuous.
	if onDeployment == "" {
		t.Fatalf("positive control FAILED: %s is absent from the Deployment's own "+
			"annotations, which is where Driver.Endpoint reads it. Either the render "+
			"changed or the spec declares no port, and the jsonpath check below would be "+
			"comparing against nothing. Deployment annotations: %v",
			k8sdriver.AnnotationPort, dep.Annotations)
	}
	// And the path the recipes used to name. It must be EMPTY, or the defect this
	// test is about could not have happened and the assertion is not measuring it.
	if onPodTemplate != "" {
		t.Fatalf("the pod template now carries %s=%q as well. That is not a failure of "+
			"the recipes — it means the render changed and this guard's premise is stale. "+
			"Re-read k8s.AnnotationPort's comment and decide which path is canonical.",
			k8sdriver.AnnotationPort, onPodTemplate)
	}
	t.Logf("measured off one rendered object: pod-template path = %q, Deployment path = %q",
		onPodTemplate, onDeployment)

	// 🔴 NOW THE PROSE, AGAINST THAT MEASUREMENT. The capture is whatever the recipe
	// puts between `{` and `annotations.` — ".metadata." for the path that works,
	// ".spec.template.metadata." for the one that prints empty.
	re := regexp.MustCompile(`jsonpath='\{(\.[A-Za-z.]*)annotations\.muster\\\.dev/port`)
	root := moduleRootForSweep(t)
	var found int
	perFile := map[string]int{}
	err = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if e.Name() == ".git" || e.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".md") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range re.FindAllStringSubmatch(string(body), -1) {
			found++
			rel, _ := filepath.Rel(root, path)
			perFile[filepath.ToSlash(rel)]++
			if m[1] != ".metadata." {
				t.Errorf("%s names jsonpath '{%sannotations.muster\\.dev/port}', which reads "+
					"%q on a correctly-provisioned instance.\n"+
					"    Measured off one rendered object in this run: that path = %q, and "+
					"'{.metadata.annotations...}' = %q.\n"+
					"    The annotation is on the DEPLOYMENT. A recipe naming the pod template "+
					"prints empty and does not error, so it reads as \"the instance was never "+
					"provisioned\" — the exact opposite of what it is run to confirm. "+
					"k8s.AnnotationPort states this once; point the recipe at it.",
					rel, m[1], onPodTemplate, onPodTemplate, onDeployment)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// 🔴 POSITIVE CONTROL ON THE WALK, PER FILE AND NOT AS A TOTAL — AND THE TOTAL
	// VERSION WAS MEASURED WALKABLE. It read `if found < 2` with a message naming the
	// two files that must carry a recipe, which is a count of the whole tree wearing
	// the description of a per-file relationship: rewriting liveruntime_test.go's RUN
	// IT recipe into a double-quoted form this regex misses, while adding one more
	// correct jsonpath in THIS file, returns the total to 2 and the test PASSES — so
	// the operator-facing recipe the guard exists for goes unchecked. That is the
	// "a guard's DESCRIPTION claims coverage, the body inspects one side" shape, in
	// the guard written to fix exactly that shape one level up.
	//
	// So the requirement is now stated as the relationship it always claimed: each
	// named file must itself carry at least one matching recipe. A zero anywhere is
	// indistinguishable from correctness, which is why it is an error rather than a
	// log line.
	for _, want := range []string{
		"internal/agentgateway/liveruntime_test.go", // the live-runtime target's RUN IT block
		"cmd/muster-server/reachability_test.go",    // this file's closing-condition comment
	} {
		if perFile[want] == 0 {
			t.Errorf("instrument check FAILED: %s carries no jsonpath recipe naming %s.\n"+
				"    Either the recipe was removed, or it was reworded into a form this "+
				"regex does not match (a double-quoted jsonpath, or an unescaped dot) — in "+
				"which case the recipe is UNCHECKED and the per-file counts below are the "+
				"only thing that says so.\n    counts: %v", want, k8sdriver.AnnotationPort, perFile)
		}
	}
	t.Logf("checked %d jsonpath recipe(s) naming %s, per file: %v",
		found, k8sdriver.AnnotationPort, perFile)
}

// TestTheNoopArmOfTheLifecycleBannerIsTrueOfTheNoopDriver pins the one banner
// sentence that was unguarded, and it is pinnable so it is pinned.
//
// 🔴 THE LINE SAID "an address resolves from the driver's own endpoint template
// INSTEAD", WHICH READS AS "THE PORT IS INERT ON noop". Measured false at three
// points: provision.Noop.Endpoint hands spec.PortNumber(provision.DefaultPortName) to
// ResolveEndpoint and DefaultNoopEndpointTemplate supplies only the HOST, so 18789
// resolves http://…:18789, 29999 resolves :29999, and port 0 is a hard
// ErrNoEndpoint. `noop` is a supported deployment and the banner fixture renders this
// very arm with it, so the sentence reached a real reader.
//
// 🔴 NOTHING GUARDED IT, WHICH IS WHY A SECOND FALSE DRAFT LANDED IN THE SAME PLACE.
// The ledger guard checks the variable is NAMED in both arms and that the two lines
// DIFFER — a words check over a state claim. This asserts the STATE: the port the
// banner prints is the port this driver actually resolves, and the host is the
// template's. The negative control is what makes it a measurement — a different
// configured port must MOVE the resolved port, which is exactly what "instead"
// denied.
//
// ⚠ IT PINS THE RELATIONSHIP, NOT THE WORDING. A reword that kept the meaning passes
// here; one that re-asserts inertness passes here too, and only a human reading the
// line against this test's name would catch it. That is a real gap and it is smaller
// than the one it replaces: before, nothing at all tied the sentence to the driver.
func TestTheNoopArmOfTheLifecycleBannerIsTrueOfTheNoopDriver(t *testing.T) {
	build := func(port int) provision.Spec {
		t.Helper()
		cfg := provisionerTestConfig(provisionerNoop)
		cfg.AgentGatewayPort = port
		spec, err := agentspec.Build(reachabilityAgent(), agentSpecConfig(cfg), agentspec.Options{})
		if err != nil {
			t.Fatalf("agentspec.Build(port=%d): %v", port, err)
		}
		return spec
	}
	resolve := func(spec provision.Spec) provision.Endpoint {
		t.Helper()
		d := provision.MustNewNoop()
		ctx := context.Background()
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("noop Create: %v", err)
		}
		ep, err := d.Endpoint(ctx, provision.Ref{Name: spec.Ref.Name})
		if err != nil {
			t.Fatalf("noop Endpoint: %v — the banner claims an address resolves on this "+
				"driver; if it does not, the line is wrong in the other direction", err)
		}
		return ep
	}

	// The default, which is what the banner prints on an unconfigured deployment.
	dflt := resolve(build(0))
	if dflt.Port != agentspec.DefaultGatewayPort {
		t.Errorf("noop resolved port %d for an unset %s, want agentspec.DefaultGatewayPort "+
			"(%d). The banner prints the resolved value, so the two would disagree.",
			dflt.Port, envAgentGatewayPort, agentspec.DefaultGatewayPort)
	}
	// The HOST comes from the driver's template — the half of the old sentence that
	// was true, kept asserted so a reword cannot quietly drop it.
	if !strings.HasSuffix(dflt.Host, ".noop.invalid") {
		t.Errorf("noop resolved host %q, want the driver's own template host (…%s)",
			dflt.Host, ".noop.invalid")
	}

	// 🔴 NEGATIVE CONTROL: a configured port MOVES the resolved port. Without this the
	// assertion above is satisfied by a driver that ignores the spec's port entirely —
	// which is precisely what the retracted "instead" asserted.
	const other = 21473 // not DefaultGatewayPort, not a neighbour of it
	moved := resolve(build(other))
	if moved.Port != other {
		t.Errorf("noop resolved port %d with %s=%d: the port does NOT travel on this "+
			"driver, so the banner must not claim it decides what a turn would dial.",
			moved.Port, envAgentGatewayPort, other)
	}
	if moved.Port == dflt.Port {
		t.Fatalf("the configured and default ports both resolved to %d, so this case "+
			"could not tell them apart", moved.Port)
	}

	// And the sentence must not regain the retracted word. SPELLED, hence walkable by
	// rewording — it is here because it survives a lazy edit, not because it is
	// sufficient. See this test's own ⚠ note.
	onOut, _ := bannerBothDirections(t)
	line := lineNaming(onOut, envAgentGatewayPort)
	if line == "" {
		t.Fatalf("no banner line names %s.\nbanner:\n%s", envAgentGatewayPort, onOut)
	}
	if !strings.Contains(line, "noop") {
		t.Fatalf("instrument check FAILED: the rendered arm is not the noop one (%q), so "+
			"the assertion below is about a different sentence", line)
	}
	if strings.Contains(line, "endpoint template instead") {
		t.Errorf("the noop arm has regained %q. The template supplies only the HOST; the "+
			"port travels, measured at three points in this test's doc.\n  line: %s",
			"endpoint template instead", line)
	}
}

// TestTheResolvedPortIsTheConfiguredOneAndNotADriverConstant is the control that
// makes the test above a measurement of the CONFIGURED value rather than of a
// coincidence.
//
// 🔴 EVERY NUMBER IN THE TEST ABOVE IS agentspec.DefaultGatewayPort, SO IT CANNOT
// DISTINGUISH "the port travelled from the spec" FROM "something downstream
// hardcoded the same literal". The whole chain — buildPorts, renderAnnotations,
// Driver.Endpoint, provision.ResolveEndpoint — is five hops long, and a hardcoded
// literal at any of them survives a suite whose only fixture value IS that
// literal. This drives a port the constant cannot equal and watches the resolved
// address move with it.
//
// ⚠ IT IS A REGRESSION TEST FOR THE WIRING, NOT FOR THE SEAM. The seam is the test
// above; this one is what stops that test being walkable.
func TestTheResolvedPortIsTheConfiguredOneAndNotADriverConstant(t *testing.T) {
	const want = 21473 // not DefaultGatewayPort, not a neighbour of it

	cfg := provisionerTestConfig(provisionerK8s)
	cfg.AgentGatewayPort = want
	d, cs := reachabilityDriver(t, cfg)
	ctx := context.Background()

	a := reachabilityAgent()
	spec, err := agentspec.Build(a, agentSpecConfig(cfg), agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build: %v", err)
	}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ep, err := d.Endpoint(ctx, agents.RefOf(a))
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep.Port != want {
		t.Errorf("resolved port %d with %s=%d.\n"+
			"    If this says %d, the configured port is not travelling and something between\n"+
			"    config.AgentGatewayPort and provision.ResolveEndpoint is answering with\n"+
			"    agentspec.DefaultGatewayPort instead. The candidates, in order: agentSpecConfig\n"+
			"    not mapping the field, agentspec.buildPorts ignoring Config.GatewayPort, k8s\n"+
			"    renderAnnotations not writing the port annotation, or Driver.Endpoint not\n"+
			"    reading it.", ep.Port, envAgentGatewayPort, want, agentspec.DefaultGatewayPort)
	}

	// The Service has to move with it too, or the address resolves to a port the
	// cluster does not publish.
	svc, err := cs.CoreV1().Services("devpod-harbour-kestrel").Get(ctx, "harbour-kestrel", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Service: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != want {
		t.Errorf("the Service publishes %+v, want a single port %d", svc.Spec.Ports, want)
	}

	// Control: the DEFAULT resolves to something different, or the assertion above
	// would pass for an implementation that always reported `want`.
	dflt := provisionerTestConfig(provisionerK8s)
	dd, _ := reachabilityDriver(t, dflt)
	dspec, err := agentspec.Build(a, agentSpecConfig(dflt), agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build (default): %v", err)
	}
	if err := dd.Create(ctx, dspec); err != nil {
		t.Fatalf("Create (default): %v", err)
	}
	dep, err := dd.Endpoint(ctx, agents.RefOf(a))
	if err != nil {
		t.Fatalf("Endpoint (default): %v", err)
	}
	if dep.Port == ep.Port {
		t.Fatalf("the configured and default ports both resolved to %d, so this case could not "+
			"tell them apart", dep.Port)
	}
	if dep.Port != agentspec.DefaultGatewayPort {
		t.Errorf("an unset %s resolved to port %d, want agentspec.DefaultGatewayPort (%d)",
			envAgentGatewayPort, dep.Port, agentspec.DefaultGatewayPort)
	}
}

// TestTheProvisionedContainerCanDeriveTheBearerMusterSends is the closing
// condition for doc_seams.go entry 1's SECOND blocker.
//
// 🔴 THE CONTRACT HAS TWO IMPLEMENTATIONS AND ONE OF THEM IS NOT IN THIS
// REPOSITORY, SO THIS TEST IS THE WHOLE OF THE AGREEMENT. muster's half is
// agentgateway.HooksSHA256().Bearer(row.HooksToken). The container's half,
// quoted from that function's own doc, is
//
//	GATEWAY_TOKEN=$(echo -n "gw-${HOOKS_TOKEN}" | sha256sum | cut -d' ' -f1)
//
// and it lives in the agent deployment's chart. This test REPRODUCES the
// container's side over the built spec's own environment — exactly the bytes a
// pod would receive — and requires the two derivations to agree.
//
// 🔴 IT GUARDS THE RELATIONSHIP AND NOT THE SPELLING, WHICH IS WHY IT HASHES
// RATHER THAN ASSERTING A LITERAL. A test requiring the string "HOOKS_TOKEN" in
// Secrets is walkable by any refactor that moves the constant, and — worse —
// would stay green if the name were right and the VALUE wrong, which is the same
// 401. Reading the environment by the name the derivation consumes, hashing it,
// and comparing bearers fails for a missing name, a wrong name, a wrong value
// and an empty value, each of which produces an identical runtime symptom.
//
// 🔴 RED AT THE PARENT COMMIT: buildSecrets emitted agentspec.EnvToken alone, so
// the lookup below found nothing, the container's half hashed "" and the two
// bearers differed. The failure mode it was hiding is the one this comment opens
// with — a 401 on every chat turn, attributed to a bad credential rather than to
// a variable nobody set.
//
// ⚠ WHAT IT STILL DOES NOT PROVE: that the OTHER repository's chart really
// computes that formula. That is a claim about a file this module cannot see, and
// the only thing that tests it is a real turn against a real runtime — the
// Makefile's `test-liveenv` target (internal/agentgateway/liveruntime_test.go).
func TestTheProvisionedContainerCanDeriveTheBearerMusterSends(t *testing.T) {
	cfg := provisionerTestConfig(provisionerK8s)
	a := reachabilityAgent()

	spec, err := agentspec.Build(a, agentSpecConfig(cfg), agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build: %v", err)
	}

	// The pod's whole environment, as a driver would assemble it: Env and Secrets
	// both land in the container's environment, and a reader of a variable cannot
	// tell which list it came from.
	podEnv := map[string]string{}
	for _, e := range spec.Env {
		podEnv[e.Name] = e.Value
	}
	for _, e := range spec.Secrets {
		podEnv[e.Name] = e.Value
	}

	// 🔴 INSTRUMENT CHECK FIRST. An empty map satisfies "the bearers differ" and
	// "the bearers agree" with equal ease depending on which way the assertion is
	// written, and it reads identically to a spec that carries nothing.
	if len(podEnv) < 3 {
		t.Fatalf("instrument check FAILED: the built spec's environment has %d entr(ies) (%v); "+
			"a spec this thin is not the one a dispatch builds and the comparison below would "+
			"be over nothing", len(podEnv), podEnv)
	}

	// THE CONTAINER'S SIDE, reproduced from the shell command in
	// agentgateway.HooksSHA256's own doc comment. `echo -n` writes no trailing
	// newline, so the hashed bytes are exactly "gw-" + the variable's value.
	sum := sha256.Sum256([]byte("gw-" + podEnv[agentspec.EnvGatewayToken]))
	containerSide := hex.EncodeToString(sum[:])

	// MUSTER'S SIDE, through the real Runtime the `hooks-sha256` scheme builds.
	musterSide := agentgateway.HooksSHA256().Bearer(a.HooksToken)

	if containerSide != musterSide {
		t.Errorf("the container would derive a DIFFERENT bearer from the one muster sends.\n"+
			"  container side (sha256 of \"gw-\"+$%s, where $%s=%q): %s\n"+
			"  muster side    (Bearer of the row's token):            %s\n"+
			"  Every chat turn against an agent this binary provisioned is then a 401 from\n"+
			"  the runtime — which reads as a bad credential rather than as a variable the\n"+
			"  spec never set. agentspec.buildSecrets is what has to carry the row's token\n"+
			"  under %s; see doc_seams.go entry 1 blocker (2).",
			agentspec.EnvGatewayToken, agentspec.EnvGatewayToken,
			podEnv[agentspec.EnvGatewayToken], containerSide, musterSide,
			agentspec.EnvGatewayToken)
	}

	// 🔴 NEGATIVE CONTROL ON THE COMPARISON ITSELF. The equality above is also
	// satisfied by a Bearer() that ignores its argument, and by a derivation that
	// is constant. A DIFFERENT token must produce a different bearer, or this test
	// cannot see a value error at all.
	other := sha256.Sum256([]byte("gw-" + "a-different-token-4d19ba"))
	if hex.EncodeToString(other[:]) == containerSide {
		t.Fatal("negative control FAILED: a different token hashed to the same bearer, so " +
			"this comparison cannot distinguish the right value from any other")
	}
	if agentgateway.HooksSHA256().Bearer("a-different-token-4d19ba") == musterSide {
		t.Fatal("negative control FAILED: Bearer() returned the same value for a different " +
			"token, so it is not reading its argument and the agreement above is vacuous")
	}

	// 🔴 AND THE OTHER NAME MUST SURVIVE, WHICH IS THE HALF A RENAME WOULD HAVE
	// BROKEN. agentspec.EnvToken is what the in-pod CLI reads (cmd/muster's
	// envToken) and what the work-autosave daemon posts its durability alarm with.
	// Shipping the gateway's name INSTEAD OF it trades a chat 401 for a silent
	// agent, which is a worse failure because nothing reports it.
	if got := podEnv[agentspec.EnvToken]; got != a.HooksToken {
		t.Errorf("$%s = %q, want the row's token: cmd/muster's envToken reads this name, and "+
			"an instance that cannot authenticate to muster goes quiet rather than erroring",
			agentspec.EnvToken, got)
	}
	// The two names carry ONE value. A spec that set them to different tokens would
	// authenticate one path and 401 the other.
	if podEnv[agentspec.EnvToken] != podEnv[agentspec.EnvGatewayToken] {
		t.Errorf("$%s and $%s carry different values (%q vs %q); they are two names for the "+
			"agent's single token", agentspec.EnvToken, agentspec.EnvGatewayToken,
			podEnv[agentspec.EnvToken], podEnv[agentspec.EnvGatewayToken])
	}

	// 🔴 BOTH ARE SECRETS, NOT Env, AND THAT IS WHAT MAKES THE CONFIDENTIAL SET
	// ENUMERABLE. A driver may log or ConfigMap Spec.Env; Build's own header says
	// the split is the whole reason provision.Spec has two fields. The map above
	// deliberately flattens them, so this reads the lists directly.
	for _, name := range []string{agentspec.EnvToken, agentspec.EnvGatewayToken} {
		var inSecrets, inEnv bool
		for _, e := range spec.Secrets {
			if e.Name == name {
				inSecrets = true
			}
		}
		for _, e := range spec.Env {
			if e.Name == name {
				inEnv = true
			}
		}
		if !inSecrets {
			t.Errorf("%s is not in Spec.Secrets", name)
		}
		if inEnv {
			t.Errorf("%s is in Spec.Env, where a driver may log, diff or ConfigMap it", name)
		}
	}
}

// TestAnAgentWithNoTokenShipsNeitherNameRatherThanAnEmptyOne is the other arm of
// buildSecrets' branch, and it is a refusal-quality guard rather than a
// correctness one.
//
// 🔴 AN EMPTY SECRET WOULD CONVERT A DIAGNOSABLE REFUSAL INTO A 401.
// agentgateway's reach() refuses an agent with no token BEFORE it builds a
// request, with a message naming the cause. sha256("gw-" + "") is a perfectly
// well-formed 64-hex credential, so a container holding an empty
// agentspec.EnvGatewayToken would derive one, send it, and be refused by the
// runtime — moving the failure from a named refusal to an opaque 401.
func TestAnAgentWithNoTokenShipsNeitherNameRatherThanAnEmptyOne(t *testing.T) {
	cfg := provisionerTestConfig(provisionerK8s)
	a := reachabilityAgent()
	a.HooksToken = ""

	spec, err := agentspec.Build(a, agentSpecConfig(cfg), agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build: %v", err)
	}
	for _, e := range spec.Secrets {
		if e.Name == agentspec.EnvToken || e.Name == agentspec.EnvGatewayToken {
			t.Errorf("a tokenless agent's spec carries %s=%q; an empty gateway token hashes to "+
				"a well-formed bearer the runtime refuses 401, which is strictly less "+
				"diagnosable than agentgateway.reach's own refusal", e.Name, e.Value)
		}
	}

	// Control: the SAME spec WITH a token does carry both, or the loop above is
	// satisfied by a Build that emits no secrets at all.
	a.HooksToken = "fixture-per-agent-token-9f31c7"
	spec, err = agentspec.Build(a, agentSpecConfig(cfg), agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build (with token): %v", err)
	}
	var found int
	for _, e := range spec.Secrets {
		if e.Name == agentspec.EnvToken || e.Name == agentspec.EnvGatewayToken {
			found++
		}
	}
	if found != 2 {
		t.Errorf("control FAILED: a spec built WITH a token carries %d of the two token names, "+
			"so the absence asserted above is not attributable to the empty token", found)
	}
}

// TestAMalformedGatewayPortIsRefusedAtBootRatherThanInsideADispatch pins the
// parse-and-refuse half.
//
// 🔴 A BAD VALUE HERE HAS TO BE A BOOT FAILURE, BECAUSE THE ALTERNATIVE IS
// SILENT. agentspec.Build refuses an out-of-range port too — and it is called
// from inside a dispatch goroutine, where the only trace is a log line nobody is
// reading and the operator's POST has already answered 200. Every other
// agent-spec variable in config.go is refused at boot for exactly this reason;
// this one is not special.
func TestAMalformedGatewayPortIsRefusedAtBootRatherThanInsideADispatch(t *testing.T) {
	load := func(value string) (config, error) {
		return loadConfig(func(n string) string {
			switch n {
			case envDatabase:
				return "postgres://unused"
			case envAgentProvisioner:
				return provisionerK8s
			case envAgentImageRepo:
				return "registry.example.test/muster/agent-runtime"
			case envAgentAPIURL:
				return "http://muster.example.test:8105"
			case envAgentGatewayPort:
				return value
			}
			return ""
		})
	}

	for _, bad := range []string{"not-a-number", "0", "-1", "65536", "18789x", "187 89"} {
		cfg, err := load(bad)
		if err == nil {
			err = cfg.validate()
		}
		if err == nil {
			t.Errorf("%s=%q was accepted; the refusal would then happen inside a dispatch "+
				"goroutine, or not at all", envAgentGatewayPort, bad)
			continue
		}
		if !strings.Contains(err.Error(), envAgentGatewayPort) {
			t.Errorf("%s=%q was refused without naming the variable: %v", envAgentGatewayPort, bad, err)
		}
	}

	// 🔴 CONTROLS, OR THE LOOP ABOVE IS SATISFIED BY A PARSER THAT REFUSES
	// EVERYTHING. Unset is the state every deployment runs in and it must resolve
	// to the constant; a legal value must survive and arrive on the field.
	cfg, err := load("")
	if err != nil {
		t.Fatalf("an unset %s was refused: %v", envAgentGatewayPort, err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate refused a config with %s unset: %v", envAgentGatewayPort, err)
	}
	if cfg.AgentGatewayPort != 0 {
		t.Errorf("an unset %s parsed to %d, want 0 — zero is what agentspec resolves to "+
			"DefaultGatewayPort, and any other sentinel would make this binary a second "+
			"authority on the number", envAgentGatewayPort, cfg.AgentGatewayPort)
	}
	if agentSpecConfig(cfg).GatewayPort != 0 {
		t.Errorf("agentSpecConfig turned an unset port into %d", agentSpecConfig(cfg).GatewayPort)
	}

	// 🔴 A LEGAL VALUE THE CONSTANT CANNOT EQUAL, so "it arrived" is distinguishable
	// from "the default happens to match".
	cfg, err = load("21473")
	if err != nil {
		t.Fatalf("a legal %s was refused: %v", envAgentGatewayPort, err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate refused %s=21473: %v", envAgentGatewayPort, err)
	}
	if cfg.AgentGatewayPort != 21473 {
		t.Errorf("%s=21473 parsed to %d", envAgentGatewayPort, cfg.AgentGatewayPort)
	}
	if got := agentSpecConfig(cfg).GatewayPort; got != 21473 {
		t.Errorf("agentSpecConfig(cfg).GatewayPort = %d, want 21473 — the field is declared on "+
			"config and not handed to the spec builder, which is the shape of an unread knob",
			got)
	}
}
