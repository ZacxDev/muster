package officership

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
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

// ciTokenSeconds is the life of a minted read token: the API server's
// minimum, ample for one call.
const ciTokenSeconds = 600

// ownNamespaceFile is where a pod's own namespace is mounted.
const ownNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// kubeCIReader is [CIReader] over the Kubernetes API, bound to one
// namespace, authenticated by ONE minted token.
type kubeCIReader struct {
	ns   string
	dyn  dynamic.Interface
	core kubernetes.Interface
}

// NewKubeCIOpener builds the production [CILogsReader.Open] for the CI
// namespace ns: each call mints a 600 s token for ServiceAccount sa in
// muster's OWN namespace (TokenRequest, with muster's own in-cluster
// identity, which may create serviceaccounts/token for sa only) and returns a
// reader authenticated by that token alone.
//
// 🔴 muster's own token MINTS; it never READS. The reader's config is built
// fresh (ciReaderConfig), so nothing of muster's credentials — its token file
// above all, which client-go would prefer over the minted token — reaches it.
func NewKubeCIOpener(ns, sa string) (func(ctx context.Context) (CIReader, error), error) {
	own, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("ci_logs reads the in-cluster API: %w", err)
	}
	b, err := os.ReadFile(ownNamespaceFile)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return nil, fmt.Errorf("ci_logs: muster's own namespace (%s): %v", ownNamespaceFile, err)
	}
	ownNS := strings.TrimSpace(string(b))
	minter, err := kubernetes.NewForConfig(own)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (CIReader, error) {
		tok, err := mintCIToken(ctx, minter, ownNS, sa)
		if err != nil {
			return nil, err
		}
		cfg := ciReaderConfig(own, tok)
		dyn, err := dynamic.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
		core, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
		return &kubeCIReader{ns: ns, dyn: dyn, core: core}, nil
	}, nil
}

// mintCIToken asks the API server for a ciTokenSeconds token of ServiceAccount
// ns/sa, for the default audience (the API server's own).
func mintCIToken(ctx context.Context, c kubernetes.Interface, ns, sa string) (string, error) {
	exp := int64(ciTokenSeconds)
	tr, err := c.CoreV1().ServiceAccounts(ns).CreateToken(ctx, sa, &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &exp}},
		metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("minting a token for %s/%s: %w", ns, sa, err)
	}
	if tr == nil || tr.Status.Token == "" {
		return "", fmt.Errorf("minting a token for %s/%s: the API server returned none", ns, sa)
	}
	return tr.Status.Token, nil
}

// ciReaderConfig is a NEW client config with own's API server and CA and the
// bearer token tok — and no other credential of own's.
func ciReaderConfig(own *rest.Config, tok string) *rest.Config {
	return &rest.Config{
		Host:        own.Host,
		APIPath:     own.APIPath,
		BearerToken: tok,
		TLSClientConfig: rest.TLSClientConfig{Insecure: own.TLSClientConfig.Insecure, ServerName: own.TLSClientConfig.ServerName,
			CAFile: own.TLSClientConfig.CAFile, CAData: own.TLSClientConfig.CAData},
		Timeout:   30 * time.Second,
		UserAgent: "muster-officer-ci-logs",
	}
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
