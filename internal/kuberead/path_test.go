package kuberead_test

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/kuberead"
)

// ---------------------------------------------------------------------------
// THE PATH BUILDER: typed arguments in, ONE GET path out, and only for a
// resource on the allowlist. The model never sends a URL or a path.
// ---------------------------------------------------------------------------

func parse(t *testing.T, raw string) kuberead.Request {
	t.Helper()
	r, err := kuberead.ParseRequest([]byte(raw))
	if err != nil {
		t.Fatalf("ParseRequest(%s): %v", raw, err)
	}
	return r
}

// TestThePathBuilderBuildsOnlyAllowlistedGETPaths pins the exact path and
// query muster builds for each request shape (literal expectations, never
// derived from the builder).
func TestThePathBuilderBuildsOnlyAllowlistedGETPaths(t *testing.T) {
	allow := fixtureAllow(t)
	for _, tc := range []struct{ name, args, path, query string }{
		{"list pods in a namespace", `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"pods","namespace":"web"}`,
			"/api/v1/namespaces/web/pods", "limit=501"},
		{"get a deployment", `{"cluster":"prod-a","verb":"get","group":"apps","version":"v1","resource":"deployments","namespace":"web","name":"api"}`,
			"/apis/apps/v1/namespaces/web/deployments/api", ""},
		{"list a cluster-scoped resource with no selector", `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"nodes"}`,
			"/api/v1/nodes", "limit=501"},
		{"a custom resource", `{"cluster":"prod-a","verb":"list","group":"widgets.example.com","version":"v2","resource":"widgets","namespace":"tools"}`,
			"/apis/widgets.example.com/v2/namespaces/tools/widgets", "limit=501"},
		{"an all-namespace list with a label selector", `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"pods","label_selector":"app=api"}`,
			"/api/v1/pods", "labelSelector=app%3Dapi&limit=501"},
		{"an all-namespace list with a field selector and a limit", `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"events","field_selector":"type=Warning","limit":50}`,
			"/api/v1/events", "fieldSelector=type%3DWarning&limit=50"},
		{"a pod's log", `{"cluster":"prod-a","log":{"namespace":"web","pod":"api-1","container":"app","previous":true,"since_seconds":3600,"tail_lines":100}}`,
			"/api/v1/namespaces/web/pods/api-1/log", "container=app&limitBytes=1048576&previous=true&sinceSeconds=3600&tailLines=100"},
		{"a pod's log with the defaults", `{"cluster":"prod-a","log":{"namespace":"web","pod":"api-1"}}`,
			"/api/v1/namespaces/web/pods/api-1/log", "limitBytes=1048576&tailLines=500"},
	} {
		b, err := allow.Build(parse(t, tc.args))
		if err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
			continue
		}
		if b.Path != tc.path || b.Query.Encode() != tc.query {
			t.Errorf("%s: built %s?%s, want %s?%s", tc.name, b.Path, b.Query.Encode(), tc.path, tc.query)
		}
	}
}

// TestThePathBuilderRefusesEverythingOutsideTheAllowlist: secrets, exec,
// attach, port-forward, every proxy, ServiceAccount tokens, RBAC, a write
// verb, watch, and every way of smuggling a path through a field are
// refused — each with kuberead.ErrRefused, so the refusal is THIS guard's.
func TestThePathBuilderRefusesEverythingOutsideTheAllowlist(t *testing.T) {
	allow := fixtureAllow(t)
	req := func(verb, group, resource, ns, name string) kuberead.Request {
		return kuberead.Request{Cluster: "prod-a", Verb: verb, Group: group, Version: "v1", Resource: resource, Namespace: ns, Name: name}
	}
	cases := map[string]kuberead.Request{
		"get a secret":                           req("get", "", "secrets", "web", "db"),
		"list secrets":                           req("list", "", "secrets", "web", ""),
		"secrets in a listed group":              req("list", "widgets.example.com", "secrets", "web", ""),
		"an unlisted resource of a listed group": req("list", "widgets.example.com", "sprockets", "web", ""),
		"serviceaccounts":                        req("list", "", "serviceaccounts", "web", ""),
		"a serviceaccount token":                 req("get", "", "serviceaccounts/token", "web", "default"),
		"pods/exec":                              req("get", "", "pods/exec", "web", "api-1"),
		"pods/attach":                            req("get", "", "pods/attach", "web", "api-1"),
		"pods/portforward":                       req("get", "", "pods/portforward", "web", "api-1"),
		"pods/proxy":                             req("get", "", "pods/proxy", "web", "api-1"),
		"services/proxy":                         req("get", "", "services/proxy", "web", "api"),
		"nodes/proxy":                            req("get", "", "nodes/proxy", "", "node-a"),
		"pods/ephemeralcontainers":               req("get", "", "pods/ephemeralcontainers", "web", "api-1"),
		"rbac":                                   req("list", "rbac.authorization.k8s.io", "clusterroles", "", ""),
		"csr":                                    req("list", "certificates.k8s.io", "certificatesigningrequests", "", ""),
		"an unknown group":                       req("list", "example.com", "widgets", "web", ""),
		"watch":                                  req("watch", "", "pods", "web", ""),
		"a write verb":                           req("delete", "", "pods", "web", "api-1"),
		"create":                                 req("create", "", "pods", "web", ""),
		"no verb":                                req("", "", "pods", "web", ""),
		"pods/log by get":                        req("get", "", "pods/log", "web", "api-1"),
		"a status subresource listed":            req("list", "apps", "deployments/status", "web", ""),
		"a status subresource read":              req("get", "apps", "deployments/status", "web", "api"),
		"pods/status":                            req("get", "", "pods/status", "web", "api-1"),
		"a name with a slash":                    req("get", "", "configmaps", "web", "../secrets/db"),
		"a name with an escape":                  req("get", "", "configmaps", "web", "x%2fsecrets"),
		"a namespace with a slash":               req("list", "", "configmaps", "web/secrets", ""),
		"a resource with a query":                req("list", "", "pods?watch=true", "web", ""),
		"a cluster resource namespace":           req("list", "", "nodes", "web", ""),
		"get without a name":                     req("get", "", "configmaps", "web", ""),
		"list with a name":                       req("list", "", "configmaps", "web", "x"),
		"a group with dots only":                 req("list", "..", "pods", "web", ""),
	}
	bad := req("list", "", "pods", "web", "")
	bad.Version = "v1/../../api/v1/secrets"
	cases["a version with a path"] = bad
	over := req("list", "", "pods", "web", "")
	over.Limit = 501
	cases["a limit over the object cap"] = over
	neg := req("list", "", "pods", "web", "")
	neg.Limit = -1
	cases["a negative limit"] = neg
	for _, l := range []kuberead.LogRequest{
		{Namespace: "web", Pod: "api-1", TailLines: 2001},
		{Namespace: "web", Pod: "api-1", SinceSeconds: 86401},
		{Namespace: "web", Pod: "api-1/exec"},
		{Namespace: "", Pod: "api-1"},
		{Namespace: "web", Pod: "api-1", Container: "app/../x"},
	} {
		l := l
		cases["log "+l.Pod+" "+l.Container] = kuberead.Request{Cluster: "prod-a", Log: &l}
	}
	both := req("list", "", "pods", "web", "")
	both.Log = &kuberead.LogRequest{Namespace: "web", Pod: "api-1"}
	cases["a read and a log at once"] = both
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := allow.Build(cases[n])
		if !errors.Is(err, kuberead.ErrRefused) {
			t.Errorf("%s: built %q (err %v), want a refusal wrapping ErrRefused", n, b.Path, err)
		}
	}
}

// TestParseRequestRefusesAnUnknownKey: a url, a path or a watch flag is not
// an argument muster has, so it is refused rather than ignored.
func TestParseRequestRefusesAnUnknownKey(t *testing.T) {
	for _, raw := range []string{
		`{"cluster":"prod-a","url":"https://example.com/api/v1/secrets"}`,
		`{"cluster":"prod-a","path":"/api/v1/secrets"}`,
		`{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"pods","namespace":"web","watch":true}`,
		`{"cluster":"prod-a","log":{"namespace":"web","pod":"api-1","follow":true}}`,
		`[]`,
	} {
		if _, err := kuberead.ParseRequest([]byte(raw)); err == nil {
			t.Errorf("ParseRequest(%s) accepted it", raw)
		}
	}
	if _, err := kuberead.ParseRequest([]byte(`{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"pods","namespace":"web"}`)); err != nil {
		t.Fatalf("positive control: a well-formed request was refused: %v", err)
	}
}

// TestAnAllNamespaceListNeedsASelector: a list of a namespaced resource with
// no namespace must carry a label or field selector; a cluster-scoped list
// needs none.
func TestAnAllNamespaceListNeedsASelector(t *testing.T) {
	allow := fixtureAllow(t)
	if _, err := allow.Build(kuberead.Request{Cluster: "prod-a", Verb: "list", Version: "v1", Resource: "pods"}); !errors.Is(err, kuberead.ErrRefused) ||
		!strings.Contains(err.Error(), "selector") {
		t.Errorf("an all-namespace pod list with no selector: err %v, want a refusal naming the selector", err)
	}
	for _, r := range []kuberead.Request{
		{Cluster: "prod-a", Verb: "list", Version: "v1", Resource: "pods", LabelSelector: "app=api"},
		{Cluster: "prod-a", Verb: "list", Version: "v1", Resource: "pods", FieldSelector: "spec.nodeName=node-a"},
		{Cluster: "prod-a", Verb: "list", Version: "v1", Resource: "nodes"},
	} {
		if _, err := allow.Build(r); err != nil {
			t.Errorf("%+v: refused: %v", r, err)
		}
	}
}

// fixtureAllowJSON is a made-up allowlist in the deployment's format: no
// real deployment's resources.
const fixtureAllowJSON = `[
  {"apiGroup": "", "resources": ["pods", "pods/log", "events", "configmaps", "services"], "scope": "namespaced"},
  {"apiGroup": "", "resources": ["nodes", "namespaces"], "scope": "cluster"},
  {"apiGroup": "apps", "resources": ["deployments"], "scope": "namespaced"},
  {"apiGroup": "widgets.example.com", "resources": ["widgets", "gadgets"], "scope": "namespaced"}
]`

func fixtureAllow(t *testing.T) *kuberead.Allowlist {
	t.Helper()
	a, err := kuberead.ParseAllowlist([]byte(fixtureAllowJSON))
	if err != nil {
		t.Fatalf("the fixture allowlist was refused: %v", err)
	}
	return a
}

// TestParseAllowlistIsStrict: the deployment's allowlist is a list of
// {apiGroup, resources, scope}; get and list are implied. Each malformed
// shape is refused: an unknown field, "*" (resource or group), any
// subresource but core pods/log (*/status included), a never-granted
// resource or RBAC, a missing scope, a duplicate. An empty one is no
// allowlist (nil, no error), which the caller treats as "disabled", never
// "allow all".
func TestParseAllowlistIsStrict(t *testing.T) {
	a := fixtureAllow(t)
	if n := len(a.Entries()); n != 12 {
		t.Fatalf("the fixture flattens to %d entries, want 12", n)
	}
	for _, raw := range []string{
		`[{"apiGroup":"widgets.example.com","resources":["*"],"scope":"namespaced"}]`,
		`[{"apiGroup":"*","resources":["widgets"],"scope":"namespaced"}]`,
		`[{"apiGroup":"apps","resources":["deployments/status"],"scope":"namespaced"}]`,
		`[{"apiGroup":"apps","resources":["*/status"],"scope":"namespaced"}]`,
		`[{"apiGroup":"","resources":["pods/status"],"scope":"namespaced"}]`,
		`[{"apiGroup":"widgets.example.com","resources":["pods/log"],"scope":"namespaced"}]`,
		`[{"apiGroup":"","resources":["secrets"],"scope":"namespaced"}]`,
		`[{"apiGroup":"","resources":["serviceaccounts"],"scope":"namespaced"}]`,
		`[{"apiGroup":"rbac.authorization.k8s.io","resources":["roles"],"scope":"namespaced"}]`,
		`[{"apiGroup":"","resources":["pods"],"scope":"namespaced","verbs":["watch"]}]`,
		`[{"apiGroup":"","resources":["pods"]}]`,
		`[{"apiGroup":"","resources":[],"scope":"namespaced"}]`,
		`[{"apiGroup":"","resources":["Pods"],"scope":"namespaced"}]`,
		`[{"apiGroup":"","resources":["pods"],"scope":"namespaced"},{"apiGroup":"","resources":["pods"],"scope":"cluster"}]`,
		`{"resources":[]}`,
	} {
		if a, err := kuberead.ParseAllowlist([]byte(raw)); err == nil || a != nil || !strings.HasPrefix(err.Error(), "kuberead: allowlist") {
			t.Errorf("ParseAllowlist(%s): %v, err %v; want a refusal", raw, a, err)
		}
	}
	for _, raw := range []string{``, `  `, `[]`, `null`} {
		if a, err := kuberead.ParseAllowlist([]byte(raw)); err != nil || a != nil {
			t.Errorf("ParseAllowlist(%q): %v, %v; want no allowlist and no error", raw, a, err)
		}
	}
}
