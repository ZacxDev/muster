package previewtunnel

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// EdgeSim is a LOCAL stand-in for the edge (Cloudflare): a TCP relay that
// closes a connection pair after Idle with no bytes in either direction.
// It does not buffer. It models ONLY the documented ≈100 s idle timeout; it
// is not evidence about what the real edge does (TLS, HTTP/2 to origin,
// buffering, whether WS ping frames count as activity).
type EdgeSim struct {
	Target string
	Idle   time.Duration
	Kills  atomic.Int64
	ln     net.Listener
	wg     sync.WaitGroup
}

func (e *EdgeSim) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	e.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go e.relay(c)
		}
	}()
	return ln.Addr().String(), nil
}

func (e *EdgeSim) Close() { e.ln.Close() }

func (e *EdgeSim) relay(a net.Conn) {
	b, err := net.Dial("tcp", e.Target)
	if err != nil {
		a.Close()
		return
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done); a.Close(); b.Close() }) }
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				last.Store(time.Now().UnixNano())
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				if err == io.EOF {
					if tc, ok := dst.(*net.TCPConn); ok {
						_ = tc.CloseWrite()
						return
					}
				}
				break
			}
		}
		stop()
	}
	go cp(b, a)
	go cp(a, b)
	if e.Idle > 0 {
		t := time.NewTicker(e.Idle / 20)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, last.Load())) > e.Idle {
					e.Kills.Add(1)
					stop()
					return
				}
			}
		}
	}
	<-done
}
