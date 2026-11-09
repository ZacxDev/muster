package previewgw

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The failed-token throttle, as muster's mirror connect has it
// (docs/mirroring.md, "Tokens, revocation, rate limits"): only FAILURES count,
// a valid token is never refused by it, and a peer past the limit gets 429
// instead of 401 for its failures. The token's 256 bits are the guessing
// defence; the throttle tells a misconfigured host it is hammering.
const (
	// FailLimit failed token checks per FailWindow per peer; the next is 429.
	FailLimit  = 10
	FailWindow = time.Minute
)

// Path is which listener a tunnel arrived on.
type Path string

const (
	PathMesh   Path = "mesh"
	PathPublic Path = "public"
)

// peerOf is the throttle's key for a request on a listener of path p. The
// mesh listener sits behind the relay, which sets X-Real-IP; the public one
// behind the edge, which sets CF-Connecting-IP. Without the header, the TCP
// peer.
//
// It is a THROTTLE key, not an identity: nothing else reads it.
func peerOf(p Path, r *http.Request) string {
	h := "X-Real-IP"
	if p == PathPublic {
		h = "CF-Connecting-IP"
	}
	if v := strings.TrimSpace(r.Header.Get(h)); v != "" && len(v) <= 64 {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// failLimiter counts failures per key in fixed windows.
type failLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu sync.Mutex
	by map[string]failWindow
}

type failWindow struct {
	start time.Time
	n     int
}

func newFailLimiter(limit int, window time.Duration, now func() time.Time) *failLimiter {
	return &failLimiter{limit: limit, window: window, now: now, by: map[string]failWindow{}}
}

// fail records one failure from key and reports whether it is past a limit
// (the 11th of a minute from one peer).
func (l *failLimiter) fail(key string) (over bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.by) > 4096 {
		for k, w := range l.by {
			if now.Sub(w.start) >= l.window {
				delete(l.by, k)
			}
		}
	}
	w, ok := l.by[key]
	if !ok || now.Sub(w.start) >= l.window {
		w = failWindow{start: now}
	}
	w.n++
	l.by[key] = w
	return w.n > l.limit
}
