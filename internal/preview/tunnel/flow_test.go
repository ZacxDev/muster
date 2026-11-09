package tunnel

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// TestFlowControlInvariantIsPinned pins the window and cap numbers as
// LITERALS, the relationship between them, and that the configurations Serve
// and NewClient actually build carry them. Moving any one constant without the
// others reddens here; so does a builder that stops using a constant.
func TestFlowControlInvariantIsPinned(t *testing.T) {
	if wire.MaxConns != 64 {
		t.Errorf("wire.MaxConns = %d, want 64 (phase 1's cap)", wire.MaxConns)
	}
	if StreamWindow != 4<<20 {
		t.Errorf("StreamWindow = %d, want 4 MiB", StreamWindow)
	}
	if ConnWindow != 260<<20 {
		t.Errorf("ConnWindow = %d, want 260 MiB = (64+1) × 4 MiB", ConnWindow)
	}
	if UploadStreamWindow != 1<<20 {
		t.Errorf("UploadStreamWindow = %d, want 1 MiB", UploadStreamWindow)
	}
	if UploadConnWindow != 65<<20 {
		t.Errorf("UploadConnWindow = %d, want 65 MiB = (64+1) × 1 MiB", UploadConnWindow)
	}
	// The invariant itself: no set of MaxConns paused streams can hold the
	// whole connection window, in either direction.
	if !(ConnWindow > wire.MaxConns*StreamWindow) {
		t.Errorf("ConnWindow %d <= MaxConns × StreamWindow %d", ConnWindow, wire.MaxConns*StreamWindow)
	}
	if !(UploadConnWindow > wire.MaxConns*UploadStreamWindow) {
		t.Errorf("UploadConnWindow %d <= MaxConns × UploadStreamWindow %d", UploadConnWindow, wire.MaxConns*UploadStreamWindow)
	}

	c := ClientOptions{}.http2Config()
	if c.MaxReceiveBufferPerStream != 4<<20 || c.MaxReceiveBufferPerConnection != 260<<20 {
		t.Errorf("client config = stream %d conn %d, want 4 MiB / 260 MiB", c.MaxReceiveBufferPerStream, c.MaxReceiveBufferPerConnection)
	}
	s, err := HostOptions{}.server()
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxConcurrentStreams != 64 || s.MaxUploadBufferPerStream != 1<<20 || s.MaxUploadBufferPerConnection != 65<<20 {
		t.Errorf("host server = streams %d upload stream %d conn %d, want 64 / 1 MiB / 65 MiB",
			s.MaxConcurrentStreams, s.MaxUploadBufferPerStream, s.MaxUploadBufferPerConnection)
	}
	for _, n := range []int{1, 10, 64} {
		s, err := HostOptions{MaxConns: n}.server()
		if err != nil || s.MaxConcurrentStreams != uint32(n) {
			t.Errorf("MaxConns %d: streams %v, err %v", n, s, err)
		}
	}
	for _, n := range []int{-1, 65, 256} {
		if _, err := (HostOptions{MaxConns: n}).server(); err == nil {
			t.Errorf("MaxConns %d accepted; the window is sized for at most %d", n, wire.MaxConns)
		}
	}
}

const (
	assets      = 62
	pausedSize  = 64 << 20
	pausedFirst = 1 << 20
)

type stallResult struct {
	assetsOK int
	echoOK   bool
	echoN    int
	echoErr  error
}

func (r stallResult) String() string {
	return fmt.Sprintf("assets %d/%d, echo ok=%v (%d round trips, err=%v)", r.assetsOK, assets, r.echoOK, r.echoN, r.echoErr)
}

// pausedReaders opens paused downloads, lets them fill their windows, then
// runs `assets` asset GETs (64–512 KiB each) and an echo stream concurrently,
// all of which must finish within budget while the readers stay paused.
func pausedReaders(t *testing.T, client ClientOptions, paused int) stallResult {
	t.Helper()
	blob := startBlobServer(t)
	echo := startTCP(t, echoUpstream)
	p := newPair(t, pairOpts{client: client, allow: []int{blob, echo}})
	for i := range paused {
		startPaused(t, p, blob, pausedSize, uint64(50+i), pausedFirst)
	}
	time.Sleep(500 * time.Millisecond) // let the paused streams fill their windows
	deadline := time.Now().Add(5 * time.Second)

	var ok atomic.Int64
	var wg sync.WaitGroup
	hc := p.httpClient()
	for i := range assets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			sz := int64(64<<10) * int64(1+i%8)
			seed := uint64(100 + i)
			req, _ := newGet(ctx, fmt.Sprintf("http://127.0.0.1:%d/blob?n=%d&seed=%d&delay=100ms", blob, sz, seed))
			resp, err := hc.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			h := sha256.New()
			n, err := io.Copy(h, resp.Body)
			want := blobSHA(seed, sz)
			if err == nil && n == sz && string(h.Sum(nil)) == string(want[:]) {
				ok.Add(1)
			}
		}()
	}
	var r stallResult
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.echoN, r.echoErr = echoRoundTrips(p, echo, 20, deadline)
		r.echoOK = r.echoErr == nil && r.echoN == 20
	}()
	wg.Wait()
	r.assetsOK = int(ok.Load())
	return r
}

// TestPausedReadersCannotStallTheTunnel is the pinned invariant's behaviour:
// with the real windows, three paused readers leave every asset and the echo
// working. Its negative control is the spike's measured bad configuration
// (8 MiB connection / 4 MiB stream window), where three paused readers took
// the whole connection window and stalled every other stream; its positive
// control is the same bad configuration with ONE paused reader, which must
// stay green, so the red is the paused readers' doing and not the harness's.
func TestPausedReadersCannotStallTheTunnel(t *testing.T) {
	bad := ClientOptions{connWindow: 8 << 20, streamWindow: 4 << 20}
	t.Run("real windows, 3 paused", func(t *testing.T) {
		r := pausedReaders(t, ClientOptions{}, 3)
		t.Logf("RESULT %v", r)
		if r.assetsOK != assets || !r.echoOK {
			t.Fatalf("paused readers stalled the tunnel: %v", r)
		}
	})
	t.Run("negative control: 8 MiB connection window, 3 paused", func(t *testing.T) {
		r := pausedReaders(t, bad, 3)
		t.Logf("RESULT %v", r)
		if r.assetsOK == assets && r.echoOK {
			t.Fatalf("negative control stayed GREEN: the harness cannot see a stalled connection window: %v", r)
		}
		if r.assetsOK != 0 {
			t.Errorf("expected the spike's 0/%d, got %v", assets, r)
		}
	})
	t.Run("positive control: 8 MiB connection window, 1 paused", func(t *testing.T) {
		r := pausedReaders(t, bad, 1)
		t.Logf("RESULT %v", r)
		if r.assetsOK != assets || !r.echoOK {
			t.Fatalf("one paused reader stalled an 8 MiB window; the control is broken: %v", r)
		}
	})
}

// TestTheWorstCaseCapCannotStallTheTunnel is the invariant at its bound: the
// host's whole cap minus one stream is paused, and the last stream must still
// carry assets and an echo. The windows are scaled down (256 KiB per stream,
// the same (cap+1) × stream formula) to keep the test's memory small; the cap
// is the real one. The negative control sizes the connection window at
// cap/2 × stream, which the paused streams fill. (Not (cap-1) × stream: a
// paused stream measured holding ~245 KiB of its 256 KiB, because x/net
// batches small window updates, so 63 of them left ~700 KiB of a (cap-1)
// window free and the control stayed green.)
func TestTheWorstCaseCapCannotStallTheTunnel(t *testing.T) {
	const sw = 256 << 10
	run := func(t *testing.T, connWindow int) (int, bool) {
		blob := startBlobServer(t)
		echo := startTCP(t, echoUpstream)
		p := newPair(t, pairOpts{client: ClientOptions{connWindow: connWindow, streamWindow: sw}, allow: []int{blob, echo}})
		paused := wire.MaxConns - 1
		for i := range paused {
			if _, err := tryPaused(t, p, blob, 4<<20, uint64(1000+i), 64<<10, 2*time.Second); err != nil {
				// The window filled before every paused stream got its
				// first bytes: that is a stall too.
				t.Logf("RESULT paused stream %d/%d stalled in setup: %v", i+1, paused, err)
				return 0, false
			}
		}
		time.Sleep(500 * time.Millisecond)
		deadline := time.Now().Add(4 * time.Second)
		hc := p.httpClient()
		ok := 0
		for i := range 4 { // the one free stream, used in turn
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			sz, seed := int64(512<<10), uint64(2000+i)
			req, _ := newGet(ctx, fmt.Sprintf("http://127.0.0.1:%d/blob?n=%d&seed=%d", blob, sz, seed))
			if resp, err := hc.Do(req); err == nil {
				h := sha256.New()
				n, err := io.Copy(h, resp.Body)
				resp.Body.Close()
				want := blobSHA(seed, sz)
				if err == nil && n == sz && string(h.Sum(nil)) == string(want[:]) {
					ok++
				}
			}
			cancel()
		}
		n, err := echoRoundTrips(p, echo, 10, deadline)
		t.Logf("RESULT paused=%d connWindow=%d KiB: assets %d/4, echo %d/10 (%v)", paused, connWindow>>10, ok, n, err)
		return ok, err == nil && n == 10
	}
	t.Run("(cap+1) × stream", func(t *testing.T) {
		if ok, echo := run(t, (wire.MaxConns+1)*sw); ok != 4 || !echo {
			t.Fatalf("the worst case stalled: assets %d/4 echo %v", ok, echo)
		}
	})
	t.Run("negative control: cap/2 × stream", func(t *testing.T) {
		if ok, echo := run(t, wire.MaxConns/2*sw); ok == 4 && echo {
			t.Fatal("negative control stayed GREEN: paused streams filling the window did not stall the last stream")
		}
	})
}

// TestStalledUploadsCannotStallTheTunnel is the same invariant in the other
// direction: upstreams that never read their request body hold the HOST's
// upload windows, and an echo stream must still get its bytes to the host.
// The negative control is x/net's server default (1 MiB connection, 1 MiB
// stream), which one stalled upload fills.
func TestStalledUploadsCannotStallTheTunnel(t *testing.T) {
	run := func(t *testing.T, host HostOptions, stalled int) (int, error) {
		sink := startTCP(t, func(c net.Conn) { time.Sleep(time.Minute) }) // accepts, never reads
		echo := startTCP(t, echoUpstream)
		p := newPair(t, pairOpts{host: host, allow: []int{sink, echo}})
		for range stalled {
			c, err := p.client.Dial(context.Background(), sink)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			go func() { _, _ = io.Copy(c, io.LimitReader(zeroReader{}, 64<<20)) }()
		}
		time.Sleep(time.Second) // let the uploads fill the sink's socket buffers and the windows
		n, err := echoRoundTrips(p, echo, 20, time.Now().Add(4*time.Second))
		t.Logf("RESULT stalled=%d upload windows stream=%d conn=%d: echo %d/20 (%v)", stalled, host.uploadStreamWindow, host.uploadConnWindow, n, err)
		return n, err
	}
	t.Run("real upload windows, 3 stalled", func(t *testing.T) {
		if n, err := run(t, HostOptions{}, 3); n != 20 || err != nil {
			t.Fatalf("stalled uploads stalled the tunnel: echo %d/20 %v", n, err)
		}
	})
	t.Run("negative control: x/net server defaults, 1 stalled", func(t *testing.T) {
		if n, err := run(t, HostOptions{uploadConnWindow: 1 << 20, uploadStreamWindow: 1 << 20}, 1); n == 20 && err == nil {
			t.Fatal("negative control stayed GREEN: one stalled upload did not stall a 1 MiB connection window")
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// TestALargeDownloadToAPausedReaderIsByteExactAndBounded: 50 MiB to a reader
// that pauses for 2 s after the first MiB arrives whole, and while it is
// paused the process's live heap grows by no more than two stream windows
// (the host holds no unbounded read-ahead; the gateway holds one window).
func TestALargeDownloadToAPausedReaderIsByteExactAndBounded(t *testing.T) {
	blob := startBlobServer(t)
	p := newPair(t, pairOpts{allow: []int{blob}})
	base := liveHeap()
	d := startPaused(t, p, blob, 50<<20, 7, 1<<20)
	var peak int64
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); {
		time.Sleep(250 * time.Millisecond)
		peak = max(peak, liveHeap()-base)
	}
	ok, n, err := d.resume()
	t.Logf("RESULT bytes=%d exact=%v peak heap growth while paused=%.2f MiB (bound %d MiB)", n, ok, float64(peak)/(1<<20), 2*StreamWindow>>20)
	if !ok || err != nil {
		t.Fatalf("not byte-exact: %d of %d bytes, err %v", n, 50<<20, err)
	}
	if peak > 2*StreamWindow {
		t.Fatalf("heap grew %.1f MiB while the reader was paused, over 2 stream windows", float64(peak)/(1<<20))
	}
}

func liveHeap() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}
