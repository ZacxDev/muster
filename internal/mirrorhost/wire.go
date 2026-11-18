package mirrorhost

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// THE TUNNEL'S BYTES ON THE WIRE: what an open's first batch cost on this
// host's socket (host.watch's mirror.first_batch_wire_bytes), beside its size
// as JSON and, on muster's span, the link's round trip — so a slow open reads
// as latency or as bytes over bandwidth.

// wireCounter counts the bytes one connection wrote to its socket (TLS
// records and all).
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

// tunnelTransport is the ONE transport a Client dials muster with, for every
// connect — not one per connect, which left a refused connect's idle
// connection open in a pool nothing would use again (until its 90 s idle
// timeout). Each connection it dials counts what it writes into the counter
// cur names AT DIAL TIME (the connect's own); http.DefaultTransport's
// settings (proxy, timeouts, TLS) otherwise, which is what the connect used
// before — except that it speaks HTTP/1.1 only, the protocol a WebSocket
// upgrade is made in.
func tunnelTransport(cur *atomic.Pointer[wireCounter]) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if wc := cur.Load(); wc != nil {
			return countingConn{Conn: c, n: wc}, nil
		}
		return c, nil
	}
	return tr
}
