package connectors

import (
	"context"
	"fmt"
)

// SchemeKubeRead is the kube-read connector's scheme: a read of a Kubernetes
// API server that muster-server performs ITSELF (internal/kuberead, through
// the officer_kube_read tool). Like SchemePostgres it is never sent over HTTP
// by the proxy: the proxy refuses it.
const SchemeKubeRead Scheme = "kube-read"

// KubeReadName is the kube-read connector's name (officers.KubeConnector names
// the same connector for the officer configuration).
const KubeReadName = "kube-read"

// KubeReadTool is the one tool that uses the connector, named in the proxy's
// refusal.
const KubeReadTool = "officer_kube_read"

// KubeReadVirtual is the kube-read connector, presented through the store by
// WithVirtual so an officer instance's request for it is filed, shown on a
// request card and approved BY HAND like every other officer connector grant.
//
// 🔴 IT HOLDS NO CREDENTIAL AND HAS NO HTTP FACE. Each cluster's endpoint, CA
// and minter token are the server's sealed environment (the officer `kube`
// block, internal/officers), read only by officer_kube_read inside
// muster-server. Describe names no host, and the proxy refuses the scheme
// before any host check, so /connect/kube-read/… reaches nothing.
func KubeReadVirtual() Virtual { return kubeReadVirtual{} }

type kubeReadVirtual struct{}

func (kubeReadVirtual) Describe() Connector {
	return Connector{Name: KubeReadName, Platform: "kubernetes (read-only, run by muster-server)", Scheme: SchemeKubeRead,
		CreatedAt: VirtualCreatedAt}
}

func (kubeReadVirtual) Credential(context.Context, Caller, string, string) (string, bool, error) {
	return "", false, fmt.Errorf("%w: %s is served by the %s tool only, never through the proxy", ErrTargetRefused, KubeReadName, KubeReadTool)
}

func (kubeReadVirtual) Rejected(Caller) {}

func (kubeReadVirtual) Problem(int64) string { return "" }

// IsKubeRead reports whether v is the kube-read connector. The callers that
// describe a virtual connector as the GitHub App branch on it first.
func IsKubeRead(v Virtual) bool { return v != nil && v.Describe().Scheme == SchemeKubeRead }

// AuditIDer is a store that returns an appended audit row's id (PGStore).
type AuditIDer interface {
	AuditID(ctx context.Context, e AuditEntry) (int64, error)
}

// AuditWithID appends e to s and returns its row id when s (or the store a
// virtual connector wraps) can say it, else 0 after a plain Audit. Kept off
// the Store interface on purpose: only officer_kube_read needs the id.
func AuditWithID(ctx context.Context, s Store, e AuditEntry) (int64, error) {
	for inner := s; ; {
		if a, ok := inner.(AuditIDer); ok {
			return a.AuditID(ctx, e)
		}
		vs, ok := inner.(*virtualStore)
		if !ok {
			break
		}
		inner = vs.Store
	}
	return 0, s.Audit(ctx, e)
}
