package officers

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/ZacxDev/muster/internal/kuberead"
)

// ---------------------------------------------------------------------------
// READING A CLUSTER: THE `kube` BLOCK AND officer_kube_read (docs/officers.md
// "Reading a cluster: officer_kube_read").
//
// A tiered officer with a `kube` block offers its JUDGEMENT instances
// officer_kube_read: a typed, read-only request against one of the clusters
// the block names, which muster-server runs itself (internal/kuberead) as the
// officer's own ServiceAccount on that cluster, with a 600 s token minted per
// call. The instance never holds a cluster credential and never names an
// address: it names a cluster, and muster dials only that cluster's endpoint.
//
//	"kube": {"clusters": [{"name": "prod-a", "service_account": "officers/officer-anomaly-prod-a"}]}
//
// Each cluster's endpoint (host:port), CA bundle (PEM) and minter token are
// the server's SEALED environment, and its read allowlist the deployment's
// configuration, never the declaration:
//
//	MUSTER_KUBE_READ_ENDPOINT_<CLUSTER>, MUSTER_KUBE_READ_CA_<CLUSTER>,
//	MUSTER_KUBE_READ_MINTER_TOKEN_<CLUSTER>, MUSTER_KUBE_READ_ALLOWLIST_<CLUSTER>
//
// (<CLUSTER> is the name as TokenEnvSuffix spells an id: prod-a -> PROD_A).
//
// 🔴 THE ALLOWLIST IS THE DEPLOYMENT'S, AND ITS ABSENCE FAILS CLOSED. muster
// compiles in no resource list: MUSTER_KUBE_READ_ALLOWLIST_<CLUSTER> is a JSON
// list of {"apiGroup", "resources", "scope"} (get and list implied), parsed
// strictly by kuberead.ParseAllowlist — a malformed one refuses the boot. One
// that is unset, blank or empty DISABLES that cluster's kube-read: the officer
// still loads, the cluster is not offered (KubeClusters leaves it out), and
// the server logs it at startup (KubeReadDisabled). Never "allow all".
//
// 🔴 JUDGEMENT ONLY. The block needs `tiers`, and only the judgement pool is
// offered the tool or may request the kube-read connector: a routine instance
// works untriaged items on a cheaper model, and a cluster read is an
// investigation's. The grant is per instance and approved by hand, as every
// officer connector grant is (never from a shared approval).
// ---------------------------------------------------------------------------

const (
	// ToolKubeRead is the judgement instances' cluster read tool.
	ToolKubeRead = "officer_kube_read"
	// KubeConnector is the connector officer_kube_read's grant is filed for
	// (connectors.KubeReadName).
	KubeConnector = "kube-read"
	// The sealed environment each declared cluster is read from.
	KubeEndpointEnvPrefix  = "MUSTER_KUBE_READ_ENDPOINT_"
	KubeCAEnvPrefix        = "MUSTER_KUBE_READ_CA_"
	KubeMinterEnvPrefix    = "MUSTER_KUBE_READ_MINTER_TOKEN_"
	KubeAllowlistEnvPrefix = "MUSTER_KUBE_READ_ALLOWLIST_"
	// MaxKubeClusters bounds kube.clusters.
	MaxKubeClusters = 8
)

// Kube is an officer's `kube` block.
type Kube struct {
	// Clusters is every declared cluster, an allowlist-less one included
	// (Allow nil); KubeClusters is the readable ones.
	Clusters []KubeCluster
}

// KubeCluster is one cluster an officer's judgement instances may read.
type KubeCluster struct {
	// Name is the cluster's name, as the instance names it.
	Name string
	// ServiceAccount is the officer's own read-only ServiceAccount there,
	// "<namespace>/<name>": the identity each call's token is minted for.
	ServiceAccount string
	// Endpoint is the API server's host:port, from the sealed environment.
	Endpoint string
	// CA is the PEM bundle its certificate chains to, from the sealed
	// environment.
	CA []byte
	// Allow is the cluster's read allowlist, from the deployment's
	// configuration; nil when none is set, which disables the cluster.
	Allow *kuberead.Allowlist
	// minter is the minter token, from the sealed environment. Unexported:
	// encoding/json and fmt never print it; MinterToken is the one read.
	minter string
}

// MinterToken is the cluster's minter token. Its only caller builds the
// cluster reader (internal/api); tests assert where it must not appear.
func (k KubeCluster) MinterToken() string { return k.minter }

// String names the cluster without its token.
func (k KubeCluster) String() string {
	return fmt.Sprintf("cluster %s (%s, as %s)", k.Name, k.Endpoint, k.ServiceAccount)
}

// GoString is String.
func (k KubeCluster) GoString() string { return k.String() }

// KubeClusters is the clusters o's instances may read: the `kube` block's
// clusters that have an allowlist, for the judgement pool of an officer that
// declares one; nil otherwise.
func (o Officer) KubeClusters() []KubeCluster {
	if o.Kube == nil || o.Tier != TierJudgement {
		return nil
	}
	var out []KubeCluster
	for _, c := range o.Kube.Clusters {
		if c.Allow != nil {
			out = append(out, c)
		}
	}
	return out
}

// KubeReadDisabled is one line per declared cluster whose kube-read is
// disabled because the deployment set no allowlist for it, for the server to
// log at startup. An officer is named once, whatever its pools.
func KubeReadDisabled(list []Officer) []string {
	var out []string
	seen := map[string]bool{}
	for _, o := range list {
		if o.Kube == nil {
			continue
		}
		for _, c := range o.Kube.Clusters {
			k := o.ID + "|" + c.Name
			if c.Allow != nil || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, fmt.Sprintf("officer %s: kube-read DISABLED for cluster %s: %s%s is unset or empty (fail closed: nothing on it is readable)",
				o.ID, c.Name, KubeAllowlistEnvPrefix, TokenEnvSuffix(c.Name)))
		}
	}
	return out
}

// KubeCluster is the declared cluster named name, for o's instances.
func (o Officer) KubeCluster(name string) (KubeCluster, bool) {
	for _, c := range o.KubeClusters() {
		if c.Name == name {
			return c, true
		}
	}
	return KubeCluster{}, false
}

type declaredKube struct {
	Clusters []struct {
		Name           string `json:"name"`
		ServiceAccount string `json:"service_account"`
	} `json:"clusters"`
}

// parseKube reads a `kube` block strictly, and each cluster's sealed
// environment through getenv. tiered is whether the officer has `tiers`.
func parseKube(raw json.RawMessage, tiered bool, getenv func(string) string) (*Kube, error) {
	if !tiered {
		return nil, errors.New("kube needs tiers: officer_kube_read is offered to the judgement pool only")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d declaredKube
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf(`kube must be {"clusters": [{"name", "service_account"}]}: %v`, err)
	}
	if len(d.Clusters) == 0 || len(d.Clusters) > MaxKubeClusters {
		return nil, fmt.Errorf("kube.clusters lists %d clusters, want 1..%d", len(d.Clusters), MaxKubeClusters)
	}
	k := &Kube{}
	seen := map[string]bool{}
	for _, c := range d.Clusters {
		name := strings.TrimSpace(c.Name)
		if !dnsLabelRE.MatchString(name) {
			return nil, fmt.Errorf("kube.clusters: name %q is not a cluster name (lowercase letters, digits and '-')", c.Name)
		}
		if seen[name] {
			return nil, fmt.Errorf("kube.clusters lists %q twice", name)
		}
		seen[name] = true
		ns, sa, ok := strings.Cut(strings.TrimSpace(c.ServiceAccount), "/")
		if !ok || !dnsLabelRE.MatchString(ns) || len(sa) > 253 || !saNameRE.MatchString(sa) {
			return nil, fmt.Errorf("kube.clusters[%s]: service_account %q must be <namespace>/<name>: the officer's own read-only ServiceAccount there", name, c.ServiceAccount)
		}
		suffix := TokenEnvSuffix(name)
		// Each variable is read by its own constant prefix (the env-doc gate
		// resolves `PREFIX + <dynamic>`, not a prefix passed through a helper).
		ep := strings.TrimSpace(getenv(KubeEndpointEnvPrefix + suffix))
		ca := strings.TrimSpace(getenv(KubeCAEnvPrefix + suffix))
		mt := strings.TrimSpace(getenv(KubeMinterEnvPrefix + suffix))
		for _, v := range []struct{ prefix, val string }{{KubeEndpointEnvPrefix, ep}, {KubeCAEnvPrefix, ca}, {KubeMinterEnvPrefix, mt}} {
			if v.val == "" {
				return nil, fmt.Errorf("kube.clusters[%s]: %s%s is not set (the cluster's sealed configuration)", name, v.prefix, suffix)
			}
		}
		if h, p, err := net.SplitHostPort(ep); err != nil || h == "" || p == "" || strings.ContainsAny(ep, "/?#@ ") {
			return nil, fmt.Errorf("kube.clusters[%s]: %s%s must be host:port (no scheme, no path)", name, KubeEndpointEnvPrefix, suffix)
		}
		if !x509.NewCertPool().AppendCertsFromPEM([]byte(ca)) {
			return nil, fmt.Errorf("kube.clusters[%s]: %s%s holds no PEM certificate (the cluster's CA bundle)", name, KubeCAEnvPrefix, suffix)
		}
		// nil (unset, blank or empty) disables the cluster; malformed refuses.
		allow, err := kuberead.ParseAllowlist([]byte(getenv(KubeAllowlistEnvPrefix + suffix)))
		if err != nil {
			return nil, fmt.Errorf("kube.clusters[%s]: %s%s: %v", name, KubeAllowlistEnvPrefix, suffix, err)
		}
		k.Clusters = append(k.Clusters, KubeCluster{Name: name, ServiceAccount: ns + "/" + sa, Endpoint: ep, CA: []byte(ca), Allow: allow, minter: mt})
	}
	return k, nil
}
