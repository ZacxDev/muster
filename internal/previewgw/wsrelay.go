package previewgw

// wsrelay.go — a preview's WebSocket (a dev server's HMR, an app's own),
// relayed MESSAGE BY MESSAGE: the gateway terminates the browser's leg and
// opens its own to the app (coder/websocket on both), rather than copying
// raw bytes after the 101. So it can ping the browser leg every
// StreamKeepalive, which keeps the edge from cutting a quiet socket at its
// ~125 s idle limit, without touching the app's framing. Each message is
// streamed from one leg's reader into the other's writer, so a large message
// is never held whole. Each leg answers its own peer's pings.
//
// The upstream leg is dialled first, so an app that refuses the upgrade has
// its own response (status, body) passed to the browser; the browser's leg is
// accepted only once the app has, with the subprotocol the app chose.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// maxRefusalBody bounds the app's refusal body passed to the browser (the
// WebSocket client reads at most this much of it anyway).
const maxRefusalBody = 1024

// wsCloseWait bounds the closing handshake with each leg.
const wsCloseWait = 5 * time.Second

// relayWebSocket relays r (an admitted, Origin-checked upgrade) to tg's app.
// r's context ends when the preview closes or the owner cookie expires; the
// browser's leg is then closed with 1008 and the code as its reason.
func (g *Gateway) relayWebSocket(w http.ResponseWriter, r *http.Request, tg *proxyTarget) {
	ctx := r.Context()
	hdr := http.Header{}
	for _, k := range []string{"Cookie", "Authorization", "Origin", "User-Agent", "Accept-Language"} {
		if vs := r.Header.Values(k); len(vs) > 0 {
			hdr[k] = append([]string(nil), vs...)
		}
	}
	g.upstreamHeaders(hdr, tg, true)
	var subs []string
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				subs = append(subs, s)
			}
		}
	}
	dctx, dcancel := context.WithTimeout(ctx, UpstreamHeaderTimeout)
	up, resp, err := websocket.Dial(dctx, "ws://"+tg.poolAddr()+r.URL.RequestURI(), &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: tg.t.rt},
		HTTPHeader:      hdr,
		Host:            tg.upstreamHost(),
		Subprotocols:    subs,
		CompressionMode: websocket.CompressionDisabled,
	})
	dcancel()
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode >= 400 {
			// The app answered and refused: its response, not the gateway's.
			g.passRefusal(w, resp, tg)
			return
		}
		st, code := upstreamError(ctx, err)
		g.renderCode(w, st, code)
		return
	}
	defer up.CloseNow()
	up.SetReadLimit(-1)
	var accSubs []string
	if s := up.Subprotocol(); s != "" {
		accSubs = []string{s}
	}
	// The Origin was checked against this preview's own already
	// (crossSiteRefusal), more strictly than Accept's same-host rule.
	br, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       accSubs,
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		up.Close(websocket.StatusGoingAway, "")
		return
	}
	defer br.CloseNow()
	br.SetReadLimit(-1)

	// The pumps run on their own context: cancelling the one a coder/websocket
	// read runs under drops its connection without a close frame, and the
	// browser is owed one.
	pctx, pcancel := context.WithCancel(context.Background())
	defer pcancel()
	fromBrowser := make(chan error, 1)
	fromApp := make(chan error, 1)
	go func() { fromBrowser <- pump(pctx, br, up) }()
	go func() { fromApp <- pump(pctx, up, br) }()
	go keepBrowserAlive(pctx, br, g.keepalive)

	select {
	case err := <-fromBrowser:
		closeLike(up, err)
		br.CloseNow()
	case err := <-fromApp:
		closeLike(br, err)
		up.CloseNow()
	case <-ctx.Done():
		_, code := upstreamError(ctx, context.Cause(ctx))
		br.Close(websocket.StatusPolicyViolation, code)
		up.Close(websocket.StatusGoingAway, "")
	}
}

// pump streams every message from src to dst until either fails.
func pump(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		typ, rd, err := src.Reader(ctx)
		if err != nil {
			return err
		}
		wr, err := dst.Writer(ctx, typ)
		if err != nil {
			return err
		}
		if _, err := io.Copy(wr, rd); err != nil {
			return err
		}
		if err := wr.Close(); err != nil {
			return err
		}
	}
}

// closeLike closes c the way the other leg ended: with its close status and
// reason when it sent one that may be sent on, else going-away.
func closeLike(c *websocket.Conn, err error) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.StatusNoStatusRcvd, websocket.StatusAbnormalClosure, websocket.StatusTLSHandshake:
		default:
			closeWithin(c, ce.Code, ce.Reason)
			return
		}
	}
	closeWithin(c, websocket.StatusGoingAway, "")
}

func closeWithin(c *websocket.Conn, code websocket.StatusCode, reason string) {
	done := make(chan struct{})
	go func() { _ = c.Close(code, reason); close(done) }()
	select {
	case <-done:
	case <-time.After(wsCloseWait):
		c.CloseNow()
	}
}

// minPingWait is the least time a browser has to answer a ping, whatever the
// keepalive interval (tests run it at milliseconds).
const minPingWait = 2 * time.Second

// keepBrowserAlive pings the browser's leg every d until ctx ends. 🔴 A PING
// THAT FAILS OR GOES UNANSWERED (within d, at least minPingWait) CLOSES THE
// BROWSER'S LEG: a browser that vanished without a close (a sleeping laptop,
// a dropped network) would otherwise hold the relay — and the app's leg, a
// host slot, and the preview's in-flight use — open with nothing reading
// it. Closing it ends the browser's pump, and the relay closes the app's leg.
func keepBrowserAlive(ctx context.Context, c *websocket.Conn, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, max(d, minPingWait))
			err := c.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				c.CloseNow()
				return
			}
		}
	}
}

// passRefusal writes the app's refusal of an upgrade to the browser: its
// status, Content-Type and (bounded) body, under the proxy's response rules.
func (g *Gateway) passRefusal(w http.ResponseWriter, resp *http.Response, tg *proxyTarget) {
	h := http.Header{}
	for _, k := range []string{"Content-Type", "Set-Cookie", "Location", "Cache-Control"} {
		if vs := resp.Header.Values(k); len(vs) > 0 {
			h[k] = vs
		}
	}
	g.previewResponseHeaders(h, tg)
	for k, vs := range h {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		_, _ = io.Copy(w, io.LimitReader(resp.Body, maxRefusalBody))
		resp.Body.Close()
	}
}
