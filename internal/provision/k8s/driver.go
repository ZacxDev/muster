package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ZacxDev/muster/internal/provision"
)

// Config is how the Kubernetes driver is configured.
//
// 🔴 TWO FIELDS ARE POINTERS BECAUSE "UNSET" AND "EMPTY" ARE DIFFERENT
// ANSWERS, AND NOTHING IN THE PROJECT THIS CAME FROM COULD TELL THEM APART —
// it used os.Getenv exclusively, os.LookupEnv appeared zero times repo-wide,
// and the consequence was a required image repository defaulting to the empty
// string and producing an incomprehensible pull failure instead of "you did not
// configure an agent image". WorkspaceStorageClass distinguishes "this cluster
// has no storage I may use" (nil) from "use the cluster's default class" ("").
type Config struct {
	// Client is the cluster API client. REQUIRED.
	//
	// It is an interface rather than a concrete clientset so a fake can be
	// injected, which is what makes every test in this package runnable without
	// a cluster.
	Client kubernetes.Interface

	// RESTConfig enables Exec. Optional: nil means Capabilities.Exec is false
	// and provision.Exec refuses, rather than the call failing later with a
	// transport error nobody can attribute.
	RESTConfig *rest.Config

	// NamespacePerInstance gives each instance its own namespace, named
	// NamespacePrefix+Name. This is what makes Destroy a namespace deletion
	// rather than an object enumeration, and it is what raises
	// Capabilities.Isolation to IsolationNamespace.
	NamespacePerInstance bool

	// NamespacePrefix prefixes per-instance namespaces. Empty means "muster-".
	NamespacePrefix string

	// Namespace is where instances go when NamespacePerInstance is false.
	// REQUIRED in that case.
	Namespace string

	// EndpointTemplate overrides DefaultEndpointTemplate. It is rendered with
	// provision.EndpointVars.
	EndpointTemplate string

	// EndpointScheme is the scheme Endpoint reports. Empty means "http".
	EndpointScheme string

	// WorkspaceStorageClass is the StorageClass for persistent workspaces.
	//
	//   nil -> this driver reports Capabilities.Persistence FALSE and refuses a
	//          spec asking for a persistent workspace.
	//   ""  -> use the cluster's default StorageClass.
	//   "x" -> use class x.
	WorkspaceStorageClass *string

	// PolicyDisabled turns off Capabilities.Policy.
	//
	// ⚠ SET IT WHEN MUSTER'S OWN SERVICE ACCOUNT LACKS THE RBAC `escalate` AND
	// `bind` VERBS. Without them a grant fails at apply time with a permission
	// error that looks like a bug; with this set, provision.Grant refuses up
	// front with a reason a user interface can display.
	PolicyDisabled bool

	// Logger is optional.
	Logger *log.Logger
}

// Driver provisions onto Kubernetes. See the package doc for what it renders
// and what it deliberately does not do.
type Driver struct {
	cfg  Config
	tmpl provision.EndpointTemplate
	log  *log.Logger
}

var (
	_ provision.Provisioner   = (*Driver)(nil)
	_ provision.PolicyGranter = (*Driver)(nil)
	_ provision.Execer        = (*Driver)(nil)
)

// New builds the driver, validating the configuration up front so a bad
// endpoint template is a startup failure naming itself rather than a
// per-request error naming DNS.
func New(cfg Config) (*Driver, error) {
	if cfg.Client == nil {
		return nil, errors.New("k8s: Config.Client is required")
	}
	if !cfg.NamespacePerInstance && strings.TrimSpace(cfg.Namespace) == "" {
		return nil, errors.New("k8s: Config.Namespace is required when NamespacePerInstance is false " +
			"(there is no safe default: guessing 'default' would put agents somewhere nobody chose)")
	}
	raw := cfg.EndpointTemplate
	if strings.TrimSpace(raw) == "" {
		raw = DefaultEndpointTemplate
	}
	tmpl, err := provision.ParseEndpointTemplate(raw)
	if err != nil {
		return nil, fmt.Errorf("k8s: %w", err)
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	return &Driver{cfg: cfg, tmpl: tmpl, log: lg}, nil
}

// Driver implements provision.Provisioner.
func (d *Driver) Driver() string { return "kubernetes" }

// Capabilities implements provision.Provisioner.
//
// 🔴 TWO OF THESE ARE FALSE FOR REASONS THAT MATTER MORE THAN THE FEATURE:
//
//   - FQDNEgress. This driver renders no NetworkPolicy at all. Restricting an
//     agent's egress by DNS NAME is the control that addresses exfiltration by
//     a prompt-injected model; an address-range policy is not that control.
//     Declaring false is how a caller learns the mitigation is absent instead
//     of assuming it from the presence of Policy.
//   - Sidecars. One container plus an init container. The chart this replaces
//     could run three log tailers, and they were structurally invisible to that
//     project's own log reads anyway.
func (d *Driver) Capabilities() provision.Capabilities {
	isolation := provision.IsolationContainer
	if d.cfg.NamespacePerInstance {
		isolation = provision.IsolationNamespace
	}
	return provision.Capabilities{
		Isolation:      isolation,
		Secrets:        true,
		Files:          true,
		Policy:         !d.cfg.PolicyDisabled,
		FQDNEgress:     false,
		Persistence:    d.cfg.WorkspaceStorageClass != nil,
		Sidecars:       false,
		ResourceLimits: true,
		Scale:          true,
		Exec:           d.cfg.RESTConfig != nil,
	}
}

// namespaceFor resolves an instance's namespace.
func (d *Driver) namespaceFor(name string) string {
	if !d.cfg.NamespacePerInstance {
		return d.cfg.Namespace
	}
	prefix := d.cfg.NamespacePrefix
	if prefix == "" {
		prefix = "muster-"
	}
	return prefix + name
}

// blind wraps an API error as provision.ErrBlind.
//
// 🔴 EVERY "I COULD NOT REACH THE API" PATH GOES THROUGH HERE, AND THAT IS WHY
// List CANNOT ACCIDENTALLY RETURN AN EMPTY SLICE. A NotFound is explicitly NOT
// blindness and is handled at its own call site — collapsing the two is the
// mistake that turns "the cluster is unreachable" into "nothing is running".
func blind(op string, err error) error {
	return fmt.Errorf("%w: %s: %v", provision.ErrBlind, op, err)
}

func notFound(name string) error {
	return fmt.Errorf("%w: %q", provision.ErrNotFound, name)
}

// checkSpec is the capability branch plus the one requirement that is specific
// to this backend.
func (d *Driver) checkSpec(spec provision.Spec) error {
	if err := provision.CheckSpec(d.Capabilities(), spec); err != nil {
		return err
	}
	// A pod needs an image. Spec.Runtime is a union so that a process driver is
	// expressible; this driver is the half of the union that needs the other
	// member.
	if spec.Runtime.Image == "" {
		return fmt.Errorf("%w: the kubernetes driver needs Runtime.Image; a Command-only runtime has "+
			"nothing to run it in", provision.ErrUnsupported)
	}
	return nil
}

// Create implements provision.Provisioner.
func (d *Driver) Create(ctx context.Context, spec provision.Spec) error {
	if err := d.checkSpec(spec); err != nil {
		return err
	}
	ns := d.namespaceFor(spec.Ref.Name)

	existing, err := d.cfg.Client.AppsV1().Deployments(ns).Get(ctx, spec.Ref.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		// 🔴 IDEMPOTENCE IS DECIDED BY THE RECORDED FINGERPRINT, NOT BY A DIFF
		// OF THE OBJECTS. The live Deployment has been through defaulting and
		// possibly through another controller, so comparing it field by field
		// reports divergence for changes nobody made.
		want := provision.Fingerprint(spec)
		if existing.Annotations[annFingerprint] == want {
			return nil
		}
		return fmt.Errorf("%w: %q exists with fingerprint %s, spec is %s",
			provision.ErrDivergentSpec, spec.Ref.Name,
			short(existing.Annotations[annFingerprint]), short(want))
	case apierrors.IsNotFound(err):
		// fall through and create
	default:
		return blind("get deployment "+spec.Ref.Name, err)
	}
	return d.apply(ctx, spec, ns)
}

// Update implements provision.Provisioner.
func (d *Driver) Update(ctx context.Context, spec provision.Spec) error {
	if err := d.checkSpec(spec); err != nil {
		return err
	}
	return d.apply(ctx, spec, d.namespaceFor(spec.Ref.Name))
}

// apply renders and upserts every object for spec.
//
// ⚠ ORDER IS LOAD-BEARING. The Deployment goes LAST, because it references the
// ServiceAccount, the ConfigMap, the Secret and the PVC; creating it first
// produces a pod that fails to start for a reason attributed to the wrong
// object.
func (d *Driver) apply(ctx context.Context, spec provision.Spec, ns string) error {
	if d.cfg.NamespacePerInstance {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: instanceLabels(spec.Ref.Name),
		}}
		if _, err := d.cfg.Client.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return blind("create namespace "+ns, err)
		}
	}

	sa := d.renderServiceAccount(spec, ns)
	if err := upsert(ctx,
		func() error {
			_, e := d.cfg.Client.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{})
			return e
		},
		func() error {
			_, e := d.cfg.Client.CoreV1().ServiceAccounts(ns).Update(ctx, sa, metav1.UpdateOptions{})
			return e
		}); err != nil {
		return blind("apply serviceaccount "+sa.Name, err)
	}

	if cm := d.renderConfigMap(spec, ns); cm != nil {
		if err := upsert(ctx,
			func() error {
				_, e := d.cfg.Client.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
				return e
			},
			func() error {
				_, e := d.cfg.Client.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})
				return e
			}); err != nil {
			return blind("apply configmap "+cm.Name, err)
		}
	}

	if sec := d.renderSecret(spec, ns); sec != nil {
		if err := upsert(ctx,
			func() error {
				_, e := d.cfg.Client.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{})
				return e
			},
			func() error {
				_, e := d.cfg.Client.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
				return e
			}); err != nil {
			return blind("apply secret "+sec.Name, err)
		}
	}

	pvc, err := d.renderPVC(spec, ns)
	if err != nil {
		return err
	}
	if pvc != nil {
		// ⚠ CREATE-ONLY. Almost every field of a bound PVC is immutable, so an
		// Update here fails on a claim that is working perfectly. An existing
		// claim is the state we want.
		if _, err := d.cfg.Client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return blind("create pvc "+pvc.Name, err)
		}
	}

	if svc := d.renderService(spec, ns); svc != nil {
		if err := upsertService(ctx, d.cfg.Client, ns, svc); err != nil {
			return blind("apply service "+svc.Name, err)
		}
	}

	dep, err := d.renderDeployment(spec, ns)
	if err != nil {
		return err
	}
	if err := upsert(ctx,
		func() error {
			_, e := d.cfg.Client.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{})
			return e
		},
		func() error {
			_, e := d.cfg.Client.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{})
			return e
		}); err != nil {
		return blind("apply deployment "+dep.Name, err)
	}
	return nil
}

// upsert creates, and on AlreadyExists updates.
func upsert(_ context.Context, create, update func() error) error {
	err := create()
	if apierrors.IsAlreadyExists(err) {
		return update()
	}
	return err
}

// upsertService is separate because a Service's ClusterIP is assigned by the
// API server and is immutable: a blind Update with an empty ClusterIP is
// rejected. The existing value has to be carried over.
func upsertService(ctx context.Context, c kubernetes.Interface, ns string, svc *corev1.Service) error {
	_, err := c.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	cur, err := c.CoreV1().Services(ns).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	next := svc.DeepCopy()
	next.ResourceVersion = cur.ResourceVersion
	next.Spec.ClusterIP = cur.Spec.ClusterIP
	next.Spec.ClusterIPs = cur.Spec.ClusterIPs
	_, err = c.CoreV1().Services(ns).Update(ctx, next, metav1.UpdateOptions{})
	return err
}

// Scale implements provision.Provisioner.
func (d *Driver) Scale(ctx context.Context, ref provision.Ref, replicas int) error {
	if err := provision.CheckScale(d.Capabilities(), replicas); err != nil {
		return err
	}
	ns := d.namespaceFor(ref.Name)
	dep, err := d.cfg.Client.AppsV1().Deployments(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return notFound(ref.Name)
	}
	if err != nil {
		return blind("get deployment "+ref.Name, err)
	}
	n := int32(replicas)
	dep.Spec.Replicas = &n
	if _, err := d.cfg.Client.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return blind("scale deployment "+ref.Name, err)
	}
	return nil
}

// Destroy implements provision.Provisioner.
//
// 🔴 THE nil IS VERIFIED, NOT ASSUMED. After the deletes, the Deployment is
// re-read: if it is still there, this returns an error rather than a nil that
// means "I issued some requests". The contract says nil means removed or
// already absent, and this is what makes that a measurement.
//
// ⚠ WHAT IT DOES NOT CLAIM: that everything has finished TERMINATING. A
// namespace deletion is asynchronous and pods linger. nil means the API has
// accepted removal and the Deployment is gone from the API's view.
func (d *Driver) Destroy(ctx context.Context, ref provision.Ref) error {
	ns := d.namespaceFor(ref.Name)
	c := d.cfg.Client

	// Every object is attempted regardless of what came before. A failure to
	// delete one must not leave the pod running — the security-relevant order
	// is "stop the workload first, tidy after", which is why the Deployment
	// leads.
	var firstErr error
	fail := func(what string, err error) {
		if err == nil || apierrors.IsNotFound(err) {
			return
		}
		d.log.Printf("k8s: destroy %s: delete %s: %v", ref.Name, what, err)
		if firstErr == nil {
			firstErr = fmt.Errorf("delete %s: %w", what, err)
		}
	}

	fail("deployment", c.AppsV1().Deployments(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{}))
	fail("service", c.CoreV1().Services(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{}))
	fail("configmap", c.CoreV1().ConfigMaps(ns).Delete(ctx, ref.Name+"-files", metav1.DeleteOptions{}))
	fail("secret", c.CoreV1().Secrets(ns).Delete(ctx, ref.Name+"-env", metav1.DeleteOptions{}))
	fail("pvc", c.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, ref.Name+"-workspace", metav1.DeleteOptions{}))
	fail("serviceaccount", c.CoreV1().ServiceAccounts(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{}))

	// 🔴 CLUSTER-SCOPED POLICY OBJECTS OUTLIVE THE NAMESPACE, AND THAT IS A
	// SECURITY BUG WAITING FOR A NAMESAKE. A ClusterRole and its binding
	// survive the namespace, the Deployment and the database row, with nothing
	// left pointing at them; the next instance to take this name gets the same
	// namespace and the same ServiceAccount — the dangling binding's exact
	// subject — and silently inherits access nobody granted it.
	if err := d.revokeAllClusterPolicies(ctx, ref); err != nil {
		d.log.Printf("k8s: destroy %s: revoke cluster policies: %v", ref.Name, err)
		if firstErr == nil {
			firstErr = err
		}
	}

	if d.cfg.NamespacePerInstance {
		fail("namespace", c.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}))
	}

	if firstErr != nil {
		return fmt.Errorf("destroy %q did not complete: %w", ref.Name, firstErr)
	}

	// The verification. A driver whose Destroy is `return nil` passes every
	// other assertion anyone writes about it.
	_, err := c.AppsV1().Deployments(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return blind("verify destroy of "+ref.Name, err)
	default:
		return fmt.Errorf("destroy %q reported no error but the deployment is still present; "+
			"nil from Destroy means removed or already absent", ref.Name)
	}
}

// List implements provision.Provisioner.
//
// 🔴 IT RETURNS AN ERROR, NEVER AN EMPTY SLICE, WHEN THE API CANNOT BE REACHED.
// Both reads below go through blind(). The caller's fallback to stored status
// depends on being able to tell "nothing is running" from "I cannot see".
func (d *Driver) List(ctx context.Context) ([]provision.Instance, error) {
	ns := d.listNamespace()
	deps, err := d.cfg.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: managedSelector()})
	if err != nil {
		return nil, blind("list deployments", err)
	}
	pods, err := d.cfg.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: managedSelector()})
	if err != nil {
		return nil, blind("list pods", err)
	}

	byInstance := map[string][]corev1.Pod{}
	for i := range pods.Items {
		p := &pods.Items[i]
		byInstance[p.Labels[labelInstance]] = append(byInstance[p.Labels[labelInstance]], *p)
	}

	out := make([]provision.Instance, 0, len(deps.Items))
	for i := range deps.Items {
		dep := &deps.Items[i]
		out = append(out, d.instanceFrom(dep, byInstance[dep.Labels[labelInstance]]))
	}
	return out, nil
}

// listNamespace is metav1.NamespaceAll when the driver owns a namespace per
// instance, and the configured namespace otherwise.
func (d *Driver) listNamespace() string {
	if d.cfg.NamespacePerInstance {
		return metav1.NamespaceAll
	}
	return d.cfg.Namespace
}

// Get implements provision.Provisioner.
func (d *Driver) Get(ctx context.Context, ref provision.Ref) (provision.Instance, error) {
	ns := d.namespaceFor(ref.Name)
	dep, err := d.cfg.Client.AppsV1().Deployments(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return provision.Instance{}, notFound(ref.Name)
	}
	if err != nil {
		return provision.Instance{}, blind("get deployment "+ref.Name, err)
	}
	pods, err := d.cfg.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: instanceSelector(ref.Name)})
	if err != nil {
		return provision.Instance{}, blind("list pods for "+ref.Name, err)
	}
	return d.instanceFrom(dep, pods.Items), nil
}

// instanceFrom folds a Deployment and its pods into one Instance.
func (d *Driver) instanceFrom(dep *appsv1.Deployment, pods []corev1.Pod) provision.Instance {
	inst := provision.Instance{
		Ref:    provision.Ref{Name: dep.Labels[labelInstance]},
		Driver: d.Driver(),
		Group:  dep.Namespace,
		Phase:  provision.PhasePending,
	}
	if inst.Ref.Name == "" {
		inst.Ref.Name = dep.Name
	}
	if raw := dep.Annotations[annCorrelationID]; raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			inst.Ref.ID = id
		}
	}
	inst.Replicas = int(dep.Status.ReadyReplicas)

	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if desired == 0 {
		inst.Phase = provision.PhaseStopped
		inst.Reason = "scaled to zero"
		inst.Replicas = 0
		return inst
	}

	pod := newestPod(pods)
	if pod == nil {
		inst.Reason = "no pod yet"
		return inst
	}
	inst.InstanceID = pod.Name
	inst.Phase = phaseOf(pod)

	ready := len(pod.Status.ContainerStatuses) > 0
	for _, cs := range pod.Status.ContainerStatuses {
		inst.Restarts += cs.RestartCount
		if !cs.Ready {
			ready = false
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			inst.Reason = cs.State.Waiting.Reason
		}
		// 🔴 LastTerminationState, NOT State. This is the field that says WHY a
		// container that is running RIGHT NOW last died. An out-of-memory kill
		// is invisible everywhere else once the kubelet restarts it: the pod
		// reads Running and Ready with a bumped restart count and nothing says
		// what happened. It is the only signal that lets an instance report
		// that it is simply too small for the repository it was given.
		if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason != "" {
			inst.RestartReason = cs.LastTerminationState.Terminated.Reason
		}
	}
	inst.Ready = ready
	return inst
}

// newestPod picks the pod to report on: the most recently created one, so a
// rolling update reports the replacement rather than the pod on its way out.
func newestPod(pods []corev1.Pod) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if best == nil || p.CreationTimestamp.Time.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	if best == nil && len(pods) > 0 {
		best = &pods[0]
	}
	return best
}

func phaseOf(p *corev1.Pod) provision.Phase {
	switch p.Status.Phase {
	case corev1.PodRunning:
		return provision.PhaseRunning
	case corev1.PodPending:
		return provision.PhasePending
	case corev1.PodSucceeded:
		return provision.PhaseSucceeded
	case corev1.PodFailed:
		return provision.PhaseFailed
	default:
		return provision.PhaseUnknown
	}
}

// Endpoint implements provision.Provisioner, honouring the per-instance
// override recorded at create time.
func (d *Driver) Endpoint(ctx context.Context, ref provision.Ref) (provision.Endpoint, error) {
	ns := d.namespaceFor(ref.Name)
	dep, err := d.cfg.Client.AppsV1().Deployments(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return provision.Endpoint{}, notFound(ref.Name)
	}
	if err != nil {
		return provision.Endpoint{}, blind("get deployment "+ref.Name, err)
	}

	var override *provision.Endpoint
	if raw := dep.Annotations[annEndpoint]; raw != "" {
		var ep provision.Endpoint
		if err := json.Unmarshal([]byte(raw), &ep); err == nil && !ep.IsZero() {
			override = &ep
		} else {
			// A malformed override is reported, not silently ignored: silently
			// falling back to the template is how an operator's explicit
			// instruction disappears.
			d.log.Printf("k8s: %s has an unreadable %s annotation (%q); falling back to the template",
				ref.Name, annEndpoint, raw)
		}
	}

	port := 0
	if raw := dep.Annotations[annPort]; raw != "" {
		port, _ = strconv.Atoi(raw)
	}

	return provision.ResolveEndpoint(
		override, d.tmpl,
		provision.EndpointVars{Name: ref.Name, Group: ns, ID: ref.ID},
		port, d.cfg.EndpointScheme,
	)
}

// podFor resolves the pod to read logs from.
func (d *Driver) podFor(ctx context.Context, ref provision.Ref) (string, string, error) {
	ns := d.namespaceFor(ref.Name)
	if _, err := d.cfg.Client.AppsV1().Deployments(ns).Get(ctx, ref.Name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", notFound(ref.Name)
		}
		return "", "", blind("get deployment "+ref.Name, err)
	}
	pods, err := d.cfg.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: instanceSelector(ref.Name)})
	if err != nil {
		return "", "", blind("list pods for "+ref.Name, err)
	}
	pod := newestPod(pods.Items)
	if pod == nil {
		return ns, "", nil
	}
	return ns, pod.Name, nil
}

// TailLogs implements provision.Provisioner.
//
// An instance with no pod yet returns "" and no error: "there is nothing to
// read yet" is not a failure. An instance that does not exist is ErrNotFound.
func (d *Driver) TailLogs(ctx context.Context, ref provision.Ref, lines int64) (string, error) {
	ns, pod, err := d.podFor(ctx, ref)
	if err != nil {
		return "", err
	}
	if pod == "" {
		return "", nil
	}
	opts := &corev1.PodLogOptions{
		// 🔴 THE CONTAINER IS NAMED. The project this came from never passed
		// one, so every log read silently returned whichever container happened
		// to be first — which was fine until the day it was not.
		Container: containerName,
	}
	if lines > 0 {
		opts.TailLines = &lines
	}
	rc, err := d.cfg.Client.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		return "", blind("stream logs for "+ref.Name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("read logs for %s: %w", ref.Name, err)
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// StreamLogs implements provision.Provisioner.
func (d *Driver) StreamLogs(ctx context.Context, ref provision.Ref, emit func(string)) error {
	ns, pod, err := d.podFor(ctx, ref)
	if err != nil {
		return err
	}
	if pod == "" {
		return nil
	}
	tail := int64(100)
	rc, err := d.cfg.Client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: containerName,
		Follow:    true,
		TailLines: &tail,
	}).Stream(ctx)
	if err != nil {
		return blind("stream logs for "+ref.Name, err)
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	// A log line can be long. The default 64KiB token limit turns one long line
	// into a stream that stops with a "token too long" error, which reads as a
	// dead pod.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if emit != nil {
			emit(sc.Text())
		}
	}
	return sc.Err()
}

func short(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	if fp == "" {
		return "(none)"
	}
	return fp
}
