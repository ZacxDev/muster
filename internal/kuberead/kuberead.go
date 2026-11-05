// Package kuberead is officer_kube_read's engine: one typed, read-only
// request against a named cluster's API server, run by muster-server itself
// (docs/officers.md "Reading a cluster: officer_kube_read").
//
// 🔴 THE MODEL NEVER SENDS A URL, A PATH OR AN ADDRESS. It names a cluster
// (whose endpoint, CA and minter token are the server's sealed environment)
// and a typed request — {verb: get|list, group, version, resource, namespace?,
// name?, label_selector?, field_selector?, limit?} or {log: {namespace, pod,
// container?, previous?, since_seconds?, tail_lines?}} — and [Allowlist.Build]
// is the ONE place a request becomes a path: a GET under /api or /apis for a
// resource on the allowlist (allowlist.json), or a pod's /log. watch is never
// offered.
//
// 🔴 A FRESH, SHORT-LIVED IDENTITY PER CALL. Each Read mints a TokenSeconds
// TokenRequest for the officer's ServiceAccount with the cluster's minter
// token, then reads with that token alone; the minter token never reads. The
// minted token's jti goes in the audit record.
//
// 🔴 REFUSED, NOT TRUNCATED. An answer over MaxAnswerBytes or a list over
// MaxObjects is a refusal saying so — a partial answer would read as the whole.
package kuberead

import (
	"errors"
	"time"
)

// Limits and fixed values.
const (
	// MaxAnswerBytes bounds one answer as returned, after stripping.
	MaxAnswerBytes = 1 << 20
	// maxRawBytes bounds what is read of one API answer before stripping
	// (managedFields can be most of a list's bytes).
	maxRawBytes = 8 << 20
	// MaxObjects bounds one list: more is a refusal.
	MaxObjects = 500
	// MaxTailLines and DefaultTailLines bound a pod log read.
	MaxTailLines     = 2000
	DefaultTailLines = 500
	// MaxSinceSeconds bounds a pod log read's window: a day.
	MaxSinceSeconds = 86400
	// MaxLogLineRunes caps one scrubbed log line.
	MaxLogLineRunes = 1000
	// CallsPerMinute is each officer's call budget (Limiter).
	CallsPerMinute = 30
	// TokenSeconds is a minted token's life: the API server's minimum.
	TokenSeconds = 600
	// ServerName is the name every API server certificate carries, verified
	// whatever address the endpoint is.
	ServerName = "kubernetes.default.svc"
	// requestTimeout bounds one mint or one read.
	requestTimeout = 30 * time.Second
)

// ErrRefused wraps every refusal of a request's shape, before any network.
var ErrRefused = errors.New("kuberead: refused")
