package kuberead

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Request is officer_kube_read's arguments: a resource read (Verb …) or a pod
// log read (Log), never both.
type Request struct {
	// Cluster names one of the officer's declared clusters (kube.clusters).
	Cluster string `json:"cluster"`
	// Verb is get (Name required) or list (Name refused).
	Verb          string `json:"verb,omitempty"`
	Group         string `json:"group"`
	Version       string `json:"version,omitempty"`
	Resource      string `json:"resource,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	Name          string `json:"name,omitempty"`
	LabelSelector string `json:"label_selector,omitempty"`
	FieldSelector string `json:"field_selector,omitempty"`
	// Limit, on a list, is the most objects to return (1..MaxObjects); the
	// answer says `more` when the API holds more. 0 asks for every object, and
	// more than MaxObjects is then a refusal.
	Limit int         `json:"limit,omitempty"`
	Log   *LogRequest `json:"log,omitempty"`
}

// LogRequest is a pod log read.
type LogRequest struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container,omitempty"`
	Previous  bool   `json:"previous,omitempty"`
	// SinceSeconds is 0 (no bound) or 1..MaxSinceSeconds.
	SinceSeconds int `json:"since_seconds,omitempty"`
	// TailLines is 0 (DefaultTailLines) or 1..MaxTailLines.
	TailLines int `json:"tail_lines,omitempty"`
}

// MaxArgsBytes bounds the tool's arguments.
const MaxArgsBytes = 8 << 10

// ParseRequest decodes the tool's arguments STRICTLY: an unknown key — a url,
// a path, watch, follow — is refused, never ignored.
func ParseRequest(raw []byte) (Request, error) {
	if len(raw) > MaxArgsBytes {
		return Request{}, fmt.Errorf("%w: arguments are over %d bytes", ErrRefused, MaxArgsBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Request
	if err := dec.Decode(&r); err != nil {
		return Request{}, fmt.Errorf("%w: arguments are {cluster, verb: get|list, group, version, resource, namespace?, name?, "+
			"label_selector?, field_selector?, limit?} or {cluster, log: {namespace, pod, container?, previous?, since_seconds?, tail_lines?}}: %v", ErrRefused, err)
	}
	return r, nil
}

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	versionRE    = regexp.MustCompile(`^v[0-9]{1,3}((alpha|beta)[0-9]{1,3})?$`)
	resourceRE   = regexp.MustCompile(`^[a-z][a-z0-9]{0,62}$`)
)

// maxSelector bounds a label or field selector.
const maxSelector = 1024

// Built is a request's GET path and query, as Build made them.
type Built struct {
	Path  string
	Query url.Values
	// List: the answer is a list (count its items); Log: a pod's log.
	List bool
	Log  bool
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRefused}, args...)...)
}

// Build is THE place a request becomes a path. It refuses (ErrRefused) every
// request that is not a get or list of an allowlisted resource, or a pod log
// read, within the bounds — before anything is sent anywhere.
func (a *Allowlist) Build(r Request) (Built, error) {
	if r.Log != nil {
		if r.Verb != "" || r.Group != "" || r.Version != "" || r.Resource != "" || r.Namespace != "" || r.Name != "" ||
			r.LabelSelector != "" || r.FieldSelector != "" || r.Limit != 0 {
			return Built{}, refuse("give either a resource read (verb, group, version, resource, …) or log, not both")
		}
		return a.buildLog(*r.Log)
	}
	if r.Verb != "get" && r.Verb != "list" {
		return Built{}, refuse("verb %q: only get and list (no watch, no write)", r.Verb)
	}
	if !versionRE.MatchString(r.Version) {
		return Built{}, refuse("version %q is not an API version (v1, v2beta1, …)", r.Version)
	}
	if r.Group != "" && !dnsSubdomain.MatchString(r.Group) {
		return Built{}, refuse("group %q is not an API group", r.Group)
	}
	// No subresource is offered to a resource read: a pod's log has its own
	// form, and status is read on the object itself.
	if r.Group == "" && r.Resource == "pods/log" {
		return Built{}, refuse("a pod's log is read with {log: {namespace, pod, …}}, not verb get")
	}
	if !resourceRE.MatchString(r.Resource) {
		return Built{}, refuse("resource %q is not a resource name (no subresources: read the object, or a pod's log with log)", r.Resource)
	}
	e, ok := a.lookup(r.Group, r.Resource)
	if !ok {
		return Built{}, refuse("%s is not on officer_kube_read's allowlist (secrets, exec, attach, port-forward, proxies, "+
			"ServiceAccount tokens and RBAC never are)", groupResource(r.Group, r.Resource))
	}
	switch {
	case e.Scope == ScopeCluster && r.Namespace != "":
		return Built{}, refuse("%s is cluster-scoped: give no namespace", groupResource(r.Group, r.Resource))
	case r.Namespace != "" && !dnsLabel.MatchString(r.Namespace):
		return Built{}, refuse("namespace %q is not a namespace name", r.Namespace)
	}
	switch {
	case r.Verb == "get" && r.Name == "":
		return Built{}, refuse("get needs a name (list reads many)")
	case r.Verb == "list" && r.Name != "":
		return Built{}, refuse("list takes no name (get reads one)")
	case r.Name != "" && !dnsSubdomain.MatchString(r.Name):
		return Built{}, refuse("name %q is not an object name", r.Name)
	}
	for _, s := range []struct{ name, v string }{{"label_selector", r.LabelSelector}, {"field_selector", r.FieldSelector}} {
		if len(s.v) > maxSelector || strings.ContainsFunc(s.v, func(c rune) bool { return c < 0x20 || c > 0x7e }) {
			return Built{}, refuse("%s must be printable ASCII, at most %d bytes", s.name, maxSelector)
		}
	}
	p := "/api/" + r.Version
	if r.Group != "" {
		p = "/apis/" + r.Group + "/" + r.Version
	}
	if r.Namespace != "" {
		p += "/namespaces/" + r.Namespace
	}
	p += "/" + r.Resource
	if r.Name != "" {
		p += "/" + r.Name
	}
	q := url.Values{}
	if r.Verb == "get" {
		if r.LabelSelector != "" || r.FieldSelector != "" || r.Limit != 0 {
			return Built{}, refuse("selectors and limit apply to list only")
		}
		return Built{Path: p, Query: q}, nil
	}
	if e.Scope == ScopeNamespaced && r.Namespace == "" && r.LabelSelector == "" && r.FieldSelector == "" {
		return Built{}, refuse("a list of %s across ALL namespaces needs a label_selector or field_selector (or name a namespace)",
			groupResource(r.Group, r.Resource))
	}
	switch {
	case r.Limit < 0 || r.Limit > MaxObjects:
		return Built{}, refuse("limit %d is not 1..%d (leave it out for every object, refused past %d)", r.Limit, MaxObjects, MaxObjects)
	case r.Limit == 0:
		// One past the cap, so an answer over it is SEEN and refused.
		q.Set("limit", strconv.Itoa(MaxObjects+1))
	default:
		q.Set("limit", strconv.Itoa(r.Limit))
	}
	if r.LabelSelector != "" {
		q.Set("labelSelector", r.LabelSelector)
	}
	if r.FieldSelector != "" {
		q.Set("fieldSelector", r.FieldSelector)
	}
	return Built{Path: p, Query: q, List: true}, nil
}

func (a *Allowlist) buildLog(l LogRequest) (Built, error) {
	if _, ok := a.lookup("", "pods/log"); !ok {
		return Built{}, refuse("pods/log is not on the allowlist")
	}
	switch {
	case !dnsLabel.MatchString(l.Namespace):
		return Built{}, refuse("log.namespace %q is required and must be a namespace name", l.Namespace)
	case !dnsSubdomain.MatchString(l.Pod):
		return Built{}, refuse("log.pod %q is required and must be a pod name", l.Pod)
	case l.Container != "" && !dnsLabel.MatchString(l.Container):
		return Built{}, refuse("log.container %q is not a container name", l.Container)
	case l.SinceSeconds < 0 || l.SinceSeconds > MaxSinceSeconds:
		return Built{}, refuse("log.since_seconds %d is not 1..%d", l.SinceSeconds, MaxSinceSeconds)
	case l.TailLines < 0 || l.TailLines > MaxTailLines:
		return Built{}, refuse("log.tail_lines %d is not 1..%d", l.TailLines, MaxTailLines)
	}
	q := url.Values{}
	if l.Container != "" {
		q.Set("container", l.Container)
	}
	if l.Previous {
		q.Set("previous", "true")
	}
	if l.SinceSeconds > 0 {
		q.Set("sinceSeconds", strconv.Itoa(l.SinceSeconds))
	}
	tail := l.TailLines
	if tail == 0 {
		tail = DefaultTailLines
	}
	q.Set("tailLines", strconv.Itoa(tail))
	q.Set("limitBytes", strconv.Itoa(MaxAnswerBytes))
	return Built{Path: "/api/v1/namespaces/" + l.Namespace + "/pods/" + l.Pod + "/log", Query: q, Log: true}, nil
}

func groupResource(group, resource string) string {
	if group == "" {
		return resource
	}
	return resource + "." + group
}
