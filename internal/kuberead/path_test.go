package kuberead_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	allow := kuberead.Default()
	for _, tc := range []struct{ name, args, path, query string }{
		{"list pods in a namespace", `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"pods","namespace":"web"}`,
			"/api/v1/namespaces/web/pods", "limit=501"},
		{"get a deployment", `{"cluster":"prod-a","verb":"get","group":"apps","version":"v1","resource":"deployments","namespace":"web","name":"api"}`,
			"/apis/apps/v1/namespaces/web/deployments/api", ""},
		{"list a cluster-scoped resource with no selector", `{"cluster":"prod-a","verb":"list","group":"","version":"v1","resource":"nodes"}`,
			"/api/v1/nodes", "limit=501"},
		{"a wildcard group's resource", `{"cluster":"prod-a","verb":"list","group":"helm.toolkit.fluxcd.io","version":"v2","resource":"helmreleases","namespace":"flux-system"}`,
			"/apis/helm.toolkit.fluxcd.io/v2/namespaces/flux-system/helmreleases", "limit=501"},
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
	allow := kuberead.Default()
	req := func(verb, group, resource, ns, name string) kuberead.Request {
		return kuberead.Request{Cluster: "prod-a", Verb: verb, Group: group, Version: "v1", Resource: resource, Namespace: ns, Name: name}
	}
	cases := map[string]kuberead.Request{
		"get a secret":                 req("get", "", "secrets", "web", "db"),
		"list secrets":                 req("list", "", "secrets", "web", ""),
		"secrets under a wildcard":     req("list", "helm.toolkit.fluxcd.io", "secrets", "web", ""),
		"serviceaccounts":              req("list", "", "serviceaccounts", "web", ""),
		"a serviceaccount token":       req("get", "", "serviceaccounts/token", "web", "default"),
		"pods/exec":                    req("get", "", "pods/exec", "web", "api-1"),
		"pods/attach":                  req("get", "", "pods/attach", "web", "api-1"),
		"pods/portforward":             req("get", "", "pods/portforward", "web", "api-1"),
		"pods/proxy":                   req("get", "", "pods/proxy", "web", "api-1"),
		"services/proxy":               req("get", "", "services/proxy", "web", "api"),
		"nodes/proxy":                  req("get", "", "nodes/proxy", "", "node-a"),
		"pods/ephemeralcontainers":     req("get", "", "pods/ephemeralcontainers", "web", "api-1"),
		"rbac":                         req("list", "rbac.authorization.k8s.io", "clusterroles", "", ""),
		"csr":                          req("list", "certificates.k8s.io", "certificatesigningrequests", "", ""),
		"an unknown group":             req("list", "example.com", "widgets", "web", ""),
		"watch":                        req("watch", "", "pods", "web", ""),
		"a write verb":                 req("delete", "", "pods", "web", "api-1"),
		"create":                       req("create", "", "pods", "web", ""),
		"no verb":                      req("", "", "pods", "web", ""),
		"pods/log by get":              req("get", "", "pods/log", "web", "api-1"),
		"a status subresource listed":  req("list", "apps", "deployments/status", "web", ""),
		"a status subresource read":    req("get", "apps", "deployments/status", "web", "api"),
		"pods/status":                  req("get", "", "pods/status", "web", "api-1"),
		"a name with a slash":          req("get", "", "configmaps", "web", "../secrets/db"),
		"a name with an escape":        req("get", "", "configmaps", "web", "x%2fsecrets"),
		"a namespace with a slash":     req("list", "", "configmaps", "web/secrets", ""),
		"a resource with a query":      req("list", "", "pods?watch=true", "web", ""),
		"a cluster resource namespace": req("list", "", "nodes", "web", ""),
		"get without a name":           req("get", "", "configmaps", "web", ""),
		"list with a name":             req("list", "", "configmaps", "web", "x"),
		"a group with dots only":       req("list", "..", "pods", "web", ""),
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
	allow := kuberead.Default()
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

// neverGranted is officers plan §2.1's "never granted" list: none of these may
// appear in the allowlist, in any group.
var neverGranted = []string{"secrets", "pods/exec", "pods/attach", "pods/portforward", "pods/proxy", "services/proxy", "nodes/proxy",
	"serviceaccounts", "serviceaccounts/token", "pods/ephemeralcontainers", "certificatesigningrequests"}

// rbacRules flattens one fixture's rules to "group|resource".
func rbacRules(t *testing.T, path string) (map[string]bool, []string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Rules []struct {
			APIGroups []string `json:"apiGroups"`
			Resources []string `json:"resources"`
			Verbs     []string `json:"verbs"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := map[string]bool{}
	var verbs []string
	for _, r := range f.Rules {
		verbs = append(verbs, strings.Join(r.Verbs, ","))
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				out[g+"|"+res] = true
			}
		}
	}
	return out, verbs
}

// TestTheAllowlistIsTheRBACLedger: muster's allowlist is EXACTLY the union of
// the officer-read ClusterRoles' rules (testdata/officer-read-rbac-*.json) —
// it fails when either side grows or shrinks. Those fixtures are the
// cross-repo ledger: each infrastructure repository pins its ClusterRole to a
// byte-identical copy, so the RBAC and this list cannot drift apart
// unnoticed. Every rule's verbs are exactly get and list (no watch), and
// nothing on the never-grant list, and no subresource but pods/log, appears.
func TestTheAllowlistIsTheRBACLedger(t *testing.T) {
	files, _ := filepath.Glob("testdata/officer-read-rbac-*.json")
	if len(files) != 2 {
		t.Fatalf("found %d officer-read fixtures (%v), want 2 (primary and fleet)", len(files), files)
	}
	want := map[string]bool{}
	for _, f := range files {
		rules, verbs := rbacRules(t, f)
		for _, v := range verbs {
			if v != "get,list" {
				t.Errorf("%s: a rule's verbs are %q, want exactly get,list (watch is not offered)", f, v)
			}
		}
		for k := range rules {
			want[k] = true
		}
	}
	if len(want) < 50 {
		t.Fatalf("positive control: the fixtures flatten to %d rules, want the full §2.1 list", len(want))
	}
	got := map[string]bool{}
	for _, e := range kuberead.Default().Entries() {
		got[e.Group+"|"+e.Resource] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("the RBAC grants %q and the allowlist does not: add it to allowlist.json, or drop it from the role", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("the allowlist names %q and the RBAC does not: the role must grant it first", k)
		}
	}
	for k := range got {
		res := k[strings.Index(k, "|")+1:]
		for _, n := range neverGranted {
			if res == n {
				t.Errorf("the allowlist names %q, which is never granted", k)
			}
		}
		if strings.Contains(res, "/") && k != "|pods/log" {
			t.Errorf("the allowlist names the subresource %q: only pods/log is offered", k)
		}
		if strings.HasPrefix(k, "rbac.authorization.k8s.io|") {
			t.Errorf("the allowlist names an RBAC resource: %q", k)
		}
	}
}
