package officers

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
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
// the server's SEALED environment, never the declaration:
//
//	MUSTER_KUBE_READ_ENDPOINT_<CLUSTER>, MUSTER_KUBE_READ_CA_<CLUSTER>,
//	MUSTER_KUBE_READ_MINTER_TOKEN_<CLUSTER>
//
// (<CLUSTER> is the name as TokenEnvSuffix spells an id: prod-a -> PROD_A).
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
	KubeEndpointEnvPrefix = "MUSTER_KUBE_READ_ENDPOINT_"
	KubeCAEnvPrefix       = "MUSTER_KUBE_READ_CA_"
	KubeMinterEnvPrefix   = "MUSTER_KUBE_READ_MINTER_TOKEN_"
	// MaxKubeClusters bounds kube.clusters.
	MaxKubeClusters = 8
)

// Kube is an officer's `kube` block.
type Kube struct {
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

// KubeClusters is the clusters o's instances may read: the `kube` block's,
// for the judgement pool of an officer that declares one; nil otherwise.
func (o Officer) KubeClusters() []KubeCluster {
	if o.Kube == nil || o.Tier != TierJudgement {
		return nil
	}
	return append([]KubeCluster(nil), o.Kube.Clusters...)
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
		k.Clusters = append(k.Clusters, KubeCluster{Name: name, ServiceAccount: ns + "/" + sa, Endpoint: ep, CA: []byte(ca), minter: mt})
	}
	return k, nil
}

// ---------------------------------------------------------------------------
// THE PER-ITEM READ BUDGET (officers plan §4: 200 reads per item).
//
// ONE counter per (officer, item), spent by every read tool that names it —
// officer_kube_read today; the obs tools are meant to share it (the plan's
// MU2), which is why it lives here, beside them, and is keyed on the item and
// not on the tool.
//
// In memory: a restart forgets the counts, which can only allow more reads,
// never refuse one early. Entries untouched for a day are dropped.
// ---------------------------------------------------------------------------

// ReadsPerItem is the per-item read budget.
const ReadsPerItem = 200

// ErrReadBudget is a read refused because its item's budget is spent.
var ErrReadBudget = errors.New("officers: the item's read budget is spent")

// ReadBudget counts reads per (officer, item).
type ReadBudget struct {
	perItem int
	now     func() time.Time

	mu sync.Mutex
	m  map[readKey]*readCount
}

type readKey struct {
	officer string
	item    ItemID
}

type readCount struct {
	n    int
	last time.Time
}

// NewReadBudget is a budget of perItem reads per item; now nil is time.Now.
func NewReadBudget(perItem int, now func() time.Time) *ReadBudget {
	if now == nil {
		now = time.Now
	}
	return &ReadBudget{perItem: perItem, now: now, m: map[readKey]*readCount{}}
}

// Spend counts one read of item by officer and returns how many it has made,
// or refuses (ErrReadBudget, nothing counted) once perItem are spent.
func (b *ReadBudget) Spend(officer string, item ItemID) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for k, c := range b.m {
		if now.Sub(c.last) > 24*time.Hour {
			delete(b.m, k)
		}
	}
	k := readKey{officer, item}
	c, ok := b.m[k]
	if !ok {
		c = &readCount{}
		b.m[k] = c
	}
	if c.n >= b.perItem {
		return c.n, fmt.Errorf("%w: officer %s has made all %d of item %s's reads (cluster and observability reads together); "+
			"conclude from what you have, and list what you could not check", ErrReadBudget, officer, b.perItem, item)
	}
	c.n++
	c.last = now
	return c.n, nil
}
