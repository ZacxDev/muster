// Package kuberead is officer_kube_read's engine: typed read requests against
// a named cluster's API server, run by muster-server itself.
//
// RED-FIRST SKELETON: the exported surface the tests are written against,
// with no behaviour. The implementation replaces it.
package kuberead

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

//go:embed allowlist.json
var allowlistJSON []byte

// Limits.
const (
	MaxAnswerBytes  = 1 << 20
	MaxObjects      = 500
	MaxTailLines    = 2000
	MaxSinceSeconds = 86400
	MaxLogLineRunes = 1000
	CallsPerMinute  = 30
	TokenSeconds    = 600
	ServerName      = "kubernetes.default.svc"
)

// ErrRefused wraps every refusal of a request's shape.
var ErrRefused = errors.New("kuberead: refused")

// Entry is one allowlisted (group, resource).
type Entry struct {
	Group    string `json:"group"`
	Resource string `json:"resource"`
	Scope    string `json:"scope"`
}

// Allowlist is the resources muster builds paths for.
type Allowlist struct{ entries []Entry }

// Default is the embedded allowlist.
func Default() *Allowlist { return &Allowlist{} }

// Entries is the allowlist's entries.
func (a *Allowlist) Entries() []Entry { return nil }

// Request is officer_kube_read's arguments.
type Request struct {
	Cluster       string      `json:"cluster"`
	Verb          string      `json:"verb,omitempty"`
	Group         string      `json:"group"`
	Version       string      `json:"version,omitempty"`
	Resource      string      `json:"resource,omitempty"`
	Namespace     string      `json:"namespace,omitempty"`
	Name          string      `json:"name,omitempty"`
	LabelSelector string      `json:"label_selector,omitempty"`
	FieldSelector string      `json:"field_selector,omitempty"`
	Limit         int         `json:"limit,omitempty"`
	Log           *LogRequest `json:"log,omitempty"`
}

// LogRequest is a pod log read.
type LogRequest struct {
	Namespace    string `json:"namespace"`
	Pod          string `json:"pod"`
	Container    string `json:"container,omitempty"`
	Previous     bool   `json:"previous,omitempty"`
	SinceSeconds int    `json:"since_seconds,omitempty"`
	TailLines    int    `json:"tail_lines,omitempty"`
}

// ParseRequest decodes the tool's arguments.
func ParseRequest(raw []byte) (Request, error) {
	var r Request
	err := json.Unmarshal(raw, &r)
	return r, err
}

// Built is a request's GET path and query.
type Built struct {
	Path  string
	Query url.Values
	List  bool
	Log   bool
}

// Build builds r's GET path.
func (a *Allowlist) Build(r Request) (Built, error) { return Built{Query: url.Values{}}, nil }

// Cluster is one cluster's API server, from the sealed configuration.
type Cluster struct{ Name, Endpoint string }

// NewCluster is a cluster.
func NewCluster(name, endpoint string, caPEM []byte, minterToken string) (Cluster, error) {
	return Cluster{Name: name, Endpoint: endpoint}, nil
}

// ServiceAccount names the officer's ServiceAccount on a cluster.
type ServiceAccount struct{ Namespace, Name string }

// Audit is one read's audit record.
type Audit struct {
	Cluster       string `json:"cluster"`
	Verb          string `json:"verb"`
	Group         string `json:"group"`
	Version       string `json:"version"`
	Resource      string `json:"resource"`
	Namespace     string `json:"namespace,omitempty"`
	Name          string `json:"name,omitempty"`
	LabelSelector string `json:"label_selector,omitempty"`
	FieldSelector string `json:"field_selector,omitempty"`
	Path          string `json:"path"`
	JTI           string `json:"jti,omitempty"`
	Status        int    `json:"status,omitempty"`
	Count         int    `json:"count"`
	Bytes         int    `json:"bytes"`
	Millis        int64  `json:"ms"`
	Refusal       string `json:"refusal,omitempty"`
}

// Reader runs reads.
type Reader struct{ allow *Allowlist }

// NewReader is a reader over allow.
func NewReader(allow *Allowlist) *Reader { return &Reader{allow: allow} }

// Read runs one read.
func (r *Reader) Read(ctx context.Context, c Cluster, sa ServiceAccount, req Request) (json.RawMessage, Audit, error) {
	return nil, Audit{}, nil
}

// Limiter bounds calls per key per window.
type Limiter struct{}

// NewLimiter is a limiter.
func NewLimiter(n int, per time.Duration, now func() time.Time) *Limiter { return &Limiter{} }

// Allow spends one call for key.
func (l *Limiter) Allow(key string) bool { return true }
