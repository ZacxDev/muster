package k8s_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/k8s"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

func strptr(s string) *string { return &s }

// newDriver builds the driver over a fake clientset, with the settings the
// contract suite's "New" hook uses.
func newDriver(t *testing.T, mutate func(*k8s.Config)) (*k8s.Driver, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	cfg := k8s.Config{
		Client:                cs,
		NamespacePerInstance:  true,
		WorkspaceStorageClass: strptr(""),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	d, err := k8s.New(cfg)
	if err != nil {
		t.Fatalf("k8s.New: %v", err)
	}
	return d, cs
}

// grantablePolicy is a policy THIS driver can interpret. The contract suite
// needs one per driver because a policy's rules are driver-specific by design.
var grantablePolicy = provision.Policy{
	Name: "contract-read-nodes",
	Rules: mustJSON(k8s.Rules{
		ClusterRules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "list"},
		}},
		NamespaceRules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"get"},
		}},
	}),
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestKubernetesSatisfiesTheContract is the point of the whole exercise: the
// same suite the in-memory driver passes, against a driver that renders real
// Kubernetes objects.
//
// 🔴 IT RUNS AGAINST A FAKE CLIENTSET AND NEEDS NO CLUSTER. What that
// structurally CANNOT see is stated in the package's own test for it —
// scheduling, image pulls, volume attachment, admission and the exec stream are
// all outside a fake's reach.
func TestKubernetesSatisfiesTheContract(t *testing.T) {
	provisiontest.RunContract(t, provisiontest.Harness{
		Name: "kubernetes",
		New: func(t *testing.T) provision.Provisioner {
			d, _ := newDriver(t, nil)
			return d
		},
		Blind: func(t *testing.T) provision.Provisioner {
			d, _ := newDriver(t, func(c *k8s.Config) { c.Client = brokenClient() })
			return d
		},
		Restricted: func(t *testing.T) provision.Provisioner {
			// The driver's genuinely minimal configuration: no storage class it
			// may use, no RBAC permission, no exec transport. Persistence is
			// the capability it then refuses.
			d, _ := newDriver(t, func(c *k8s.Config) {
				c.WorkspaceStorageClass = nil
				c.PolicyDisabled = true
			})
			return d
		},
		GrantablePolicy: grantablePolicy,
	})
}

// brokenClient is a clientset whose every call fails, standing in for an
// unreachable apiserver.
func brokenClient() kubernetes.Interface {
	cs := fake.NewClientset()
	cs.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp: connection refused")
	})
	return cs
}

// --------------------------------------------------------------------------
// Capabilities follow configuration
// --------------------------------------------------------------------------

// TestCapabilitiesFollowConfiguration is what makes Config's pointer fields
// worth their awkwardness: each one CHANGES a declared capability, and the
// capability is what CheckSpec and provision.Grant branch on.
func TestCapabilitiesFollowConfiguration(t *testing.T) {
	full, _ := newDriver(t, func(c *k8s.Config) {
		c.RESTConfig = &rest.Config{Host: "https://cluster.example.test"}
	})
	caps := full.Capabilities()
	if !caps.Persistence {
		t.Error("a configured storage class must give Persistence")
	}
	if !caps.Policy {
		t.Error("Policy is on unless PolicyDisabled")
	}
	if !caps.Exec {
		t.Error("a RESTConfig must give Exec")
	}
	if caps.Isolation != provision.IsolationNamespace {
		t.Errorf("namespace-per-instance is IsolationNamespace, got %v", caps.Isolation)
	}

	// 🔴 nil AND "" ARE DIFFERENT ANSWERS. This is the whole reason the field
	// is a pointer, and the only place in the package where the distinction is
	// observable.
	none, _ := newDriver(t, func(c *k8s.Config) { c.WorkspaceStorageClass = nil })
	if none.Capabilities().Persistence {
		t.Error("a nil storage class means no persistence; it must not be read as the default class")
	}
	defaultSC, _ := newDriver(t, func(c *k8s.Config) { c.WorkspaceStorageClass = strptr("") })
	if !defaultSC.Capabilities().Persistence {
		t.Error("an EMPTY storage class means the cluster default, which IS persistence")
	}

	noPolicy, _ := newDriver(t, func(c *k8s.Config) { c.PolicyDisabled = true })
	if noPolicy.Capabilities().Policy {
		t.Error("PolicyDisabled must turn Policy off")
	}
	// And the declaration must actually gate the grant.
	if err := provision.Grant(context.Background(), noPolicy, provision.Ref{Name: "x"}, grantablePolicy); !errors.Is(err, provision.ErrUnsupported) {
		t.Errorf("PolicyDisabled must make Grant refuse, got %v", err)
	}

	shared, _ := newDriver(t, func(c *k8s.Config) {
		c.NamespacePerInstance = false
		c.Namespace = "agents"
	})
	if shared.Capabilities().Isolation != provision.IsolationContainer {
		t.Error("a shared namespace is container isolation, not namespace isolation")
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	if _, err := k8s.New(k8s.Config{}); err == nil {
		t.Error("a nil Client must be refused at construction")
	}
	if _, err := k8s.New(k8s.Config{Client: fake.NewClientset()}); err == nil {
		t.Error("a shared-namespace driver with no namespace must be refused: guessing 'default' would " +
			"put agents somewhere nobody chose")
	}
	if _, err := k8s.New(k8s.Config{Client: fake.NewClientset(), NamespacePerInstance: true, EndpointTemplate: "{{.Name"}); err == nil {
		t.Error("an unparseable endpoint template must fail at construction, not per request")
	}
}

// TestRefusesACommandOnlyRuntime pins the one backend-specific requirement.
// Runtime is a union so a process driver is expressible; this driver is the
// half that needs an image.
func TestRefusesACommandOnlyRuntime(t *testing.T) {
	d, _ := newDriver(t, nil)
	spec := provisiontest.MinimalSpec("commandonly")
	spec.Runtime = provision.Runtime{Command: []string{"/usr/bin/agent"}}
	err := d.Create(context.Background(), spec)
	if !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	if !strings.Contains(err.Error(), "Runtime.Image") {
		t.Fatalf("the refusal must name the field, got %q", err)
	}
}

// --------------------------------------------------------------------------
// What gets rendered
// --------------------------------------------------------------------------

// richSpec exercises every field the renderer touches, with pairwise distinct
// values so an assertion cannot pass by two fields happening to agree.
func richSpec(name string) provision.Spec {
	s := provisiontest.MinimalSpec(name)
	s.Ref.ID = 4217
	s.Runtime.Args = []string{"--serve"}
	s.Runtime.WorkingDir = "/data/workspace"
	s.Env = []provision.EnvVar{{Name: "MUSTER_ROLE", Value: "worker"}}
	s.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "token-value-19731"}}
	s.Files = []provision.File{
		{Path: "/etc/muster/agent.json", Content: []byte(`{"tools":["tasks"]}`)},
		{Path: "/root/.config/creds", Content: []byte("credential-content-8421"), Secret: true},
	}
	s.Init = []string{"mkdir -p /data/workspace/cache"}
	s.Resources = provision.Resources{CPURequest: "250m", CPULimit: "2", MemoryRequest: "512Mi", MemoryLimit: "3Gi"}
	s.Workspace = provision.Workspace{Path: "/data/workspace", Size: "7Gi", Persist: true}
	s.Repo = provision.Repo{URL: "https://git.example.test/org/repo.git", Branch: "trunk", Path: "/data/workspace/repo"}
	s.Config = map[string]any{"model": "some-model"}
	s.Ports = []provision.Port{{Name: provision.DefaultPortName, Port: 8421}, {Name: "metrics", Port: 9102}}
	s.Labels = map[string]string{"team": "ops"}
	return s
}

func TestRendersTheMinimalObjectSet(t *testing.T) {
	d, cs := newDriver(t, nil)
	ctx := context.Background()
	spec := richSpec("rich")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ns := "muster-rich"

	if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if _, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "rich", metav1.GetOptions{}); err != nil {
		t.Fatalf("serviceaccount: %v", err)
	}
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "rich-files", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("configmap: %v", err)
	}
	if len(cm.BinaryData) != 1 {
		t.Errorf("configmap holds %d keys, want 1 (the non-secret file only)", len(cm.BinaryData))
	}
	sec, err := cs.CoreV1().Secrets(ns).Get(ctx, "rich-env", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	if len(sec.Data) != 2 {
		t.Errorf("secret holds %d keys, want 2 (one env var, one secret file)", len(sec.Data))
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "rich-workspace", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "7Gi" {
		t.Errorf("pvc size %q, want 7Gi", got)
	}
	svc, err := cs.CoreV1().Services(ns).Get(ctx, "rich", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if len(svc.Spec.Ports) != 2 {
		t.Errorf("service has %d ports, want 2", len(svc.Spec.Ports))
	}

	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "rich", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	pod := dep.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Errorf("deployment has %d containers, want exactly 1 — Capabilities.Sidecars is false", len(pod.Containers))
	}
	if len(pod.InitContainers) != 1 {
		t.Fatalf("deployment has %d init containers, want 1 (Spec.Init is non-empty)", len(pod.InitContainers))
	}
	init := pod.InitContainers[0]
	// 🔴 `sh -eu`, NOT a wrapper that swallows failures. The whole reason
	// Spec.Files exists is so that Init holds only required, imperative steps —
	// and a required step that fails must stop the pod.
	if len(init.Command) < 2 || init.Command[0] != "sh" || init.Command[1] != "-eu" {
		t.Errorf("init container command is %v, want it to start with [sh -eu]", init.Command)
	}
	c := pod.Containers[0]
	if c.Resources.Limits.Memory().String() != "3Gi" || c.Resources.Requests.Cpu().String() != "250m" {
		t.Errorf("resources not rendered: %+v", c.Resources)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "rich-env" {
		t.Errorf("secrets must arrive by envFrom, got %+v", c.EnvFrom)
	}

	envByName := map[string]string{}
	for _, e := range c.Env {
		envByName[e.Name] = e.Value
	}
	for k, want := range map[string]string{
		"MUSTER_ROLE":        "worker",
		"MUSTER_REPO_URL":    "https://git.example.test/org/repo.git",
		"MUSTER_REPO_BRANCH": "trunk",
		"MUSTER_REPO_PATH":   "/data/workspace/repo",
		"MUSTER_CONFIG":      `{"model":"some-model"}`,
	} {
		if envByName[k] != want {
			t.Errorf("env %s = %q, want %q", k, envByName[k], want)
		}
	}

	// The operator's own label survives; the driver's selector labels win.
	if dep.Labels["team"] != "ops" {
		t.Error("the operator's label was dropped")
	}
	if dep.Labels["app.kubernetes.io/instance"] != "rich" {
		t.Error("the driver's instance label is missing")
	}
}

// TestFilesArePlacedNativelyAtTheirPaths is the replacement for the base64
// shell coupling: each file is a subPath mount at its exact absolute path, with
// its own mode.
func TestFilesArePlacedNativelyAtTheirPaths(t *testing.T) {
	d, cs := newDriver(t, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("files")
	spec.Files = []provision.File{
		{Path: "/etc/muster/agent.json", Content: []byte(`{"a":1}`)},
		{Path: "/usr/local/bin/hook", Content: []byte("#!/bin/sh\n"), Mode: 0o755},
		{Path: "/root/.config/creds", Content: []byte("secret-bytes"), Secret: true},
	}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dep, err := cs.AppsV1().Deployments("muster-files").Get(ctx, "files", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	c := dep.Spec.Template.Spec.Containers[0]

	mountByPath := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mountByPath[m.MountPath] = m
	}
	for _, want := range []string{"/etc/muster/agent.json", "/usr/local/bin/hook", "/root/.config/creds"} {
		m, ok := mountByPath[want]
		if !ok {
			t.Fatalf("no volumeMount at %q; the file is not placed where the spec says", want)
		}
		// 🔴 SUBPATH IS WHAT PLACES A SINGLE FILE AT AN ABSOLUTE PATH. Without
		// it the mount turns the file's DIRECTORY into a volume and everything
		// else in that directory disappears — /usr/local/bin being the obvious
		// disaster.
		if m.SubPath == "" {
			t.Errorf("mount at %q has no subPath; it would replace the whole directory", want)
		}
	}
	// Confidential and non-confidential files come from different volumes.
	if mountByPath["/root/.config/creds"].Name == mountByPath["/etc/muster/agent.json"].Name {
		t.Error("a secret file and a plain file share a volume; one of them is in the wrong store")
	}

	volByName := map[string]corev1.Volume{}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		volByName[v.Name] = v
	}
	plain := volByName[mountByPath["/usr/local/bin/hook"].Name]
	if plain.ConfigMap == nil {
		t.Fatal("the plain-file volume is not a ConfigMap")
	}
	var found bool
	for _, item := range plain.ConfigMap.Items {
		if item.Key == mountByPath["/usr/local/bin/hook"].SubPath {
			found = true
			if item.Mode == nil || *item.Mode != 0o755 {
				t.Errorf("the executable file's mode is %v, want 0755", item.Mode)
			}
		}
	}
	if !found {
		t.Error("the executable file has no item entry, so it cannot carry a mode")
	}

	// The default modes differ between a plain and a secret file, and that
	// difference must survive the render.
	secretVol := volByName[mountByPath["/root/.config/creds"].Name]
	if secretVol.Secret == nil {
		t.Fatal("the secret-file volume is not a Secret")
	}
	for _, item := range secretVol.Secret.Items {
		if item.Mode == nil || *item.Mode != 0o600 {
			t.Errorf("a secret file defaulted to mode %v, want 0600", item.Mode)
		}
	}
}

// TestFileKeysDoNotDependOnOrdering — a key derived from a file's POSITION
// would make reordering Spec.Files rewrite every key and roll the pod for no
// reason.
func TestFileKeysDoNotDependOnOrdering(t *testing.T) {
	ctx := context.Background()
	a := provisiontest.MinimalSpec("ordered")
	a.Files = []provision.File{
		{Path: "/etc/muster/one.json", Content: []byte("1")},
		{Path: "/etc/muster/two.json", Content: []byte("2")},
	}
	b := a
	b.Files = []provision.File{a.Files[1], a.Files[0]}

	keysOf := func(spec provision.Spec) []string {
		d, cs := newDriver(t, nil)
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		cm, err := cs.CoreV1().ConfigMaps("muster-ordered").Get(ctx, "ordered-files", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get configmap: %v", err)
		}
		out := make([]string, 0, len(cm.BinaryData))
		for k := range cm.BinaryData {
			out = append(out, k)
		}
		sortStrings(out)
		return out
	}
	ka, kb := keysOf(a), keysOf(b)
	if strings.Join(ka, ",") != strings.Join(kb, ",") {
		t.Fatalf("reordering Spec.Files changed the ConfigMap keys:\n  %v\n  %v", ka, kb)
	}
	// Positive control: the keys are not all identical to begin with.
	if len(ka) != 2 || ka[0] == ka[1] {
		t.Fatalf("control: expected two distinct keys, got %v", ka)
	}
	// And the fingerprint must agree, or a reorder rebuilds the instance.
	if provision.Fingerprint(a) != provision.Fingerprint(b) {
		t.Fatal("reordering Spec.Files moved the fingerprint")
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// TestRestartReasonSurvivesTheRestartThatHidesIt is the port of the single most
// load-bearing field on Instance.
//
// The fixture is the exact state the field exists for: a container that is
// RUNNING and READY, with a bumped restart count, whose last termination was an
// out-of-memory kill. Every other field says the instance is healthy.
func TestRestartReasonSurvivesTheRestartThatHidesIt(t *testing.T) {
	d, cs := newDriver(t, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("oomer")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ns := "muster-oomer"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oomer-7d4c8",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "muster-agent",
				"app.kubernetes.io/instance":   "oomer",
				"app.kubernetes.io/managed-by": "muster",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "agent",
				Ready:        true,
				RestartCount: 3,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
				},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	inst, err := d.Get(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if inst.Phase != provision.PhaseRunning || !inst.Ready {
		t.Fatalf("control: the pod must read healthy in every other field, got phase=%q ready=%t", inst.Phase, inst.Ready)
	}
	if inst.Restarts != 3 {
		t.Errorf("Restarts = %d, want 3", inst.Restarts)
	}
	if inst.RestartReason != "OOMKilled" {
		t.Fatalf("RestartReason = %q, want %q. It is the ONLY field that says why a running, ready "+
			"instance last died — without it, an agent too small for its repository is indistinguishable "+
			"from a healthy one.", inst.RestartReason, "OOMKilled")
	}
	// Reason is the WAITING reason and must stay distinct: folding the two
	// makes a current problem and a past one the same field.
	if inst.Reason != "" {
		t.Errorf("Reason = %q, want empty — the container is not waiting", inst.Reason)
	}
	if inst.InstanceID != "oomer-7d4c8" {
		t.Errorf("InstanceID = %q, want the pod name", inst.InstanceID)
	}
	if inst.Ref.ID != 0 {
		t.Errorf("Ref.ID = %d, want 0 — MinimalSpec sets none and none must be invented", inst.Ref.ID)
	}
}

// TestCorrelationIDRoundTripsWhenSet is the other half: an id that WAS supplied
// comes back.
func TestCorrelationIDRoundTripsWhenSet(t *testing.T) {
	d, _ := newDriver(t, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("correlated")
	spec.Ref.ID = 4217
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	inst, err := d.Get(ctx, provision.Ref{Name: "correlated"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if inst.Ref.ID != 4217 {
		t.Fatalf("Ref.ID = %d, want 4217", inst.Ref.ID)
	}
}

// TestDeploymentStrategyDependsOnTheWorkspace pins a choice whose failure mode
// reads as a cluster problem rather than a strategy one.
func TestDeploymentStrategyDependsOnTheWorkspace(t *testing.T) {
	ctx := context.Background()

	d, cs := newDriver(t, nil)
	persistent := provisiontest.MinimalSpec("stateful")
	persistent.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
	if err := d.Create(ctx, persistent); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dep, err := cs.AppsV1().Deployments("muster-stateful").Get(ctx, "stateful", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("a persistent workspace must use Recreate (a ReadWriteOnce volume deadlocks a rolling "+
			"update), got %q", dep.Spec.Strategy.Type)
	}

	stateless := provisiontest.MinimalSpec("stateless")
	if err := d.Create(ctx, stateless); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dep, err = cs.AppsV1().Deployments("muster-stateless").Get(ctx, "stateless", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatalf("a stateless instance must roll, got %q", dep.Spec.Strategy.Type)
	}
	if dep.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue() != 0 {
		t.Error("maxUnavailable must be 0 so a roll is not an outage")
	}
}

// TestSecretValuesNeverAppearInThePodSpec. A Deployment is readable by anything
// that can read the namespace; a Secret is not.
func TestSecretValuesNeverAppearInThePodSpec(t *testing.T) {
	const secretValue = "token-value-19731"
	d, cs := newDriver(t, nil)
	ctx := context.Background()
	spec := richSpec("confidential")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dep, err := cs.AppsV1().Deployments("muster-confidential").Get(ctx, "confidential", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	b, err := json.Marshal(dep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), secretValue) {
		t.Fatal("a secret value appears in the Deployment object")
	}
	if strings.Contains(string(b), "credential-content-8421") {
		t.Fatal("a secret FILE's content appears in the Deployment object")
	}
	// Positive control: the value IS somewhere, so the absence above is
	// placement and not omission.
	sec, err := cs.CoreV1().Secrets("muster-confidential").Get(ctx, "confidential-env", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(sec.Data["MUSTER_CALLBACK_TOKEN"]) != secretValue {
		t.Fatalf("the secret value is not in the Secret either: %q", sec.Data["MUSTER_CALLBACK_TOKEN"])
	}
}

// TestUpdateReplacesTheSpecAndCreateIsThenIdempotent — the reconcile loop's
// actual sequence.
func TestUpdateReplacesTheSpecAndCreateIsThenIdempotent(t *testing.T) {
	d, cs := newDriver(t, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("rolling")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, _ := cs.AppsV1().Deployments("muster-rolling").Get(ctx, "rolling", metav1.GetOptions{})

	changed := spec
	changed.Runtime.Image = "ghcr.io/muster-example/rolled:3"
	if err := d.Update(ctx, changed); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := cs.AppsV1().Deployments("muster-rolling").Get(ctx, "rolling", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Spec.Template.Spec.Containers[0].Image != "ghcr.io/muster-example/rolled:3" {
		t.Fatalf("Update did not change the image: %q", after.Spec.Template.Spec.Containers[0].Image)
	}
	if before.Annotations["muster.dev/spec-fingerprint"] == after.Annotations["muster.dev/spec-fingerprint"] {
		t.Fatal("the recorded fingerprint did not move; the next Create would see divergence forever")
	}
	if err := d.Create(ctx, changed); err != nil {
		t.Fatalf("Create after Update must be idempotent: %v", err)
	}
}

// TestDestroyVerifiesRatherThanAssumes is the mutation-proof form of contract
// rule 2. The reactor makes DELETE succeed while removing nothing — exactly
// what a finalizer, a webhook or a permission quirk can do — and Destroy must
// NOT report success.
func TestDestroyVerifiesRatherThanAssumes(t *testing.T) {
	d, cs := newDriver(t, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("stubborn")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Control: without the reactor, Destroy succeeds. Without this the case
	// could pass because Destroy always fails.
	d2, _ := newDriver(t, nil)
	if err := d2.Create(ctx, spec); err != nil {
		t.Fatalf("control Create: %v", err)
	}
	if err := d2.Destroy(ctx, spec.Ref); err != nil {
		t.Fatalf("control: Destroy must succeed on a normal backend, got %v", err)
	}

	cs.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		// "Accepted" — and nothing is removed.
		return true, nil, nil
	})
	err := d.Destroy(ctx, spec.Ref)
	if err == nil {
		t.Fatal("Destroy returned nil while the deployment survived. nil means removed or already absent.")
	}
	if !strings.Contains(err.Error(), "still present") {
		t.Fatalf("the error must say what it found, got %q", err)
	}
}

// TestDestroyRemovesClusterScopedPolicyObjects. They outlive the namespace, the
// Deployment and any database row, and the next instance to take this name gets
// the same ServiceAccount — the dangling binding's exact subject.
func TestDestroyRemovesClusterScopedPolicyObjects(t *testing.T) {
	d, cs := newDriver(t, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("recycled")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := provision.Grant(ctx, d, spec.Ref, grantablePolicy); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	name := "muster-recycled-contract-read-nodes"
	if _, err := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("control: the clusterrole must exist before the destroy: %v", err)
	}

	if err := d.Destroy(ctx, spec.Ref); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Error("the ClusterRole survived the destroy; a future namesake inherits access nobody granted")
	}
	if _, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Error("the ClusterRoleBinding survived the destroy")
	}
}

// --------------------------------------------------------------------------
// Policy
// --------------------------------------------------------------------------

func TestGrantAppliesRBAC(t *testing.T) {
	d, cs := newDriver(t, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("privileged")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := provision.Grant(ctx, d, spec.Ref, grantablePolicy); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	name := "muster-privileged-contract-read-nodes"
	cr, err := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("clusterrole: %v", err)
	}
	if len(cr.Rules) != 1 || cr.Rules[0].Resources[0] != "nodes" {
		t.Errorf("clusterrole rules are %+v", cr.Rules)
	}
	crb, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("clusterrolebinding: %v", err)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Name != "privileged" || crb.Subjects[0].Namespace != "muster-privileged" {
		t.Errorf("binding subject is %+v; it must be the instance's own ServiceAccount", crb.Subjects)
	}
	role, err := cs.RbacV1().Roles("muster-privileged").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	if role.Rules[0].Resources[0] != "configmaps" {
		t.Errorf("role rules are %+v", role.Rules)
	}
	if _, err := cs.RbacV1().RoleBindings("muster-privileged").Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("rolebinding: %v", err)
	}
}

// TestGrantRefusesWhatItCannotApply walks each driver-side refusal, each
// reached with a policy that is otherwise fine.
//
// MUTATION MATRIX (each applied alone):
//
//	delete the Env/Files refusal        -> "env or files" RED
//	delete the HasRules refusal         -> "no rules" RED
//	drop DisallowUnknownFields          -> "unknown field" RED
//	delete the empty-rules refusal      -> "parses but is empty" RED
//	delete the ServiceAccount existence -> "no such instance" RED
func TestGrantRefusesWhatItCannotApply(t *testing.T) {
	ctx := context.Background()
	d, client := newDriver(t, nil)
	cs := func() *fake.Clientset { return client }
	spec := provisiontest.MinimalSpec("refuser")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Control: the driver DOES grant a policy it understands, so every refusal
	// below is about the policy rather than about the driver.
	if err := provision.Grant(ctx, d, spec.Ref, grantablePolicy); err != nil {
		t.Fatalf("control: %v", err)
	}

	cases := []struct {
		name    string
		pol     provision.Policy
		mustSay string
	}{
		{
			name: "env or files",
			pol: provision.Policy{
				Name:  "with-env",
				Rules: grantablePolicy.Rules,
				Env:   []provision.EnvVar{{Name: "K", Value: "v"}},
			},
			mustSay: "env var(s)",
		},
		{
			name:    "no rules",
			pol:     provision.Policy{Name: "empty", Rules: []byte(`{}`)},
			mustSay: "carries no rules",
		},
		{
			name:    "unknown field",
			pol:     provision.Policy{Name: "alien", Rules: []byte(`{"dockerCapabilities":["NET_ADMIN"]}`)},
			mustSay: "cannot interpret",
		},
		{
			name:    "parses but is empty",
			pol:     provision.Policy{Name: "hollow", Rules: []byte(`{"clusterRules":[]}`)},
			mustSay: "declares neither",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := provision.Grant(ctx, d, spec.Ref, c.pol)
			if !errors.Is(err, provision.ErrUnsupported) {
				t.Fatalf("want ErrUnsupported, got %v", err)
			}
			if !strings.Contains(err.Error(), c.mustSay) {
				t.Fatalf("the refusal must name the reason: want %q in %q", c.mustSay, err)
			}
		})
	}

	// 🔴 THE CASE A MUTATION SWEEP FOUND, AND THE ONLY ONE THAT REACHES THE
	// STRICT PARSER ON ITS OWN. A payload made ENTIRELY of unknown fields is
	// refused even by a lenient parser, because it decodes to an empty Rules
	// and the empty-rules guard catches it — so the "unknown field" case above
	// does not, by itself, prove DisallowUnknownFields is doing anything. This
	// one does: it carries rules this driver understands AND a field it does
	// not, so a lenient parser applies half a policy and reports the whole
	// thing granted. Measured: removing DisallowUnknownFields leaves the case
	// above passing and turns this one red.
	t.Run("understood in part is still refused", func(t *testing.T) {
		partly := provision.Policy{
			Name: "half-alien",
			Rules: []byte(`{"clusterRules":[{"apiGroups":[""],"resources":["nodes"],"verbs":["get"]}],` +
				`"dockerCapabilities":["NET_ADMIN"]}`),
		}
		err := provision.Grant(ctx, d, spec.Ref, partly)
		if !errors.Is(err, provision.ErrUnsupported) {
			t.Fatalf("a policy carrying rules this driver understands AND a field it does not must be "+
				"REFUSED, not half-applied; got %v", err)
		}
		if !strings.Contains(err.Error(), "cannot interpret") {
			t.Fatalf("the refusal must name the reason, got %q", err)
		}
		// And nothing may have been applied.
		if _, gerr := cs().RbacV1().ClusterRoles().Get(ctx, "muster-refuser-half-alien", metav1.GetOptions{}); gerr == nil {
			t.Fatal("the refused policy created a ClusterRole anyway")
		}
	})

	t.Run("no such instance", func(t *testing.T) {
		err := provision.Grant(ctx, d, provision.Ref{Name: "never-created"}, grantablePolicy)
		if !errors.Is(err, provision.ErrNotFound) {
			t.Fatalf("granting to an instance that does not exist creates a binding a future namesake "+
				"would inherit; want ErrNotFound, got %v", err)
		}
	})
}

func TestRevokeOfAnUngrantedPolicyIsNil(t *testing.T) {
	d, _ := newDriver(t, nil)
	if err := d.Revoke(context.Background(), provision.Ref{Name: "nobody"}, "never-granted"); err != nil {
		t.Fatalf("Revoke of an absent grant must be nil, got %v", err)
	}
}

// --------------------------------------------------------------------------
// Endpoint
// --------------------------------------------------------------------------

func TestEndpointTemplateIsConfigurable(t *testing.T) {
	ctx := context.Background()
	d, _ := newDriver(t, func(c *k8s.Config) {
		c.EndpointTemplate = "{{.Name}}.agents.example.test"
		c.EndpointScheme = "https"
	})
	spec := provisiontest.MinimalSpec("reachable")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ep, err := d.Endpoint(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep.Host != "reachable.agents.example.test" || ep.Scheme != "https" || ep.Port != 8421 {
		t.Fatalf("the configured template was not used: %+v", ep)
	}

	// The default, for comparison — and to prove the case above is measuring a
	// change rather than a coincidence.
	dflt, _ := newDriver(t, nil)
	if err := dflt.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ep2, err := dflt.Endpoint(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep2.Host != "reachable.muster-reachable.svc" {
		t.Fatalf("default endpoint host is %q", ep2.Host)
	}
	if ep2.Host == ep.Host {
		t.Fatal("the configured and default hosts are identical; this case could not tell them apart")
	}
}

func TestEndpointOfAPortlessInstance(t *testing.T) {
	d, _ := newDriver(t, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("portless")
	spec.Ports = nil
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := d.Endpoint(ctx, spec.Ref)
	if !errors.Is(err, provision.ErrNoEndpoint) {
		t.Fatalf("an instance with no ports must be ErrNoEndpoint — distinct from ErrNotFound (it exists) "+
			"and from ErrBlind (we can see it); got %v", err)
	}
}

// --------------------------------------------------------------------------
// Logs
// --------------------------------------------------------------------------

func TestTailLogsOfAnInstanceWithNoPodYet(t *testing.T) {
	d, _ := newDriver(t, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("nopod")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	out, err := d.TailLogs(ctx, spec.Ref, 10)
	if err != nil {
		t.Fatalf("an instance with no pod yet is not a failure: %v", err)
	}
	if out != "" {
		t.Fatalf("want empty output, got %q", out)
	}
}
