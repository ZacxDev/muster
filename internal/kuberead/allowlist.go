package kuberead

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Resource scopes, as an allowlist spells them.
const (
	ScopeNamespaced = "namespaced"
	ScopeCluster    = "cluster"
)

// Entry is one allowlisted (group, resource): a top-level resource, or the
// one subresource ever admitted, core pods/log.
type Entry struct {
	Group    string
	Resource string
	Scope    string
}

// Allowlist is the resources muster builds a path for on ONE cluster.
// Immutable. It comes from the deployment's configuration (ParseAllowlist):
// muster compiles in no resource list of its own.
type Allowlist struct {
	entries []Entry
	byKey   map[string]Entry
}

// neverGranted are refused in ANY group, whatever an allowlist says: a belt
// over the deployment's list (officers plan §2.1, "never granted").
var neverGranted = map[string]bool{
	"secrets": true, "serviceaccounts": true, "serviceaccounts/token": true,
	"pods/exec": true, "pods/attach": true, "pods/portforward": true, "pods/proxy": true, "pods/ephemeralcontainers": true,
	"services/proxy": true, "nodes/proxy": true, "certificatesigningrequests": true,
}

// neverGrantedGroups are API groups no allowlist may name.
var neverGrantedGroups = map[string]bool{"rbac.authorization.k8s.io": true}

// podsLog is the one subresource an allowlist may name, in the core group.
const podsLog = "pods/log"

// rule is one element of the allowlist document.
type rule struct {
	APIGroup  *string  `json:"apiGroup"`
	Resources []string `json:"resources"`
	Scope     string   `json:"scope"`
}

// ParseAllowlist reads one cluster's allowlist: a JSON list of
//
//	{"apiGroup": "<group, \"\" for core>", "resources": ["<plural>", …], "scope": "namespaced"|"cluster"}
//
// with get and list implied (no verbs field: there are no others). It is
// STRICT: an unknown field, a "*" resource or group, a subresource other than
// core pods/log (deployments/status, */status …), a never-granted resource,
// an RBAC group, a missing or unknown scope, an empty resources list and a
// duplicate are each refused.
//
// An empty document (blank, null or []) is NO allowlist: nil and no error.
// The caller treats that as the cluster's kube-read being disabled — never as
// "allow all".
func ParseAllowlist(b []byte) (*Allowlist, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rules []rule
	if err := dec.Decode(&rules); err != nil {
		return nil, fmt.Errorf(`kuberead: allowlist must be a JSON list of {"apiGroup", "resources", "scope"}: %v`, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("kuberead: allowlist: trailing data after the list")
	}
	if len(rules) == 0 {
		return nil, nil
	}
	a := &Allowlist{byKey: map[string]Entry{}}
	for i, r := range rules {
		if r.APIGroup == nil {
			return nil, fmt.Errorf("kuberead: allowlist[%d]: no apiGroup (\"\" is the core group)", i)
		}
		g := *r.APIGroup
		switch {
		case strings.Contains(g, "*"):
			return nil, fmt.Errorf("kuberead: allowlist[%d]: apiGroup %q: a wildcard is never admitted; name the group", i, g)
		case g != "" && !dnsSubdomain.MatchString(g):
			return nil, fmt.Errorf("kuberead: allowlist[%d]: apiGroup %q is not an API group", i, g)
		case neverGrantedGroups[g]:
			return nil, fmt.Errorf("kuberead: allowlist[%d]: apiGroup %q (rbac) is never granted", i, g)
		}
		if r.Scope != ScopeNamespaced && r.Scope != ScopeCluster {
			return nil, fmt.Errorf("kuberead: allowlist[%d]: scope %q, want %q or %q", i, r.Scope, ScopeNamespaced, ScopeCluster)
		}
		if len(r.Resources) == 0 {
			return nil, fmt.Errorf("kuberead: allowlist[%d]: no resources", i)
		}
		for _, res := range r.Resources {
			switch {
			case strings.Contains(res, "*"):
				return nil, fmt.Errorf("kuberead: allowlist[%d]: resource %q: a wildcard is never admitted; list each resource", i, res)
			case neverGranted[res]:
				return nil, fmt.Errorf("kuberead: allowlist[%d]: %s is never granted", i, groupResource(g, res))
			case res == podsLog && g == "":
				// The one subresource: read with the log form.
			case strings.Contains(res, "/"):
				return nil, fmt.Errorf("kuberead: allowlist[%d]: %s is a subresource: only core pods/log is admitted (status is read on the object)", i, groupResource(g, res))
			case !resourceRE.MatchString(res):
				return nil, fmt.Errorf("kuberead: allowlist[%d]: resource %q is not a plural resource name", i, res)
			}
			k := g + "|" + res
			if _, dup := a.byKey[k]; dup {
				return nil, fmt.Errorf("kuberead: allowlist names %s twice", groupResource(g, res))
			}
			e := Entry{Group: g, Resource: res, Scope: r.Scope}
			a.byKey[k] = e
			a.entries = append(a.entries, e)
		}
	}
	return a, nil
}

// Entries is a copy of the allowlist's entries.
func (a *Allowlist) Entries() []Entry { return append([]Entry(nil), a.entries...) }

// lookup is resource of group as the allowlist admits it: exactly, never by
// pattern. Never-granted names are refused whatever the list says.
func (a *Allowlist) lookup(group, resource string) (Entry, bool) {
	if a == nil || neverGranted[resource] || neverGrantedGroups[group] {
		return Entry{}, false
	}
	e, ok := a.byKey[group+"|"+resource]
	return e, ok
}
