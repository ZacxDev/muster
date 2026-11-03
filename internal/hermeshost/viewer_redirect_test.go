package hermeshost

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

// TestTheViewerRefusesARedirect: the viewer's ticket rides the
// Sec-WebSocket-Protocol header, which net/http copies to ANY redirect
// Location. A gateway API that answers the WebSocket with a redirect gets a
// refused Attach naming the Location's host, and the Location is never asked
// — so it never sees the ticket.
//
// It does not name internal/noredirect: it pins the behaviour, so it runs
// unchanged against a tree without the fix.
func TestTheViewerRefusesARedirect(t *testing.T) {
	dir := shortHome(t)
	g := hermestest.NewGateway(t, dir)
	var mu sync.Mutex
	hits := 0
	var protos []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		protos = append(protos, r.Header.Get("Sec-WebSocket-Protocol"))
		mu.Unlock()
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(target.Close)
	asked := 0
	from := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(from.Close)
	g.Set("api_origin", from.URL)

	_, _, err := NewHome(dir).Attach(gctx(t), viewSession)
	mu.Lock()
	defer mu.Unlock()
	if asked == 0 {
		t.Fatalf("the redirecting API was never dialled (%v); the test proves nothing", err)
	}
	if hits != 0 || len(protos) != 0 {
		t.Fatalf("the redirect target got %d request(s), subprotocols %q — the ticket followed the redirect", hits, protos)
	}
	u, _ := url.Parse(target.URL)
	if !errors.Is(err, ErrGatewayProtocol) || !strings.Contains(err.Error(), u.Host) {
		t.Fatalf("err = %v, want ErrGatewayProtocol naming %s", err, u.Host)
	}
}
