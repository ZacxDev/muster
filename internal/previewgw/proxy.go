package previewgw

// proxy.go — the reverse proxy behind an admitted p-<id> request
// (docs/design/previews.md, "The proxy"): httputil.ReverseProxy, flushing
// every write, over a CONNECT stream on the preview's host's tunnel. HTTP/1.1
// to the app; request and response bodies stream both ways, never buffered
// whole. WebSockets are wsrelay.go, the quiet-SSE keepalive sse.go.
//
//   - Request: Host localhost:<port> (dev servers allow it by default); the
//     public host in X-Forwarded-Host, X-Forwarded-Proto https. Removed: the
//     gateway's cookies and muster_* cookies, Forwarded, every inbound
//     X-Forwarded-*, X-Real-IP, CF-*, True-Client-IP, CDN-Loop, X-Muster-*.
//     🔴 THE VIEWER'S ADDRESS IS NOT FORWARDED. The app's own cookies and
//     Authorization pass. An Origin equal to the preview's own becomes
//     http://localhost:<port>.
//   - Response: a Location at localhost / 127.0.0.1 / [::1] on the preview's
//     port is rewritten to the preview's origin; every Set-Cookie loses
//     Domain, and one named like a gateway cookie is dropped; frame-ancestors
//     with muster's origin (a second CSP header, which a browser intersects
//     with the app's); nosniff; Referrer-Policy no-referrer, REPLACING the
//     app's; the app's Strict-Transport-Security dropped.
//   - Errors: the gateway's own (host offline, nothing listening, timeouts)
//     are a page naming a code (^[a-z_]{1,32}$) and nothing else — never an
//     error's text. An app's own error is the app's response, as it sent it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/preview/tunnel"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

const (
	// UpstreamHeaderTimeout bounds the wait for an app's response headers
	// (the design's "upstream headers" limit).
	UpstreamHeaderTimeout = 60 * time.Second
	// StreamKeepalive is the browser-leg WebSocket ping interval and the
	// quiet-SSE comment interval: well under the edge's ~125 s idle cut.
	StreamKeepalive = 30 * time.Second
	// Upstream connection pool, per tunnel. Every pooled connection holds one
	// of the host's maxConns streams, so the pool is a fraction of the
	// smallest useful maxConns budget and lets go of idle ones quickly.
	maxIdleUpstream           = 16
	maxIdleUpstreamPerPreview = 4
	upstreamIdleTimeout       = 30 * time.Second
)

// Why a proxied request's context ended early.
var (
	errPreviewClosed  = errors.New("previewgw: the preview closed")
	errSessionExpired = errors.New("previewgw: the owner cookie expired")
)

// proxyTarget is where one admitted request goes, carried in its context.
type proxyTarget struct {
	id   string
	port int
	t    *Tunnel
}

type targetKey struct{}

func targetOf(ctx context.Context) *proxyTarget {
	tg, _ := ctx.Value(targetKey{}).(*proxyTarget)
	return tg
}

// upstreamHost is the Host header the app sees.
func (tg *proxyTarget) upstreamHost() string { return "localhost:" + strconv.Itoa(tg.port) }

// poolAddr is the outbound URL's host: the transport's pool key, never
// resolved (the dialer takes only the port from it).
func (tg *proxyTarget) poolAddr() string { return tg.id + ":" + strconv.Itoa(tg.port) }

// newUpstreamTransport is the proxy's HTTP/1.1 transport on t: every
// connection is a CONNECT stream on t. No proxy, no compression negotiation,
// the upstream-headers timeout.
func newUpstreamTransport(t *Tunnel) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			_, ps, err := net.SplitHostPort(addr)
			port, perr := strconv.Atoi(ps)
			if err != nil || perr != nil {
				return nil, fmt.Errorf("previewgw: bad upstream address %q", addr)
			}
			return t.dial(ctx, port)
		},
		DisableCompression:    true,
		ResponseHeaderTimeout: UpstreamHeaderTimeout,
		MaxIdleConns:          maxIdleUpstream,
		MaxIdleConnsPerHost:   maxIdleUpstreamPerPreview,
		IdleConnTimeout:       upstreamIdleTimeout,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (g *Gateway) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: g.rewrite,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return targetOf(r.Context()).t.rt.RoundTrip(r)
		}),
		FlushInterval:  -1,
		ModifyResponse: g.modifyResponse,
		ErrorHandler:   g.proxyError,
		ErrorLog:       slog.NewLogLogger(g.log.Handler(), slog.LevelDebug),
	}
}

// dropRequestHeader names an inbound header the app never sees.
func dropRequestHeader(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "forwarded", "x-real-ip", "true-client-ip", "cdn-loop":
		return true
	}
	return strings.HasPrefix(n, "x-forwarded-") || strings.HasPrefix(n, "cf-") || strings.HasPrefix(n, "x-muster-")
}

// upstreamHeaders rewrites h (a copy of the browser's headers) into what the
// app may see, for target tg (the request rules in this file's comment).
func (g *Gateway) upstreamHeaders(h http.Header, tg *proxyTarget) {
	for k := range h {
		if dropRequestHeader(k) {
			delete(h, k)
		}
	}
	if c := forwardCookies(h); c != "" {
		h.Set("Cookie", c)
	} else {
		h.Del("Cookie")
	}
	if h.Get("Origin") == g.previewOrigin(tg.id) {
		h.Set("Origin", "http://"+tg.upstreamHost())
	}
	h.Set("X-Forwarded-Host", "p-"+tg.id+"."+g.cfg.Domain)
	h.Set("X-Forwarded-Proto", "https")
}

func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	tg := targetOf(pr.In.Context())
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = tg.poolAddr()
	pr.Out.Host = tg.upstreamHost()
	g.upstreamHeaders(pr.Out.Header, tg)
}

// previewResponseHeaders applies the response rules to h, an app's headers.
func (g *Gateway) previewResponseHeaders(h http.Header, tg *proxyTarget) {
	h.Del("Strict-Transport-Security")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Add("Content-Security-Policy", "frame-ancestors "+frameAncestor(g.cfg.PublicURL))
	if loc := h.Get("Location"); loc != "" {
		h.Set("Location", g.rewriteLocation(loc, tg))
	}
	if sc := h.Values("Set-Cookie"); len(sc) > 0 {
		h.Del("Set-Cookie")
		for _, v := range sc {
			if v = appSetCookie(v); v != "" {
				h.Add("Set-Cookie", v)
			}
		}
	}
}

func (g *Gateway) modifyResponse(resp *http.Response) error {
	g.previewResponseHeaders(resp.Header, targetOf(resp.Request.Context()))
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(strings.ToLower(ct), "text/event-stream") {
		resp.Body = newSSEBody(resp.Body, g.keepalive)
	}
	return nil
}

// rewriteLocation points a Location at the app's own loopback address and
// port to the preview's origin; any other Location is left as it is.
func (g *Gateway) rewriteLocation(loc string, tg *proxyTarget) string {
	u, err := url.Parse(loc)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Port() != strconv.Itoa(tg.port) {
		return loc
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
	default:
		return loc
	}
	u.Scheme = "https"
	u.Host = "p-" + tg.id + "." + g.cfg.Domain
	return u.String()
}

// appSetCookie is an app's Set-Cookie without its Domain attribute, so every
// cookie an app sets is host-only; "" for one named like a gateway cookie.
// The other attributes pass as the app wrote them.
func appSetCookie(v string) string {
	parts := strings.Split(v, ";")
	name, _, ok := strings.Cut(parts[0], "=")
	if !ok || gatewayCookie(strings.TrimSpace(name)) {
		return ""
	}
	keep := parts[:1]
	for _, a := range parts[1:] {
		k, _, _ := strings.Cut(a, "=")
		if strings.EqualFold(strings.TrimSpace(k), "domain") {
			continue
		}
		keep = append(keep, a)
	}
	return strings.Join(keep, ";")
}

// upstreamError is the code and status for an error reaching the app.
func upstreamError(ctx context.Context, err error) (int, string) {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errPreviewClosed):
		return http.StatusNotFound, codePreviewClosed
	case errors.Is(cause, errSessionExpired):
		return http.StatusForbidden, codeSessionExpired
	}
	var re *tunnel.RefusedError
	var ne net.Error
	switch {
	case errors.Is(err, ErrHostOffline), errors.Is(err, tunnel.ErrClosed), errors.Is(err, tunnel.ErrPeerUnresponsive):
		return http.StatusServiceUnavailable, codeHostOffline
	case errors.As(err, &re):
		switch re.Reason {
		case wire.ReasonMaxConns:
			return http.StatusTooManyRequests, string(re.Reason)
		case wire.ReasonPortNotAllowed, wire.ReasonForeignListener:
			return http.StatusForbidden, string(re.Reason)
		}
		return http.StatusBadGateway, codeNotAnswering
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return http.StatusGatewayTimeout, codeUpstreamTimeout
	}
	return http.StatusBadGateway, codeNotAnswering
}

func (g *Gateway) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	st, code := upstreamError(r.Context(), err)
	g.renderCode(w, st, code)
}

// serveAdmitted proxies a request the owner cookie admitted (expiring at
// exp) to preview id. The request runs under a context that ends when the
// preview closes or the cookie expires, whichever is first, so a long-lived
// response or WebSocket is cut then — not at a later check.
func (g *Gateway) serveAdmitted(w http.ResponseWriter, r *http.Request, id string, exp time.Time) {
	p, ok := g.useBegin(id)
	if !ok {
		g.renderCode(w, http.StatusNotFound, codePreviewClosed)
		return
	}
	defer g.useEnd(id)
	g.mu.Lock()
	t := g.byKey[tunnelKey{p.OwnerID, p.Host}]
	g.mu.Unlock()
	if t == nil {
		g.renderCode(w, http.StatusServiceUnavailable, codeHostOffline)
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	defer context.AfterFunc(p.ctx, func() { cancel(errPreviewClosed) })()
	defer g.clock.AfterFunc(exp.Sub(g.clock.Now()), func() { cancel(errSessionExpired) })()
	tg := &proxyTarget{id: id, port: p.Port, t: t}
	r = r.WithContext(context.WithValue(ctx, targetKey{}, tg))
	switch {
	case isWebSocket(r):
		g.relayWebSocket(w, r, tg)
	case headerHasToken(r.Header, "Connection", "upgrade"):
		// Only WebSocket upgrades are relayed (and Origin-checked).
		g.renderCode(w, http.StatusBadRequest, codeBadUpgrade)
	default:
		g.proxy.ServeHTTP(w, r)
	}
}
