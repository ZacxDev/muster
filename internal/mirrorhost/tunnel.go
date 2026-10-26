package mirrorhost

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// THE TUNNEL'S BYTES ON THE WIRE.
//
// 🔴 COMPRESSED WHEN BOTH ENDS SAY SO, AND ONLY THEN. This host OFFERS
// permessage-deflate (RFC 7692) on its connect; a muster that accepts it
// answers with the extension, and every message from then on is deflated
// both ways. A muster that predates it answers without the extension, and the
// connection is exactly the plain one it always was — that is the
// negotiation, not a guess, so a new host and an old muster (and an old host
// and a new muster, which offers nothing) need nothing else.
// Config.NoCompression stops the offer (-no-tunnel-compression).
//
// WHY: a view's backfill is JSON — up to MaxBackfillEvents events, several
// hundred KiB of repetitive keys, prose, code and test logs — and the open
// waits for its first batch. MEASURED on the tmuxit TestTmuxOpenPerfReport rig
// (a 6 MiB synthetic transcript): the first batch is 96,891 bytes as JSON and
// 7,075 deflated at the tunnel's level (flate.BestSpeed). On a slow link the open's transit is the batch's size over the
// link's bandwidth, so that fraction is the transit saved.
//
// ⚠ The read limit (mirror.MaxFrame) is on the DECOMPRESSED message
// (coder/websocket applies it after the inflater), so a small deflated frame
// cannot inflate past it.

// tunnelCompression is the mode this host offers: context takeover (one
// sliding window per direction per connection — a muster-mirror has ONE
// connection), or none.
func tunnelCompression(off bool) websocket.CompressionMode {
	if off {
		return websocket.CompressionDisabled
	}
	return websocket.CompressionContextTakeover
}

// negotiatedCompression reports whether muster's handshake answer accepted
// permessage-deflate.
func negotiatedCompression(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	for _, v := range resp.Header.Values("Sec-WebSocket-Extensions") {
		if strings.Contains(strings.ToLower(v), "permessage-deflate") {
			return true
		}
	}
	return false
}

// wireCounter counts the bytes one connection wrote to its socket (TLS
// records and all): what a first batch cost on the wire.
type wireCounter struct{ written atomic.Int64 }

type countingConn struct {
	net.Conn
	n *wireCounter
}

func (c countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.n.written.Add(int64(n))
	return n, err
}

// dialClient is an HTTP client for ONE connect whose connection counts what it
// writes into wc — http.DefaultTransport's settings (proxy, timeouts, TLS)
// otherwise, which is what the connect used before.
func dialClient(wc *wireCounter) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return countingConn{Conn: c, n: wc}, nil
	}
	return &http.Client{Transport: tr}
}
