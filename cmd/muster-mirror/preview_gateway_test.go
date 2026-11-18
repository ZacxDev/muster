package main

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

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

// TestPreviewsListWindowsWithMusterDown is -preview through run with muster
// refusing every connect: the tunnel dial names this host, and the host lists
// the backend's windows itself (previews' one window source) and the gateway
// gets them — muster's state has no part in it.
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
	if h := <-gw.hosts; h != "laptop" {
		t.Fatalf("the tunnel dial named host %q", h)
	}
	if w := <-got; w != "@3,@5" {
		t.Fatalf("windows %q; want @3,@5 from the backend", w)
	}
}
