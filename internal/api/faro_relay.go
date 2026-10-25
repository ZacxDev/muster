package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// THE BROWSER TELEMETRY RELAY: POST /telemetry/faro (docs/telemetry.md).
//
// The page's Faro SDK posts its batches (events, measurements, the browser's
// spans) HERE, same-origin, and this forwards each one to MUSTER_FARO_UPSTREAM
// — the Faro receiver, typically an Alloy `faro.receiver` on a private
// network. So the browser never talks to a LAN address, the CSP needs no
// other origin, and the receiver's key (MUSTER_FARO_API_KEY) stays on the
// server: the page is given none.
//
// 🔴 SIGNED-IN, SAME-ORIGIN, CAPPED AND RATE-LIMITED, because it is a door
// from the internet to a private receiver:
//   - requireOperatorWrite: a browser session, and a same-origin request — an
//     anonymous caller, a machine token and another site's page all stop here;
//   - a body over faroRelayMaxBody is 413, unread past the cap;
//   - each user may post faroRelayBurst batches per faroRelayWindow, then 429;
//   - only the body and its content type cross: no cookie, no header of the
//     browser's, and the upstream's answer is reduced to its status.
//
// ⚠ IT DOES NOT INSPECT THE BODY. What the page puts in a batch is the page's
// (ui's faroInitScript scrubs URLs of their query strings before it sends), and
// the relay is a pipe with a size and a rate, not a filter.
// ---------------------------------------------------------------------------

// FaroRelayPath is the relay's route: what the page's Faro posts to when a
// relay upstream is configured.
const FaroRelayPath = "/telemetry/faro"

const (
	// faroRelayMaxBody bounds one batch. Faro's own batches are a few KiB (its
	// transport sends at most 50 items at a time); 256 KiB leaves room for a
	// batch of spans and refuses anything shaped like an upload.
	faroRelayMaxBody = 256 << 10
	// faroRelayBurst batches per faroRelayWindow per user: a phone's page
	// sends a handful a minute; this is an order of magnitude above it.
	faroRelayBurst  = 120
	faroRelayWindow = time.Minute
	// faroRelayTimeout bounds one forward. A receiver that is down costs the
	// page one short-lived request, never a held connection.
	faroRelayTimeout = 5 * time.Second
)

// faroRelay is the relay's configuration and per-user windows.
type faroRelay struct {
	upstream, key string
	client        *http.Client

	mu  sync.Mutex
	per map[int64]*rateLimiter
}

// SetFaroRelay turns the relay on, forwarding to upstream with key as its
// x-api-key ("" for none). An empty upstream leaves it off: the route answers
// 404, like a route that does not exist.
func (s *Server) SetFaroRelay(upstream, key string) {
	if upstream == "" {
		s.faro = nil
		return
	}
	s.faro = &faroRelay{upstream: upstream, key: key, per: map[int64]*rateLimiter{},
		client: &http.Client{Timeout: faroRelayTimeout,
			// The receiver is named by the operator; a redirect elsewhere is
			// not followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// allow is user's next batch against its window.
func (f *faroRelay) allow(user int64, now time.Time) bool {
	f.mu.Lock()
	l := f.per[user]
	if l == nil {
		// The map holds one small window per user who has posted; a user
		// whose window has lapsed is dropped as others arrive, so it stays as
		// small as the set of recently active users.
		for u, w := range f.per {
			w.mu.Lock()
			stale := now.Sub(w.windowStart) >= faroRelayWindow
			w.mu.Unlock()
			if stale {
				delete(f.per, u)
			}
		}
		l = &rateLimiter{}
		f.per[user] = l
	}
	f.mu.Unlock()
	return l.allow(now, faroRelayBurst, faroRelayWindow)
}

// handleFaroRelay is POST /telemetry/faro.
func (s *Server) handleFaroRelay(w http.ResponseWriter, r *http.Request) {
	f := s.faro
	if f == nil {
		http.NotFound(w, r)
		return
	}
	if !f.allow(requestOwner(r.Context()), s.now()) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many telemetry posts", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, faroRelayMaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "the telemetry batch is too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read the batch", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), faroRelayTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.upstream, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "the relay is misconfigured", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if f.key != "" {
		req.Header.Set("X-API-Key", f.key)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		s.logger.Printf("telemetry: faro relay: the receiver did not answer: %v", err)
		http.Error(w, "the telemetry receiver did not answer", http.StatusBadGateway)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.logger.Printf("telemetry: faro relay: the receiver answered %d", resp.StatusCode)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
