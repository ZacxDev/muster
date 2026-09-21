package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/ZacxDev/muster/internal/provision"
)

// The label and annotation keys this driver writes.
//
// 🔴 `managed-by` IS THE SELECTOR List USES, AND IT IS WHAT KEEPS THIS DRIVER
// FROM REPORTING — OR DESTROYING — WORKLOADS IT DID NOT CREATE. Every object
// rendered here carries it; nothing is enumerated without it.
const (
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"

	managedBy  = "muster"
	nameValue  = "muster-agent"
	fieldOwner = "muster"

	// annFingerprint records provision.Fingerprint(spec) so Create can answer
	// the divergence question without storing the spec anywhere else.
	annFingerprint = "muster.dev/spec-fingerprint"
	// annCorrelationID records Ref.ID, so an operator reading the object can
	// find the row it came from. It is written only when non-zero — an
	// annotation reading "0" is a value, and this one means "there was none".
	annCorrelationID = "muster.dev/correlation-id"
	// annEndpoint records the per-instance Spec.Endpoint override as JSON, so
	// Endpoint() can honour it later without the spec.
	annEndpoint = "muster.dev/endpoint"
	// annPort records the resolved reachable port, for the same reason.
	annPort = "muster.dev/port"
)

// containerName is the single application container's name. It is a CONSTANT
// so that every log read, exec and status inspection names the same container
// explicitly — the project this replaces never named one, so every one of those
// operations silently read whichever container happened to be first.
const containerName = "agent"

// initContainerName runs Spec.Init.
const initContainerName = "muster-init"

// workspaceVolumeName is the shared volume the init container and the
// application container both see. It exists even for a non-persistent
// workspace, as an emptyDir, because otherwise an init step's output would not
// survive into the container that needs it — which is the single most common
// way an init container is written and silently does nothing.
const workspaceVolumeName = "workspace"

const (
	filesVolumeName       = "muster-files"
	secretFilesVolumeName = "muster-secret-files"
)

// DefaultWorkspacePath is where a workspace is mounted when the spec does not
// say.
const DefaultWorkspacePath = "/data/workspace"

// DefaultEndpointTemplate is the in-cluster address of an instance's Service.
//
// It is a DEFAULT, not a constant in the code path: Config.EndpointTemplate
// replaces it wholesale and Spec.Endpoint overrides it per instance. The string
// it replaces in the original project was reachable from nowhere.
const DefaultEndpointTemplate = "{{.Name}}.{{.Group}}.svc.cluster.local"

func instanceLabels(name string) map[string]string {
	return map[string]string{
		labelName:      nameValue,
		labelInstance:  name,
		labelManagedBy: managedBy,
	}
}

// managedSelector is the label selector every enumeration uses.
func managedSelector() string {
	return labelManagedBy + "=" + managedBy + "," + labelName + "=" + nameValue
}

func instanceSelector(name string) string {
	return managedSelector() + "," + labelInstance + "=" + name
}

// mergeLabels folds the operator's own labels under the driver's.
//
// ⚠ THE DRIVER'S LABELS WIN. They are the selector; letting a caller overwrite
// `app.kubernetes.io/instance` would point one instance's Deployment at another
// instance's pods, and the Deployment controller would happily adopt them.
func mergeLabels(own map[string]string, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range own {
		out[k] = v
	}
	return out
}

// fileKey is the ConfigMap/Secret key holding one file's content.
//
// It is derived from the PATH, not from the file's position in the list, so
// reordering Spec.Files does not rewrite every key and roll the pod. The digest
// prefix makes it collision-free; the sanitised basename is there so a human
// reading `kubectl describe` can tell which file it is.
func fileKey(p string) string {
	sum := sha256.Sum256([]byte(p))
	base := path.Base(p)
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := b.String()
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	const maxBase = 40
	if len(name) > maxBase {
		name = name[:maxBase]
	}
	return hex.EncodeToString(sum[:6]) + "-" + name
}

// sortedFiles returns the spec's files split into non-secret and secret,
// each sorted by path so every render is byte-stable.
func sortedFiles(files []provision.File) (plain, secret []provision.File) {
	for _, f := range files {
		if f.Secret {
			secret = append(secret, f)
		} else {
			plain = append(plain, f)
		}
	}
	sort.Slice(plain, func(i, j int) bool { return plain[i].Path < plain[j].Path })
	sort.Slice(secret, func(i, j int) bool { return secret[i].Path < secret[j].Path })
	return plain, secret
}

// --------------------------------------------------------------------------
// Object rendering. Each function is pure: spec in, object out. That is what
// makes the manifest set testable without a cluster, which is the property the
// chart it replaces did not have.
// --------------------------------------------------------------------------

func (d *Driver) renderServiceAccount(spec provision.Spec, ns string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Ref.Name,
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
	}
}

// renderConfigMap holds the non-secret files. Returns nil when there are none,
// so the driver does not create an empty object it would then have to delete.
func (d *Driver) renderConfigMap(spec provision.Spec, ns string) *corev1.ConfigMap {
	plain, _ := sortedFiles(spec.Files)
	if len(plain) == 0 {
		return nil
	}
	data := map[string][]byte{}
	for _, f := range plain {
		data[fileKey(f.Path)] = f.Content
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Ref.Name + "-files",
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		// BinaryData, not Data: provision.File carries bytes, and Data is a
		// string map that silently mangles anything that is not valid UTF-8.
		BinaryData: data,
	}
}

// renderSecret holds confidential environment AND confidential files.
func (d *Driver) renderSecret(spec provision.Spec, ns string) *corev1.Secret {
	_, secretFiles := sortedFiles(spec.Files)
	if len(spec.Secrets) == 0 && len(secretFiles) == 0 {
		return nil
	}
	data := map[string][]byte{}
	for _, e := range spec.Secrets {
		data[e.Name] = []byte(e.Value)
	}
	for _, f := range secretFiles {
		data[fileKey(f.Path)] = f.Content
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Ref.Name + "-env",
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

func (d *Driver) renderPVC(spec provision.Spec, ns string) (*corev1.PersistentVolumeClaim, error) {
	if !spec.Workspace.Persist {
		return nil, nil
	}
	size := spec.Workspace.Size
	if size == "" {
		size = "10Gi"
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace size %q: %v", provision.ErrInvalidSpec, size, err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Ref.Name + "-workspace",
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	// 🔴 A NIL STORAGE CLASS AND AN EMPTY ONE ARE DIFFERENT REQUESTS, AND THIS
	// IS THE ONE PLACE THE DISTINCTION IS LOAD-BEARING. Empty string means "use
	// the cluster's default StorageClass"; leaving the field unset entirely
	// means the same thing on most clusters but is expressed differently in the
	// object. Config.WorkspaceStorageClass is a *string precisely so an
	// operator can say "" and mean the default, rather than having "" mean "I
	// did not configure this" — the distinction nothing in the original project
	// could make anywhere.
	if d.cfg.WorkspaceStorageClass != nil && *d.cfg.WorkspaceStorageClass != "" {
		sc := *d.cfg.WorkspaceStorageClass
		pvc.Spec.StorageClassName = &sc
	}
	return pvc, nil
}

func (d *Driver) renderService(spec provision.Spec, ns string) *corev1.Service {
	if len(spec.Ports) == 0 {
		return nil
	}
	ports := make([]corev1.ServicePort, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		proto := corev1.ProtocolTCP
		if strings.EqualFold(p.Protocol, "UDP") {
			proto = corev1.ProtocolUDP
		}
		name := p.Name
		if name == "" {
			name = "port-" + strconv.Itoa(p.Port)
		}
		ports = append(ports, corev1.ServicePort{
			Name:       name,
			Port:       int32(p.Port),
			TargetPort: intstr.FromInt32(int32(p.Port)),
			Protocol:   proto,
		})
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Ref.Name,
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		Spec: corev1.ServiceSpec{
			Selector: instanceLabels(spec.Ref.Name),
			Ports:    ports,
		},
	}
}

func (d *Driver) renderDeployment(spec provision.Spec, ns string) (*appsv1.Deployment, error) {
	labels := mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels)

	env := make([]corev1.EnvVar, 0, len(spec.Env)+4)
	for _, e := range spec.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	// The repository is DECLARED here, not cloned. See the package doc.
	if spec.Repo.URL != "" {
		env = append(env, corev1.EnvVar{Name: "MUSTER_REPO_URL", Value: spec.Repo.URL})
		if spec.Repo.Branch != "" {
			env = append(env, corev1.EnvVar{Name: "MUSTER_REPO_BRANCH", Value: spec.Repo.Branch})
		}
		if p := spec.Repo.Path; p != "" {
			env = append(env, corev1.EnvVar{Name: "MUSTER_REPO_PATH", Value: p})
		}
	}
	// Config is opaque: it is handed over verbatim as JSON and no key of it is
	// interpreted here.
	if len(spec.Config) > 0 {
		b, err := json.Marshal(spec.Config)
		if err != nil {
			return nil, fmt.Errorf("%w: Spec.Config is not JSON-serialisable: %v", provision.ErrInvalidSpec, err)
		}
		env = append(env, corev1.EnvVar{Name: "MUSTER_CONFIG", Value: string(b)})
	}

	resources, err := renderResources(spec.Resources)
	if err != nil {
		return nil, err
	}

	volumes, mounts := d.renderVolumes(spec)

	wsPath := spec.Workspace.Path
	if wsPath == "" {
		wsPath = DefaultWorkspacePath
	}
	mounts = append(mounts, corev1.VolumeMount{Name: workspaceVolumeName, MountPath: wsPath})

	ports := make([]corev1.ContainerPort, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		proto := corev1.ProtocolTCP
		if strings.EqualFold(p.Protocol, "UDP") {
			proto = corev1.ProtocolUDP
		}
		name := p.Name
		if name == "" {
			name = "port-" + strconv.Itoa(p.Port)
		}
		ports = append(ports, corev1.ContainerPort{Name: name, ContainerPort: int32(p.Port), Protocol: proto})
	}

	container := corev1.Container{
		Name:         containerName,
		Image:        spec.Runtime.Image,
		Command:      spec.Runtime.Command,
		Args:         spec.Runtime.Args,
		WorkingDir:   spec.Runtime.WorkingDir,
		Env:          env,
		Ports:        ports,
		Resources:    resources,
		VolumeMounts: mounts,
	}
	if secret := d.renderSecret(spec, ns); secret != nil && len(spec.Secrets) > 0 {
		// envFrom, not one EnvVar per secret key: the values must not appear in
		// the pod spec, which is readable by anything that can read the
		// Deployment.
		container.EnvFrom = append(container.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name},
			},
		})
	}
	podSpec := corev1.PodSpec{
		ServiceAccountName: spec.Ref.Name,
		Containers:         []corev1.Container{container},
		Volumes:            volumes,
	}
	if len(spec.Init) > 0 {
		podSpec.InitContainers = []corev1.Container{d.renderInitContainer(spec, container)}
	}

	replicas := int32(spec.DesiredReplicas())
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.Ref.Name,
			Namespace:   ns,
			Labels:      labels,
			Annotations: d.renderAnnotations(spec),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: instanceLabels(spec.Ref.Name)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{annFingerprint: provision.Fingerprint(spec)},
				},
				Spec: podSpec,
			},
			Strategy: deploymentStrategy(spec),
		},
	}
	return dep, nil
}

// deploymentStrategy picks Recreate for a persistent workspace and
// RollingUpdate otherwise.
//
// 🔴 THE PERSISTENT CASE IS NOT A PREFERENCE. A ReadWriteOnce volume can be
// attached to one node at a time, so a rolling update deadlocks: the new pod
// waits for a volume the old pod will not release until the new pod is ready.
// The symptom is a Deployment stuck in Pending with a scheduling event about
// volume attachment, which reads as a cluster problem rather than a strategy
// one.
func deploymentStrategy(spec provision.Spec) appsv1.DeploymentStrategy {
	if spec.Workspace.Persist {
		return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
}

func (d *Driver) renderAnnotations(spec provision.Spec) map[string]string {
	ann := map[string]string{
		annFingerprint: provision.Fingerprint(spec),
	}
	if spec.Ref.ID != 0 {
		ann[annCorrelationID] = strconv.FormatInt(spec.Ref.ID, 10)
	}
	if spec.Endpoint != nil && !spec.Endpoint.IsZero() {
		if b, err := json.Marshal(spec.Endpoint); err == nil {
			ann[annEndpoint] = string(b)
		}
	}
	if port := spec.PortNumber(provision.DefaultPortName); port != 0 {
		ann[annPort] = strconv.Itoa(port)
	}
	return ann
}

// renderInitContainer runs Spec.Init before the application container.
//
// 🔴 IT RUNS UNDER `sh -eu` AND DOES NOT SWALLOW FAILURES, WHICH IS THE
// OPPOSITE OF WHAT IT REPLACES. The project this came from appended file
// placement to the same list of shell commands and then wrapped the whole block
// in `{ … } || :` so a failure could not abort startup — necessary there,
// because a missing convenience file would otherwise have been an outage, and
// dangerous because it meant nothing in that block could ever be observed to
// fail. Here, content travels as Spec.Files and is placed by the kubelet, so
// Init holds only steps that are genuinely imperative and genuinely required.
// A failing one SHOULD stop the pod: that is a crash loop with a log, rather
// than a running agent missing something it needs.
func (d *Driver) renderInitContainer(spec provision.Spec, main corev1.Container) corev1.Container {
	wsPath := spec.Workspace.Path
	if wsPath == "" {
		wsPath = DefaultWorkspacePath
	}
	return corev1.Container{
		Name:  initContainerName,
		Image: spec.Runtime.Image,
		// -e: stop at the first failure. -u: an unset variable is an error, not
		// an empty string spliced into a path.
		Command:      []string{"sh", "-eu", "-c", strings.Join(spec.Init, "\n")},
		Env:          main.Env,
		EnvFrom:      main.EnvFrom,
		VolumeMounts: main.VolumeMounts,
		WorkingDir:   wsPath,
	}
}

// renderVolumes projects the spec's files.
//
// Each file gets its own volumeMount with a subPath, which is what places a
// single key at an arbitrary absolute path rather than turning its directory
// into a mount point. Per-file modes travel on the volume's items.
func (d *Driver) renderVolumes(spec provision.Spec) ([]corev1.Volume, []corev1.VolumeMount) {
	plain, secretFiles := sortedFiles(spec.Files)

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	if len(plain) > 0 {
		items := make([]corev1.KeyToPath, 0, len(plain))
		for _, f := range plain {
			mode := int32(f.EffectiveMode())
			key := fileKey(f.Path)
			items = append(items, corev1.KeyToPath{Key: key, Path: key, Mode: &mode})
			mounts = append(mounts, corev1.VolumeMount{
				Name: filesVolumeName, MountPath: f.Path, SubPath: key, ReadOnly: true,
			})
		}
		volumes = append(volumes, corev1.Volume{
			Name: filesVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: spec.Ref.Name + "-files"},
					Items:                items,
				},
			},
		})
	}

	if len(secretFiles) > 0 {
		items := make([]corev1.KeyToPath, 0, len(secretFiles))
		for _, f := range secretFiles {
			mode := int32(f.EffectiveMode())
			key := fileKey(f.Path)
			items = append(items, corev1.KeyToPath{Key: key, Path: key, Mode: &mode})
			mounts = append(mounts, corev1.VolumeMount{
				Name: secretFilesVolumeName, MountPath: f.Path, SubPath: key, ReadOnly: true,
			})
		}
		volumes = append(volumes, corev1.Volume{
			Name: secretFilesVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: spec.Ref.Name + "-env",
					Items:      items,
				},
			},
		})
	}

	ws := corev1.Volume{Name: workspaceVolumeName}
	if spec.Workspace.Persist {
		ws.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: spec.Ref.Name + "-workspace",
			},
		}
	} else {
		ws.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}
	volumes = append(volumes, ws)

	return volumes, mounts
}

func renderResources(r provision.Resources) (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{}
	set := func(list *corev1.ResourceList, name corev1.ResourceName, value, field string) error {
		if value == "" {
			return nil
		}
		q, err := resource.ParseQuantity(value)
		if err != nil {
			return fmt.Errorf("%w: resources.%s = %q: %v", provision.ErrInvalidSpec, field, value, err)
		}
		if *list == nil {
			*list = corev1.ResourceList{}
		}
		(*list)[name] = q
		return nil
	}
	if err := set(&out.Requests, corev1.ResourceCPU, r.CPURequest, "CPURequest"); err != nil {
		return out, err
	}
	if err := set(&out.Requests, corev1.ResourceMemory, r.MemoryRequest, "MemoryRequest"); err != nil {
		return out, err
	}
	if err := set(&out.Limits, corev1.ResourceCPU, r.CPULimit, "CPULimit"); err != nil {
		return out, err
	}
	if err := set(&out.Limits, corev1.ResourceMemory, r.MemoryLimit, "MemoryLimit"); err != nil {
		return out, err
	}
	return out, nil
}
