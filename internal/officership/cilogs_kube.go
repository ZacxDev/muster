package officership

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	pipelineRunsGVR = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}
	taskRunsGVR     = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "taskruns"}
)

// maxListed bounds one list of PipelineRuns or TaskRuns.
const maxListed = 200

// kubeCIReader is [CIReader] over the in-cluster Kubernetes API, bound to one
// namespace, authenticated by a dedicated ServiceAccount token file.
type kubeCIReader struct {
	ns   string
	dyn  dynamic.Interface
	core kubernetes.Interface
}

// NewKubeCIReader builds the production [CIReader]: the in-cluster API server
// and CA, with the bearer token read from tokenFile (re-read as it rotates).
//
// 🔴 THE TOKEN IS tokenFile's, NEVER THE POD'S OWN ServiceAccount's: muster's
// own grant is cluster-wide; this reader's must be get/list in ns only.
func NewKubeCIReader(ns, tokenFile string) (CIReader, error) {
	if _, err := os.Stat(tokenFile); err != nil {
		return nil, fmt.Errorf("ci_logs token_file: %w", err)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("ci_logs reads the in-cluster API: %w", err)
	}
	cfg.BearerToken = ""
	cfg.BearerTokenFile = tokenFile
	cfg.Timeout = 30 * time.Second
	cfg.UserAgent = "muster-officer-ci-logs"
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &kubeCIReader{ns: ns, dyn: dyn, core: core}, nil
}

func (k *kubeCIReader) PipelineRuns(ctx context.Context, selector string) ([]PipelineRun, error) {
	l, err := k.dyn.Resource(pipelineRunsGVR).Namespace(k.ns).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: maxListed})
	if err != nil {
		return nil, err
	}
	out := make([]PipelineRun, 0, len(l.Items))
	for _, it := range l.Items {
		out = append(out, ParsePipelineRun(it.Object))
	}
	return out, nil
}

func (k *kubeCIReader) TaskRuns(ctx context.Context, pipelineRun string) ([]TaskRun, error) {
	l, err := k.dyn.Resource(taskRunsGVR).Namespace(k.ns).List(ctx, metav1.ListOptions{LabelSelector: "tekton.dev/pipelineRun=" + pipelineRun, Limit: maxListed})
	if err != nil {
		return nil, err
	}
	out := make([]TaskRun, 0, len(l.Items))
	for _, it := range l.Items {
		out = append(out, ParseTaskRun(it.Object))
	}
	return out, nil
}

func (k *kubeCIReader) PodLog(ctx context.Context, pod, container string, maxBytes int64) (string, error) {
	tail := int64(20000)
	rc, err := k.core.CoreV1().Pods(k.ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, TailLines: &tail, LimitBytes: &maxBytes}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxBytes))
	return string(b), err
}

// ParsePipelineRun reads a tekton.dev/v1 PipelineRun object.
func ParsePipelineRun(obj map[string]any) PipelineRun {
	u := unstructured.Unstructured{Object: obj}
	p := PipelineRun{Name: u.GetName(), Created: u.GetCreationTimestamp().Time, Labels: u.GetLabels(), Params: map[string]string{}}
	params, _, _ := unstructured.NestedSlice(obj, "spec", "params")
	for _, x := range params {
		m, _ := x.(map[string]any)
		name, _ := m["name"].(string)
		if v, ok := m["value"].(string); ok && name != "" {
			p.Params[name] = v
		}
	}
	p.Succeeded, p.Reason, p.Message = succeeded(obj)
	return p
}

// ParseTaskRun reads a tekton.dev/v1 TaskRun object.
func ParseTaskRun(obj map[string]any) TaskRun {
	u := unstructured.Unstructured{Object: obj}
	t := TaskRun{Name: u.GetName(), PipelineTask: u.GetLabels()["tekton.dev/pipelineTask"]}
	t.PodName, _, _ = unstructured.NestedString(obj, "status", "podName")
	t.Succeeded, t.Reason, t.Message = succeeded(obj)
	steps, _, _ := unstructured.NestedSlice(obj, "status", "steps")
	for _, x := range steps {
		m, _ := x.(map[string]any)
		s := Step{}
		s.Name, _ = m["name"].(string)
		s.Container, _ = m["container"].(string)
		if term, ok := m["terminated"].(map[string]any); ok {
			s.Terminated = true
			s.ExitCode = int(asInt64(term["exitCode"]))
			s.Reason, _ = term["reason"].(string)
		}
		t.Steps = append(t.Steps, s)
	}
	return t
}

func succeeded(obj map[string]any) (status, reason, message string) {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, x := range conds {
		m, _ := x.(map[string]any)
		if m["type"] == "Succeeded" {
			status, _ = m["status"].(string)
			reason, _ = m["reason"].(string)
			message, _ = m["message"].(string)
			return
		}
	}
	return "", "", ""
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
