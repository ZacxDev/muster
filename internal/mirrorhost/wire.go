package mirrorhost

import (
	"context"
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
