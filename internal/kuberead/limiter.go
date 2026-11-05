package kuberead

import (
	"sync"
	"time"
)

// Limiter allows at most n calls per key in any window (a sliding window: a
// call stops counting once it is window old). In memory: a restart forgets
// it, which can only allow a burst of one window's calls again.
type Limiter struct {
	n      int
	window time.Duration
	now    func() time.Time

	mu    sync.Mutex
	calls map[string][]time.Time
}

// NewLimiter is a limiter of n calls per window per key; now nil is time.Now.
func NewLimiter(n int, window time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{n: n, window: window, now: now, calls: map[string][]time.Time{}}
}

// Allow spends one call for key, or reports false (nothing spent) when key
// has made n calls in the last window.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	kept := l.calls[key][:0]
	for _, t := range l.calls[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.n {
		l.calls[key] = kept
		return false
	}
	l.calls[key] = append(kept, now)
	return true
}
