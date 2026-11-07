package push

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/safedial"
)

// Web Push under MUSTER_DIAL_DENY_PRIVATE (WebpushPusher.WithDialGuard): a
// push host that resolves to a non-public address is refused at connect time,
// and the push service never sees a connection.

// TestAGuardedPushSendRefusesAHostThatResolvesPrivate: the endpoint names
// example.com (httptest's certificate covers it); the guard's resolver answers
// with the test server's loopback address.
//
//   - strict guard: refused with safedial's error, the server is not reached;
//   - CONTROL, a guard admitting loopback over the same resolver and the same
//     production client: the send is DELIVERED, so the refusal above is the
//     address rule and not a transport that cannot connect;
//   - CONTROL, no guard (the default): the production client reaches the
//     loopback server too.
func TestAGuardedPushSendRefusesAHostThatResolvesPrivate(t *testing.T) {
	var reached atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()
	_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
	endpoint := "https://example.com:" + port + "/push/abc"
	toServer := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	// guarded is the production pusher with g, trusting httptest's certificate.
	guarded := func(g *safedial.Guard) *WebpushPusher {
		p := NewWebpushPusher(realVAPID(t, "mailto:ops@example.com")).WithDialGuard(g)
		tr, ok := p.client.(*http.Client).Transport.(*http.Transport)
		if !ok {
			t.Fatalf("WithDialGuard left the client's transport a %T, not the guard's *http.Transport", p.client.(*http.Client).Transport)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
		return p
	}

	res := guarded(&safedial.Guard{Resolve: toServer}).Send(context.Background(), realSubscription(t, endpoint), []byte(`{}`))
	if !errors.Is(res.Err, safedial.ErrForbiddenAddress) || res.StatusCode != 0 {
		t.Fatalf("strict guard: send = %+v; want safedial.ErrForbiddenAddress and no status", res)
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("strict guard: the push server received %d request(s)", n)
	}

	res = guarded(&safedial.Guard{AllowLoopback: true, Resolve: toServer}).Send(context.Background(), realSubscription(t, endpoint), []byte(`{}`))
	if res.Err != nil || res.StatusCode != http.StatusCreated || reached.Load() != 1 {
		t.Fatalf("CONTROL (guard admitting loopback): send = %+v, server reached %d time(s); want 201, once", res, reached.Load())
	}

	off := overTLS(t, NewWebpushPusher(realVAPID(t, "mailto:ops@example.com")).WithDialGuard(nil), ts)
	if res := off.Send(context.Background(), realSubscription(t, ts.URL+"/push/abc"), []byte(`{}`)); res.Err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("CONTROL (no guard, the default): send to a loopback push host = %+v; want 201", res)
	}
}
