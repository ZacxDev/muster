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
//     Authorization pass. The browser's Origin passes as it is, matching
//     X-Forwarded-Host and -Proto (an app comparing them for CSRF agrees);
//     ONLY ON A WEBSOCKET UPGRADE does an Origin equal to the preview's own
//     become http://localhost:<port> (wsrelay.go), for dev servers that
//     compare it with Host.
//   - Response: a Location at localhost / 127.0.0.1 / [::1] on the preview's
//     port is rewritten to the preview's origin; every Set-Cookie loses
//     Domain, and one named like a gateway cookie is dropped; frame-ancestors
//     with muster's origin (a second CSP header, which a browser intersects
//     with the app's); nosniff; Referrer-Policy no-referrer, REPLACING the
//     app's; the app's Strict-Transport-Security dropped. Also dropped: every
//     X-Muster-* header (an app cannot forge the gateway's error code) and
//     Clear-Site-Data, which a browser applies to the whole registrable
//     domain — every other preview, and w.
//   - 1xx: an app's informational response (103 Early Hints, a 100) reaches
//     the browser without the rules above — ReverseProxy forwards it from an
//     httptrace Got1xxResponse hook, before ModifyResponse exists for it — so
//     the Transport installs a hook of its own that removes every field
//     dropAppNonFinal names from the 1xx header IN PLACE, then adds the
//     gateway's own safety headers to it (addSafetyHeaders: nosniff,
//     Referrer-Policy no-referrer, frame-ancestors), as on a final response
//     (filter1xx). Ours is the newer trace on the outbound context, and
//     httptrace runs the newer hook first, on the same map, before
//     ReverseProxy's copies it to the browser (and clears what it copied, so
//     nothing lingers into the final response's headers). A Link passes.
//   - Trailers: an app's trailer named like a header these rules drop or
//     replace (dropAppNonFinal) is removed, declared (Trailer:) or not
//     (net/http's TrailerPrefix sends it undeclared), from both the Trailer
//     line the browser sees and the values after the body. ReverseProxy
//     copies trailers into the response after the body, past the rules
//     above, so without this an app's X-Muster-Preview-Error trailer would
//     read as the gateway's code.
//   - Errors: the gateway's own (host offline, nothing listening, timeouts)
//     are a page naming a code (^[a-z_]{1,32}$) and nothing else — never an
//     error's text. An app's own error is the app's response, as it sent it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/ZacxDev/muster/internal/preview/tunnel"
	"github.com/ZacxDev/muster/internal/preview/wire"
	"github.com/ZacxDev/muster/internal/telemetry"
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
	id    string
	port  int
	t     *Tunnel
	start time.Time // when the request took its slot (muster_preview_response_seconds)
	// upgraded: the browser's leg of a WebSocket was accepted (a 101 the
	// response writer never sees: the connection is hijacked).
	upgraded bool
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
			r = r.WithContext(httptrace.WithClientTrace(r.Context(), g.filter1xx()))
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
// app may see, for target tg (the request rules in this file's comment). ws
// is true for a WebSocket upgrade, the only request whose Origin is
// rewritten.
func (g *Gateway) upstreamHeaders(h http.Header, tg *proxyTarget, ws bool) {
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
	if ws && h.Get("Origin") == g.previewOrigin(tg.id) {
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
	g.upstreamHeaders(pr.Out.Header, tg, false)
}

// musterHeader is true for an X-Muster-* name: the gateway's alone.
func musterHeader(name string) bool { return strings.HasPrefix(strings.ToLower(name), "x-muster-") }

// dropAppNonFinal names an app field outside its final response header — a
// trailer, or a 1xx response's header — that never reaches the browser: an
// X-Muster-* one, and every name previewResponseHeaders drops or rewrites.
// Those rules run on the final header only, so there such a name would pass
// them untouched. (RFC 9110 §6.5.1 forbids most of these as trailers, and a
// browser acts on little of a 103 but its Link, so a conforming client
// ignores them there; the gateway does not rely on that.)
func dropAppNonFinal(name string) bool {
	if musterHeader(name) {
		return true
	}
	switch http.CanonicalHeaderKey(name) {
	case "Clear-Site-Data", "Strict-Transport-Security", "Referrer-Policy", "X-Content-Type-Options",
		"Content-Security-Policy", "Location", "Set-Cookie":
		return true
	}
	return false
}

func stripAppTrailers(tr http.Header) {
	for k := range tr {
		if dropAppNonFinal(k) {
			delete(tr, k)
		}
	}
}

// addSafetyHeaders adds the headers the gateway sets on every app response
// it relays, final or 1xx: nosniff, Referrer-Policy no-referrer (replacing
// the app's) and a frame-ancestors CSP with muster's origin (a second CSP
// header, intersected by a browser with the app's own).
func (g *Gateway) addSafetyHeaders(h http.Header) {
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Add("Content-Security-Policy", "frame-ancestors "+frameAncestor(g.cfg.PublicURL))
}

// filter1xx is the outbound request's trace: it removes the fields
// dropAppNonFinal names from an app's 1xx header in place, then adds the
// gateway's safety headers (addSafetyHeaders). The map is what ReverseProxy
// copies onto the browser's 1xx, and net/http's server writes a 1xx's header
// from the ResponseWriter's map, so a field set here is one the browser
// receives on the 1xx. (dropAppNonFinal removes the app's CSP first, so the
// 1xx carries the gateway's alone.) It must be the
// NEWER trace on the context than ReverseProxy's (installed in the
// Transport, after ReverseProxy installed its own): httptrace's compose calls
// the newer hook, then the older, with the same map, so this runs before
// ReverseProxy's hook copies the header to the browser. Its error is never
// returned (compose returns the older hook's), and it returns none.
// 🔴 A new trace per request: WithClientTrace composes INTO the trace it is
// given (it rewrites its hook fields), so a shared one would chain every
// request's ReverseProxy hook onto every later request.
func (g *Gateway) filter1xx() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{Got1xxResponse: func(_ int, h textproto.MIMEHeader) error {
		for k := range h {
			if dropAppNonFinal(k) {
				delete(h, k)
			}
		}
		g.addSafetyHeaders(http.Header(h))
		return nil
	}}
}

// trailerFilter strips resp's trailers once its body has ended. The
// transport fills resp.Trailer's values — and adds any undeclared trailer —
// inside the Read that reaches EOF (for an event stream, on sseBody's
// goroutine, ordered before sseBody hands the EOF on). So the strip runs when
// a Read here returns io.EOF: on the proxy's own goroutine, after every write
// to the map and before ReverseProxy copies it. 🔴 Never on Close: before
// EOF, sseBody's goroutine may still be inside src.Read writing the map (a
// browser that left mid-stream), and ReverseProxy copies no trailers then.
type trailerFilter struct {
	io.ReadCloser
	resp *http.Response
}

func (b *trailerFilter) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		stripAppTrailers(b.resp.Trailer)
	}
	return n, err
}

// previewResponseHeaders applies the response rules to h, an app's headers.
func (g *Gateway) previewResponseHeaders(h http.Header, tg *proxyTarget) {
	for k := range h {
		if musterHeader(k) {
			delete(h, k)
		}
	}
	h.Del("Clear-Site-Data")
	h.Del("Strict-Transport-Security")
	g.addSafetyHeaders(h)
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
	tg := targetOf(resp.Request.Context())
	g.m.responses.WithLabelValues(kindHost).Observe(time.Since(tg.start).Seconds())
	g.previewResponseHeaders(resp.Header, tg)
	// The declared trailers are keys already: ReverseProxy builds the
	// browser's Trailer: line from them right after this returns. Stripped
	// before any body wrapper exists, so nothing else can be touching
	// resp.Trailer yet (sseBody's goroutine starts on its first Read).
	stripAppTrailers(resp.Trailer)
	// The keepalive comment is plain text: inside a compressed body it would
	// corrupt the stream, so a body with a Content-Encoding other than
	// identity gets none.
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ce := resp.Header.Get("Content-Encoding"); strings.HasPrefix(ct, "text/event-stream") &&
		(ce == "" || strings.EqualFold(ce, "identity")) {
		resp.Body = newSSEBody(resp.Body, g.keepalive)
	}
	// Their values, and undeclared ones, come with the body's end. Outermost,
	// so its strip runs on the proxy's own goroutine.
	resp.Body = &trailerFilter{ReadCloser: resp.Body, resp: resp}
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
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		// The body passed PREVIEW_MAX_BODY while being streamed to the app.
		return http.StatusRequestEntityTooLarge, codeBodyTooLarge
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
//
// Before anything is dialled: at most MaxConnsPerPreview requests of the
// preview in flight (useBegin; one more waits up to the dial wait for a slot,
// then is refused 429), and a declared Content-Length within
// PREVIEW_MAX_BODY. A body without one (chunked) is counted as it streams to
// the app — never buffered — and refused 413 when it passes the limit
// (upstreamError). Each admitted request is a preview.request span and a
// muster_preview_requests_total count; neither carries its path or query.
func (g *Gateway) serveAdmitted(w http.ResponseWriter, r *http.Request, id string, exp time.Time) {
	tg := &proxyTarget{id: id}
	sctx, span := telemetry.Start(r.Context(), "preview.request", attribute.String("muster.preview.kind", kindHost),
		attribute.String("muster.preview.auth", authOwner))
	rec := &statusRecorder{ResponseWriter: w}
	left := false
	defer func() {
		if left {
			// The browser gave up while waiting for a slot: never admitted,
			// and nothing was written.
			telemetry.End(span, "client_left")
			return
		}
		st := rec.status
		if tg.upgraded {
			st = http.StatusSwitchingProtocols
		} else if st == 0 {
			st = http.StatusOK // a handler that wrote nothing: net/http answers 200
		}
		g.m.requests.WithLabelValues(kindHost, authOwner, codeClass(st)).Inc()
		span.SetAttributes(attribute.String("muster.preview.status_class", codeClass(st)))
		// The reason is the code the gateway itself rendered (renderCode
		// records it on rec), never read back from the response headers: an
		// app's trailers land there too, and an app must not be able to mark
		// its own request's span as a gateway error.
		reason := ""
		if rec.code != "" {
			reason = refusalLabel(rec.code)
		}
		telemetry.End(span, reason)
	}()
	p, code, left := g.useBegin(r.Context(), id)
	switch {
	case left:
		return
	case code == "":
	case code == codeTooManyConns:
		g.renderCode(rec, http.StatusTooManyRequests, code)
		return
	default:
		g.renderCode(rec, http.StatusNotFound, code)
		return
	}
	defer g.useEnd(p)
	// The response clock starts with the slot taken: a wait for one is not
	// the app's latency (muster_preview_response_seconds).
	tg.start = time.Now()
	ws := isWebSocket(r)
	if !ws && r.ContentLength > g.maxBody {
		g.renderCode(rec, http.StatusRequestEntityTooLarge, codeBodyTooLarge)
		return
	}
	g.mu.Lock()
	t := g.byKey[tunnelKey{p.OwnerID, p.Host}]
	g.mu.Unlock()
	if t == nil {
		g.renderCode(rec, http.StatusServiceUnavailable, codeHostOffline)
		return
	}
	span.SetAttributes(attribute.String("muster.preview.path", string(t.path)))
	ctx, cancel := context.WithCancelCause(sctx)
	defer cancel(nil)
	defer context.AfterFunc(p.ctx, func() { cancel(errPreviewClosed) })()
	defer g.clock.AfterFunc(exp.Sub(g.clock.Now()), func() { cancel(errSessionExpired) })()
	tg.port, tg.t = p.Port, t
	r = r.WithContext(context.WithValue(ctx, targetKey{}, tg))
	if !ws && r.Body != nil && r.Body != http.NoBody {
		// w, not rec: MaxBytesReader tells net/http's own ResponseWriter to
		// close the connection after a body that passed the limit.
		r.Body = http.MaxBytesReader(w, r.Body, g.maxBody)
	}
	switch {
	case ws:
		g.relayWebSocket(rec, r, tg)
	case headerHasToken(r.Header, "Connection", "upgrade"):
		// Only WebSocket upgrades are relayed (and Origin-checked).
		g.renderCode(rec, http.StatusBadRequest, codeBadUpgrade)
	default:
		g.proxy.ServeHTTP(rec, r)
	}
}

// statusRecorder notes the status an admitted request was answered with,
// and the code when the gateway answered it itself.
// Unwrap hands net/http's flusher and hijacker (http.ResponseController,
// coder/websocket) the real ResponseWriter.
type statusRecorder struct {
	http.ResponseWriter
	status int
	code   string // the code of the gateway's own page, when renderCode wrote one
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 && code >= 200 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
