package main

import (
	"sync"
	"time"
)

// The model credential's state, as REAL turns have shown it. It is reported on
// /healthz and never gates it (see handleHealthz).
//
// 🔴 THERE IS NO PROBE. ccd never spends a request of its own to find out whether
// the credential works: a probe turn costs subscription tokens on the same limit
// the operator's sessions use, and a probe process racing an interactive /login
// credential refresh can clobber it. The state below is therefore only as fresh
// as the last turn — `unknown` until one has run.
const (
	authUnknown     = "unknown"
	authOK          = "ok"
	authFailed      = failAuth        // "auth_failed": the account needs a new token
	authRateLimited = failRateLimited // "rate_limited": the account is healthy and resting
)

type authSnapshot struct {
	Auth       string     `json:"auth"`
	AuthDetail string     `json:"auth_detail,omitempty"`
	AuthAt     *time.Time `json:"auth_at,omitempty"` // when the last turn that said anything about it ended
}

// authTracker holds the latest verdict on the model credential.
type authTracker struct {
	mu   sync.Mutex
	snap authSnapshot
	now  func() time.Time
}

func newAuthTracker() *authTracker {
	return &authTracker{snap: authSnapshot{Auth: authUnknown}, now: time.Now}
}

func (a *authTracker) snapshot() authSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snap
}

func (a *authTracker) set(state, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.snap = authSnapshot{Auth: state, AuthDetail: detail, AuthAt: &now}
}

// observeTurn records what a real turn proved: nil = it succeeded (an
// authenticated round trip); an auth or rate-limit failure says that about the
// credential. Other failures say nothing about the credential and leave the state
// alone. Callers: runTurn (a ccd-driven turn) and onHook's StopFailure (any turn,
// including one typed in the attached terminal).
func (a *authTracker) observeTurn(f *failure) {
	switch {
	case f == nil:
		a.set(authOK, "")
	case f.Type == failAuth || f.Type == failRateLimited:
		a.set(f.Type, f.Message)
	}
}
