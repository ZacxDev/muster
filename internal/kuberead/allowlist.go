package kuberead

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed allowlist.json
var allowlistJSON []byte

// Resource scopes, as allowlist.json spells them.
const (
	ScopeNamespaced = "namespaced"
	ScopeCluster    = "cluster"
)

// Entry is one allowlisted (group, resource): resource may be a subresource
// ("pods/log", "deployments/status") or "*" (every resource of the group).
type Entry struct {
	Group    string `json:"group"`
	Resource string `json:"resource"`
	Scope    string `json:"scope"`
}

// Allowlist is the resources muster builds a path for. Immutable.
type Allowlist struct {
	entries []Entry
	byKey   map[string]Entry
}

// neverGranted are refused in ANY group, a wildcard group included: a belt
// over the allowlist (officers plan §2.1, "never granted").
var neverGranted = map[string]bool{
	"secrets": true, "serviceaccounts": true, "serviceaccounts/token": true,
	"pods/exec": true, "pods/attach": true, "pods/portforward": true, "pods/proxy": true, "pods/ephemeralcontainers": true,
	"services/proxy": true, "nodes/proxy": true, "certificatesigningrequests": true,
}

var defaultAllowlist = mustParseAllowlist(allowlistJSON)

// Default is the embedded allowlist (allowlist.json).
func Default() *Allowlist { return defaultAllowlist }

func mustParseAllowlist(b []byte) *Allowlist {
	a, err := ParseAllowlist(b)
	if err != nil {
		panic(err)
	}
	return a
}

// ParseAllowlist reads an allowlist file. An entry with an unknown scope, a
// never-granted resource or a duplicate is refused.
func ParseAllowlist(b []byte) (*Allowlist, error) {
	var f struct {
		Resources []Entry `json:"resources"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("kuberead: allowlist: %w", err)
	}
	a := &Allowlist{byKey: map[string]Entry{}}
	for _, e := range f.Resources {
		if e.Scope != ScopeNamespaced && e.Scope != ScopeCluster {
			return nil, fmt.Errorf("kuberead: allowlist: %s/%s has scope %q", e.Group, e.Resource, e.Scope)
		}
		if neverGranted[e.Resource] {
			return nil, fmt.Errorf("kuberead: allowlist names %s/%s, which is never granted", e.Group, e.Resource)
		}
		k := e.Group + "|" + e.Resource
		if _, dup := a.byKey[k]; dup {
			return nil, fmt.Errorf("kuberead: allowlist names %s/%s twice", e.Group, e.Resource)
		}
		a.byKey[k] = e
		a.entries = append(a.entries, e)
	}
	return a, nil
}

// Entries is a copy of the allowlist's entries.
func (a *Allowlist) Entries() []Entry { return append([]Entry(nil), a.entries...) }

// lookup is resource of group as the allowlist admits it: exactly, or under
// the group's "*" (a wildcard admits a top-level resource only, never a
// subresource). Never-granted names are refused whatever the group.
func (a *Allowlist) lookup(group, resource string) (Entry, bool) {
	if neverGranted[resource] {
		return Entry{}, false
	}
	if e, ok := a.byKey[group+"|"+resource]; ok {
		return e, true
	}
	if e, ok := a.byKey[group+"|*"]; ok && dnsLabel.MatchString(resource) {
		return Entry{Group: group, Resource: resource, Scope: e.Scope}, true
	}
	return Entry{}, false
}
