package officers

import (
	"errors"
	"time"
)

// RED-FIRST SKELETON for officer_kube_read's configuration and read budget:
// the exported surface the tests are written against, with no behaviour.

const (
	// ToolKubeRead is the judgement instances' cluster read tool.
	ToolKubeRead = "officer_kube_read"
	// KubeConnector is the connector its grant is filed for.
	KubeConnector = "kube-read"
	// The sealed environment each declared cluster is read from.
	KubeEndpointEnvPrefix = "MUSTER_KUBE_READ_ENDPOINT_"
	KubeCAEnvPrefix       = "MUSTER_KUBE_READ_CA_"
	KubeMinterEnvPrefix   = "MUSTER_KUBE_READ_MINTER_TOKEN_"
	// ReadsPerItem is the per-item read budget.
	ReadsPerItem = 200
)

// KubeCluster is one cluster an officer may read.
type KubeCluster struct {
	Name           string
	ServiceAccount string
	Endpoint       string
	CA             []byte
	minter         string
}

// MinterToken is the cluster's minter token.
func (k KubeCluster) MinterToken() string { return k.minter }

// KubeClusters is the clusters o's instances may read.
func (o Officer) KubeClusters() []KubeCluster { return nil }

// ErrReadBudget is a read over the per-item budget.
var ErrReadBudget = errors.New("officers: read budget spent")

// ReadBudget counts reads per item.
type ReadBudget struct{}

// NewReadBudget is a budget.
func NewReadBudget(perItem int, now func() time.Time) *ReadBudget { return &ReadBudget{} }

// Spend counts one read.
func (b *ReadBudget) Spend(officer string, item ItemID) (int, error) { return 0, nil }
