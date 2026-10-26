package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
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
//   - each TAB (user and Faro session) may post faroRelayBurst batches per
//     faroRelayWindow, each user faroRelayUserBurst across its tabs, then 429
//     with a Retry-After of a few seconds;
//   - only the body and its content type cross: no cookie, no header of the
//     browser's, and the upstream's answer is reduced to its status.
//
// ⚠ IT DOES NOT FILTER THE BODY. It reads one field of it — the Faro session
// id, the key of a tab's rate window — and forwards the rest as sent. What the
// page puts in a batch is the page's (ui's faro-init.js scrubs URLs and
// exceptions before it sends); the relay is a pipe with a size and a rate.
// ---------------------------------------------------------------------------

// FaroRelayPath is the relay's route: what the page's Faro posts to when a
// relay upstream is configured.
const FaroRelayPath = "/telemetry/faro"

const (
	// faroRelayMaxBody bounds one batch. Faro's own batches are a few KiB (its
	// transport sends at most 50 items at a time); 256 KiB leaves room for a
	// batch of spans and refuses anything shaped like an upload.
	faroRelayMaxBody = 256 << 10
	// faroRelayBurst batches per faroRelayWindow per TAB (a user's Faro
	// session): the page batches every 5 s (faro.go), so one open view sends
	// about a dozen a minute; this is ten times that.
	faroRelayBurst = 120
	// faroRelayUserBurst is the ceiling across ALL of one user's tabs, so a
	// page cannot mint session ids to multiply its share: twenty tabs' worth.
	faroRelayUserBurst = 600
	faroRelayWindow    = time.Minute
	// faroRelayRetryAfter is what a refused page is told to wait. Faro drops
	// everything until then — a disconnect event included — so it is short:
	// the window is a ceiling on a runaway page, not a penalty.
	faroRelayRetryAfter = "5"
	// faroRelayTimeout bounds one forward. A receiver that is down costs the
	// page one short-lived request, never a held connection.
	faroRelayTimeout = 5 * time.Second
	// faroRelayLogEvery is the most often a failed forward is logged; the
	// failures between are counted into the next line.
	faroRelayLogEvery = time.Minute
)

// relayKey is one tab's window: its user and its Faro session id.
type relayKey struct {
	user    int64
	session string
}

type faroRelay struct {
	upstream, key string
	client        *http.Client

	mu      sync.Mutex
	per     map[relayKey]*rateLimiter
	perUser map[int64]*rateLimiter

	// The failed-forward log's own limit: the last line's time and the
	// failures since it.
	logMu     sync.Mutex
	loggedAt  time.Time
	suppressd int
}

// SetFaroRelay turns the relay on, forwarding to upstream with key as its
// x-api-key ("" for none). An empty upstream leaves it off: the route answers
// 404, like a route that does not exist.
func (s *Server) SetFaroRelay(upstream, key string) {
	if upstream == "" {
		s.faro = nil
		return
	}
	s.faro = &faroRelay{upstream: upstream, key: key, per: map[relayKey]*rateLimiter{}, perUser: map[int64]*rateLimiter{},
		client: &http.Client{Timeout: faroRelayTimeout,
			// The receiver is named by the operator; a redirect elsewhere is
			// not followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// limiterFor is m's window for k, dropping lapsed ones as new keys arrive
// (so the maps stay as small as the set of recently active tabs). Under f.mu.
func limiterFor[K comparable](m map[K]*rateLimiter, k K, now time.Time) *rateLimiter {
	l := m[k]
	if l != nil {
		return l
	}
	for kk, w := range m {
		w.mu.Lock()
		stale := now.Sub(w.windowStart) >= faroRelayWindow
		w.mu.Unlock()
		if stale {
			delete(m, kk)
		}
	}
	l = &rateLimiter{}
	m[k] = l
	return l
}

// allow is one batch of user's tab session against the tab's window and the
// user's ceiling.
func (f *faroRelay) allow(user int64, session string, now time.Time) bool {
	f.mu.Lock()
	tab, all := limiterFor(f.per, relayKey{user, session}, now), limiterFor(f.perUser, user, now)
	f.mu.Unlock()
	return tab.allow(now, faroRelayBurst, faroRelayWindow) && all.allow(now, faroRelayUserBurst, faroRelayWindow)
}

// logFailure logs a failed forward at most once per faroRelayLogEvery,
// carrying how many were not logged since the last line.
func (f *faroRelay) logFailure(logger interface{ Printf(string, ...any) }, now time.Time, what string) {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	if !f.loggedAt.IsZero() && now.Sub(f.loggedAt) < faroRelayLogEvery {
		f.suppressd++
		return
	}
	logger.Printf("telemetry: faro relay: %s (%d more failed forward(s) not logged since the last line)", what, f.suppressd)
	f.loggedAt, f.suppressd = now, 0
}

// faroSession is the batch's Faro session id (meta.session.id), cut to 64
// bytes; "" when it has none. Only the key of the rate window: nothing else
// in the batch is read.
func faroSession(body []byte) string {
	var b struct {
		Meta struct {
			Session struct {
				ID string `json:"id"`
			} `json:"session"`
		} `json:"meta"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	id := b.Meta.Session.ID
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}

// handleFaroRelay is POST /telemetry/faro.
func (s *Server) handleFaroRelay(w http.ResponseWriter, r *http.Request) {
	f := s.faro
	if f == nil {
		http.NotFound(w, r)
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
	if !f.allow(requestOwner(r.Context()), faroSession(body), s.now()) {
		w.Header().Set("Retry-After", faroRelayRetryAfter)
		http.Error(w, "too many telemetry posts", http.StatusTooManyRequests)
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
		f.logFailure(s.logger, s.now(), "the receiver did not answer: "+err.Error())
		http.Error(w, "the telemetry receiver did not answer", http.StatusBadGateway)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		f.logFailure(s.logger, s.now(), "the receiver answered "+strconv.Itoa(resp.StatusCode))
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
