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

// maxFailKeys bounds the limiter's map. Keys are cheap for an
// unauthenticated client to vary (a header on the public path), so past this
// many live keys a NEW key shares one overflow bucket: those peers are
// throttled together, the map stops growing, and a known key keeps its own.
const maxFailKeys = 4096

// overflowKey is the shared bucket's key; no peer key is empty.
const overflowKey = ""

// failLimiter counts failures per key in fixed windows.
//
// Expired keys are pruned at most once per window, when the map is full —
// never on every call, which with many keys made each failure a scan of the
// whole map under the mutex.
type failLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu        sync.Mutex
	by        map[string]failWindow
	lastPrune time.Time
	prunes    int // how many prune scans ran (tests)
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
	w, ok := l.by[key]
	if !ok && len(l.by) >= maxFailKeys {
		if l.lastPrune.IsZero() || now.Sub(l.lastPrune) >= l.window {
			l.lastPrune = now
			l.prunes++
			for k, kw := range l.by {
				if now.Sub(kw.start) >= l.window {
					delete(l.by, k)
				}
			}
		}
		if len(l.by) >= maxFailKeys {
			key = overflowKey
			w, ok = l.by[key]
		}
	}
	if !ok || now.Sub(w.start) >= l.window {
		w = failWindow{start: now}
	}
	w.n++
	l.by[key] = w
	return w.n > l.limit
}
