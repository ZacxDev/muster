package previewhost

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/preview/tunnel"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

// The host's dial to the gateway (docs/design/previews.md, "Host dial order:
// mesh first, then public"): -preview-urls in order, the first whose TCP
// connect, WebSocket upgrade and version answer complete within DialBudget
// taken; all failing, the list again after a jittered backoff. While on a
// later endpoint (the public path) the earlier ones are re-tried every
// MeshRetry; one that answers becomes the tunnel, and the one it replaces
// keeps serving its open streams until the gateway closes it (at most
// drainFor).

// Defaults of TunnelConfig.
const (
	DefaultDialBudget = 3 * time.Second
	DefaultMeshRetry  = 5 * time.Minute
	minBackoff        = time.Second
	maxBackoff        = time.Minute
	// drainFor bounds how long a replaced tunnel keeps serving: the gateway
	// closes it when its last stream ends, or after 60 s.
	drainFor = 70 * time.Second
	// stableFor is how long a tunnel must stay up for the backoff to reset.
	stableFor = time.Minute
)

// TunnelConfig is the host's tunnel: where and as whom it dials.
type TunnelConfig struct {
	// URLs is -preview-urls, in dial order (the mesh endpoint first).
	URLs []string
	// Token is the host's mpt_ token; HostName the name it was minted for.
	Token    string
	HostName string
	// DialBudget bounds each endpoint's TCP connect + upgrade + version
	// answer (default DefaultDialBudget). MeshRetry is how often a host on a
	// later endpoint re-tries the earlier ones (default DefaultMeshRetry).
	DialBudget time.Duration
	MeshRetry  time.Duration
	// PingInterval is the tunnel's WebSocket ping (0 = wire.PingInterval).
	PingInterval time.Duration

	// Tests only: the timer the backoff and the mesh retry wait on (nil =
	// time.After), and the dial (nil = tunnel.DialHost).
	after func(time.Duration) <-chan time.Time
	dial  func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)
}

func (tc *TunnelConfig) defaults() {
	if tc.DialBudget <= 0 {
		tc.DialBudget = DefaultDialBudget
	}
	if tc.MeshRetry <= 0 {
		tc.MeshRetry = DefaultMeshRetry
	}
	if tc.after == nil {
		tc.after = time.After
	}
	if tc.dial == nil {
		tc.dial = tunnel.DialHost
	}
}

// Run serves previews until ctx ends: it polls previews.json and the
// listeners, and keeps a tunnel to the gateway up. It returns only when ctx
// ends; nothing it meets is fatal.
func (h *Host) Run(ctx context.Context, tc TunnelConfig) {
	tc.defaults()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer h.recoverAs("the preview poll")
		h.poll(ctx)
	}()
	defer wg.Wait()
	backoff := minBackoff
	for ctx.Err() == nil {
		up := time.Now()
		if err := h.connectAndServe(ctx, tc); err != nil && ctx.Err() == nil {
			h.logger.Printf("muster-mirror: previews: %v", err)
		}
		if time.Since(up) >= stableFor {
			backoff = minBackoff
		}
		wait := backoff/2 + rand.N(backoff/2+1)
		backoff = min(backoff*2, maxBackoff)
		select {
		case <-ctx.Done():
		case <-tc.after(wait):
		}
	}
}

func (h *Host) recoverAs(what string) {
	if r := recover(); r != nil {
		h.logger.Printf("muster-mirror: previews: %s panicked (mirroring continues): %v", what, r)
	}
}

// dialList tries urls in order, each within the budget, and returns the
// first tunnel up and its index.
func (h *Host) dialList(ctx context.Context, tc TunnelConfig, urls []string) (*websocket.Conn, int, error) {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+tc.Token)
	hdr.Set(wire.VersionHeader, strconv.Itoa(wire.Version))
	hdr.Set(wire.HostNameHeader, tc.HostName)
	var errs []error
	for i, u := range urls {
		dctx, cancel := context.WithTimeout(ctx, tc.DialBudget)
		ws, resp, err := tc.dial(dctx, u, hdr)
		cancel()
		if err == nil {
			return ws, i, nil
		}
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		errs = append(errs, fmt.Errorf("%s: %s", u, dialRefusal(resp, err)))
	}
	return nil, 0, fmt.Errorf("no gateway endpoint answered: %w", errors.Join(errs...))
}

// dialRefusal says why a dial failed, without the token.
func dialRefusal(resp *http.Response, err error) string {
	if resp == nil {
		return err.Error()
	}
	if r := resp.Header.Get(wire.VersionRangeHeader); r != "" {
		return fmt.Sprintf("the gateway speaks preview protocol versions %s, this host %d", r, wire.Version)
	}
	return fmt.Sprintf("refused with %d", resp.StatusCode)
}

// connectAndServe dials the list and serves the tunnel it got until that
// tunnel ends; on a later endpoint it re-tries the earlier ones every
// MeshRetry, and moves to one that answers. A changed maxConns ends the
// tunnel (its HTTP/2 SETTINGS are fixed), so the caller redials with it.
func (h *Host) connectAndServe(ctx context.Context, tc TunnelConfig) error {
	ws, idx, err := h.dialList(ctx, tc, tc.URLs)
	if err != nil {
		return err
	}
	var drains sync.WaitGroup
	defer drains.Wait()
	for {
		h.logger.Printf("muster-mirror: previews: tunnel up via %s", tc.URLs[idx])
		n, changed := h.maxConns()
		tctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func(ws *websocket.Conn) {
			err := errors.New("the tunnel panicked")
			defer func() {
				if r := recover(); r != nil {
					h.logger.Printf("muster-mirror: previews: the preview tunnel panicked (mirroring continues): %v", r)
				}
				done <- err
			}()
			err = tunnel.Serve(tctx, ws, h.Handler(), tunnel.HostOptions{MaxConns: n, PingInterval: tc.PingInterval, ErrorLog: h.logger})
		}(ws)
		var retry <-chan time.Time
		if idx > 0 {
			retry = tc.after(tc.MeshRetry)
		}
	wait:
		for {
			select {
			case err := <-done:
				cancel()
				return fmt.Errorf("the tunnel via %s ended: %w", tc.URLs[idx], err)
			case <-ctx.Done():
				cancel()
				<-done
				return nil
			case <-changed:
				if m, ch := h.maxConns(); m != n {
					cancel()
					<-done
					return fmt.Errorf("maxConns changed %d → %d: reconnecting", n, m)
				} else {
					changed = ch
				}
			case <-retry:
				nws, nidx, err := h.dialList(ctx, tc, tc.URLs[:idx])
				if err != nil {
					h.logger.Printf("muster-mirror: previews: staying on %s: %v", tc.URLs[idx], err)
					retry = tc.after(tc.MeshRetry)
					continue
				}
				// The replaced tunnel keeps its open streams until the
				// gateway closes it, never longer than drainFor.
				drains.Add(1)
				go func(cancel context.CancelFunc, done <-chan error) {
					defer drains.Done()
					select {
					case <-done:
					case <-ctx.Done():
						<-done
					case <-tc.after(drainFor):
						cancel()
						<-done
					}
					cancel()
				}(cancel, done)
				ws, idx = nws, nidx
				break wait
			}
		}
	}
}
