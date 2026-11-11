package previewgw

import (
	"net"
	"net/http"
	"net/netip"
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

// peerOf is the throttle's key for a request on a listener of path p: the TCP
// peer, unless that peer is in trusted (PREVIEW_TRUSTED_PROXIES), the relay in
// front of the gateway, whose client-address header then names the client:
// X-Real-IP on the mesh listener, CF-Connecting-IP on the public one. Without
// the header, the TCP peer.
//
// 🔴 A HEADER FROM ANY OTHER PEER IS IGNORED. Anything that reaches a
// listener directly (an in-cluster client, past the relay) could otherwise set
// the header itself and rotate it, a fresh bucket per guess, and the throttle
// would never answer it 429.
//
// It is a THROTTLE key, not an identity: nothing else reads it.
func peerOf(p Path, r *http.Request, trusted []netip.Prefix) string {
	peer := tcpPeer(r.RemoteAddr)
	if a, err := netip.ParseAddr(peer); err == nil && inPrefixes(a.Unmap(), trusted) {
		h := "X-Real-IP"
		if p == PathPublic {
			h = "CF-Connecting-IP"
		}
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" && len(v) <= 64 {
			return v
		}
	}
	return peer
}

// tcpPeer is a RemoteAddr's host, or the whole of it when it has no port.
func tcpPeer(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
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
