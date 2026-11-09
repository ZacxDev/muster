package main

import (
	"bufio"
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/preview/tunnel"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

// previewGateway is a fake preview gateway on the tunnel client: it accepts
// the host's tunnel, opens the offer stream, and delivers each decoded line
// and the host name the dial carried.
type previewGateway struct {
	url    string
	offers chan wire.Offer
	hosts  chan string
}

func newPreviewGateway(t *testing.T) *previewGateway {
	t.Helper()
	g := &previewGateway{offers: make(chan wire.Offer, 64), hosts: make(chan string, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.HostPath {
			http.NotFound(w, r)
			return
		}
		g.hosts <- r.Header.Get(wire.HostNameHeader)
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c, err := tunnel.NewClient(ctx, ws, tunnel.ClientOptions{})
		if err != nil {
			return
		}
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, wire.OfferPath, nil)
			req.Header.Set(wire.GatewayBootHeader, "boot-1")
			resp, err := c.RoundTrip(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			br := bufio.NewReader(resp.Body)
			for {
				line, err := br.ReadBytes('\n')
				if len(line) > 0 {
					if m, derr := wire.NewDecoder(bytes.NewReader(line)).Next(); derr == nil {
						g.offers <- m
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(func() { cancel(); srv.Close() })
	g.url = "ws" + strings.TrimPrefix(srv.URL, "http") + wire.HostPath
	return g
}

// windows waits for a windows line and returns its ids, comma-joined.
func (g *previewGateway) windows(t *testing.T, within time.Duration) string {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case m := <-g.offers:
			if m.Type == wire.TypeWindows {
				return strings.Join(m.Windows, ",")
			}
		case <-deadline:
			t.Fatalf("the gateway got no window listing within %v", within)
		}
	}
}

// TestTheRefreshWindowListingReachesTheGateway: a listing the mirror's
// refresh publishes (previewWindows, mirrorhost.Config.OnWindows) reaches the
// gateway's offer stream once previews are up — on a backend that cannot list
// windows itself, so the refresh is the only source.
func TestTheRefreshWindowListingReachesTheGateway(t *testing.T) {
	gw := newPreviewGateway(t)
	file, token := previewFiles(t, `{"ports": ["5173"]}`)
	sink := &previewWindows{}
	ctx, cancel := context.WithCancel(context.Background())
	var lg lockedBuffer
	done := startPreview(ctx, previewFlags{On: true, URLs: gw.url, TokenFile: token, File: file}, true,
		func() map[int]string { return nil }, "laptop", listBackend{}, sink, log.New(&lg, "", 0))
	t.Cleanup(func() { cancel(); previewSettle(done, 5*time.Second) })
	if h := <-gw.hosts; h != "laptop" {
		t.Fatalf("the tunnel dial named host %q", h)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		sink.publish([]mirrorhost.WindowInfo{{Session: "work", ID: "@3", PanePID: 100}})
		select {
		case m := <-gw.offers:
			if m.Type == wire.TypeWindows {
				if got := strings.Join(m.Windows, ","); got != "@3" {
					t.Fatalf("windows %q", got)
				}
				return
			}
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refresh's listing never reached the gateway; log:\n%s", lg.String())
		}
	}
}

// TestPreviewsListWindowsWithMusterDown is -preview through run with muster
// refusing every connect, so the mirror's refresh never runs: the host reads
// the windows itself and the gateway gets them.
func TestPreviewsListWindowsWithMusterDown(t *testing.T) {
	gw := newPreviewGateway(t)
	file, token := previewFiles(t, `{"ports": ["5173"]}`)
	prev := newTmuxBackend
	t.Cleanup(func() { newTmuxBackend = prev })
	got := make(chan string, 1)
	rc, errs := runUntil(t, map[string]string{envPreview: "1", envPreviewURLs: gw.url, envPreviewTokenFile: token, envPreviewFile: file},
		func() bool {
			select {
			case w := <-gw.offers:
				if w.Type == wire.TypeWindows {
					got <- strings.Join(w.Windows, ",")
					return true
				}
			default:
			}
			return false
		}, windowsBackend{})
	if rc != 0 {
		t.Fatalf("rc %d, log:\n%s", rc, errs)
	}
	if w := <-got; w != "@3,@5" {
		t.Fatalf("windows %q; want @3,@5 from the backend", w)
	}
}
