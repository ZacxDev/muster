package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/clientaddr"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE PUBLIC PAGES LISTENER'S HARDENING (pages_public.go, pages.go): what
// reaches muster through it, how long a slow client can hold it, the miss
// budget, and that no log line carries a link's token. No database: the
// share-link store is a counting fake. Every address, header value and token
// is invented.

// countingLinkStore is a pages.Store whose LinkByToken finds nothing and counts
// its calls; any other method is not expected here (the embedded nil Store
// panics).
type countingLinkStore struct {
	pages.Store
	lookups atomic.Int64
}

func (c *countingLinkStore) LinkByToken(context.Context, string) (pages.Link, error) {
	c.lookups.Add(1)
	return pages.Link{}, pages.ErrLinkNotFound
}

// linkServer is a Server with pages on over a counting store, the given
// client-address policy, and its log in buf.
func linkServer(t *testing.T, policy *clientaddr.Policy, buf *bytes.Buffer) (*Server, *countingLinkStore) {
	t.Helper()
	store := &countingLinkStore{}
	s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "public-pages-operator-pw"}, log.New(buf, "", 0))
	s.UseExtensions(Extensions{Pages: store, ClientAddr: policy})
	return s, store
}

// TestThePublicPagesListenerPassesOnlyItsHeaders: inside, the request carries
// exactly the allowlisted headers it was sent; a cookie, a bearer, an
// act-for, any X-Muster-*, any X-Openclaw-* and every forwarding header are
// gone. The configured client-address header is the one addition.
func TestThePublicPagesListenerPassesOnlyItsHeaders(t *testing.T) {
	sent := map[string]string{
		// allowlisted
		"Accept": "text/html", "Accept-Encoding": "gzip", "Accept-Language": "en", "Content-Type": "application/json",
		"If-None-Match": `"w0rd"`, "If-Modified-Since": "Mon, 02 Jan 2006 15:04:05 GMT", "Range": "bytes=0-1",
		"Origin": "null", "Referer": "https://elsewhere.example/", "User-Agent": "fixture/1",
		"Sec-Fetch-Dest": "iframe", "Sec-Fetch-Site": "same-origin",
		// not
		"Cookie": "muster_session=w0rd", "Authorization": "Bearer mst_w0rd", "Proxy-Authorization": "Basic w0rd",
		"Muster-Act-For": "1", "X-Muster-Token": "w0rd", "X-Muster-Step-Up": "w0rd", "X-Api-Key": "w0rd",
		"X-Openclaw-Session-Key": "w0rd", "X-Openclaw-Model": "w0rd", "X-Forwarded-For": "198.51.100.7",
		"X-Real-Ip": "198.51.100.7", "Cf-Connecting-Ip": "198.51.100.7", "X-Forwarded-Proto": "https",
		"Hx-Request": "true", "X-Test-User": "1",
	}
	inside := func(clientIPHeader string) []string {
		var got []string
		h := PagesPublicSurface(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k := range r.Header {
				got = append(got, k)
			}
		}), clientIPHeader)
		r := httptest.NewRequest(http.MethodPost, "/s/w0rd/submit", strings.NewReader(`{}`))
		for k, v := range sent {
			r.Header.Set(k, v)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		sort.Strings(got)
		return got
	}
	want := "Accept Accept-Encoding Accept-Language Content-Type If-Modified-Since If-None-Match Origin Range Referer Sec-Fetch-Dest Sec-Fetch-Site User-Agent"
	if got := strings.Join(inside(""), " "); got != want {
		t.Errorf("inside, with no client-address header:\n  got  %s\n  want %s", got, want)
	}
	if got := strings.Join(inside(clientaddr.HeaderXForwardedFor), " "); !strings.Contains(" "+got+" ", " X-Forwarded-For ") ||
		strings.Contains(got, "X-Real-Ip") || strings.Contains(got, "Cookie") {
		t.Errorf("inside, with %s configured: %s", clientaddr.HeaderXForwardedFor, got)
	}
}

// TestThePublicPagesListenerKeepsTheClientAddressRight: behind a trusted
// proxy, the configured header still names each viewer (two viewers have two
// miss budgets); from an untrusted peer it is ignored (a viewer naming a new
// address each time still spends ONE budget).
func TestThePublicPagesListenerKeepsTheClientAddressRight(t *testing.T) {
	defer func(m int) { pageMissLimit = m }(pageMissLimit)
	pageMissLimit = 1
	policy, err := clientaddr.New(clientaddr.Config{TrustedProxies: "192.0.2.0/24", Header: clientaddr.HeaderXForwardedFor})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	s, _ := linkServer(t, policy, &buf)
	public := PagesPublicSurface(s.Handler(), policy.Header())
	get := func(peer, xff string) int {
		r := httptest.NewRequest(http.MethodGet, "/s/"+strings.Repeat("Q", 43), nil)
		r.RemoteAddr = peer + ":40000"
		r.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		public.ServeHTTP(w, r)
		return w.Code
	}
	// Through the proxy: viewer A spends its budget; viewer B has its own.
	if a1, a2, b1 := get("192.0.2.10", "203.0.113.1"), get("192.0.2.10", "203.0.113.1"), get("192.0.2.10", "203.0.113.2"); a1 != http.StatusNotFound ||
		a2 != http.StatusTooManyRequests || b1 != http.StatusNotFound {
		t.Errorf("behind the trusted proxy: A %d then %d, B %d; want 404, 429, 404 (one budget per viewer)", a1, a2, b1)
	}
	// Direct and untrusted: the header is not believed.
	if c1, c2 := get("198.51.100.20", "203.0.113.3"), get("198.51.100.20", "203.0.113.4"); c1 != http.StatusNotFound || c2 != http.StatusTooManyRequests {
		t.Errorf("an untrusted peer naming two addresses: %d then %d; want 404, 429 (its own one budget)", c1, c2)
	}
}

// TestTheMissBudgetIsCheckedBeforeTheLookup: on the public pages listener,
// once an address has spent its miss budget its requests are 429 and the
// store is not asked.
func TestTheMissBudgetIsCheckedBeforeTheLookup(t *testing.T) {
	defer func(m int) { pageMissLimit = m }(pageMissLimit)
	pageMissLimit = 2
	var buf bytes.Buffer
	s, store := linkServer(t, nil, &buf)
	h := PagesPublicSurface(s.Handler(), "")
	codes := []int{}
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodGet, "/s/"+strings.Repeat("Q", 43), nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		codes = append(codes, w.Code)
	}
	if n := store.lookups.Load(); n != 2 {
		t.Errorf("the store was asked %d time(s) for 5 requests with a budget of 2; want 2 (codes %v)", n, codes)
	}
	if codes[1] != http.StatusNotFound || codes[2] != http.StatusTooManyRequests || codes[4] != http.StatusTooManyRequests {
		t.Errorf("codes %v; want 404 404 429 429 429", codes)
	}
	// The control, and the private listener's own contract: there the lookup
	// still comes first (a throttled address opens a live link,
	// TestAThrottledAddressStillOpensALiveLink), so every request is a lookup.
	s2, store2 := linkServer(t, nil, &buf)
	for i := 0; i < 5; i++ {
		s2.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/s/"+strings.Repeat("Q", 43), nil))
	}
	if n := store2.lookups.Load(); n != 5 {
		t.Errorf("on the private listener the store was asked %d time(s) for 5 requests; want 5", n)
	}
}

// TestThePublicPagesServerCutsOffASlowBody: a request whose body stops
// arriving, on a route that sets no deadline of its own, is cut off at the
// server's read timeout instead of holding the connection.
func TestThePublicPagesServerCutsOffASlowBody(t *testing.T) {
	defer func(d time.Duration) { pagesPublicReadTimeout = d }(pagesPublicReadTimeout)
	pagesPublicReadTimeout = 300 * time.Millisecond
	readErr := make(chan error, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
	})
	srv := NewPagesPublicServer("127.0.0.1:0", inner, "", 5*time.Second)
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	_, _ = io.WriteString(c, "POST /s/w0rd/submit HTTP/1.1\r\nHost: pages.example.test\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"comp")
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("the stalled body read to the end")
		}
		if took := time.Since(start); took < 250*time.Millisecond {
			t.Errorf("cut off after %v, before the read timeout: refused for some other reason (%v)", took, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body was still being read after 5s; the read timeout is 300ms")
	}
	if srv.WriteTimeout < pageUploadReadTimeout+30*time.Second || srv.IdleTimeout <= 0 || srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("timeouts: write %v (want ≥ the upload's %v + 30s), idle %v, header %v", srv.WriteTimeout, pageUploadReadTimeout, srv.IdleTimeout, srv.ReadHeaderTimeout)
	}
}

// TestNoCookieLeavesThePublicListenerByAnyPath: a Set-Cookie the handler adds
// is gone whether the header goes out by WriteHeader, by a first Write, or by
// a Flush through http.ResponseController; a hijack is refused.
func TestNoCookieLeavesThePublicListenerByAnyPath(t *testing.T) {
	for name, send := range map[string]func(http.ResponseWriter){
		"WriteHeader": func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) },
		"Write":       func(w http.ResponseWriter) { _, _ = io.WriteString(w, "ok") },
		"Flush":       func(w http.ResponseWriter) { _ = http.NewResponseController(w).Flush() },
	} {
		h := PagesPublicSurface(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: "muster_session", Value: "w0rd"})
			send(w)
			http.SetCookie(w, &http.Cookie{Name: "late", Value: "w0rd"})
		}), "")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/s/w0rd", nil))
		if sc := w.Result().Header.Values("Set-Cookie"); len(sc) != 0 {
			t.Errorf("by %s: the public listener sent Set-Cookie %v", name, sc)
		}
	}
	var hijackErr error
	h := PagesPublicSurface(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var c net.Conn
		c, _, hijackErr = http.NewResponseController(w).Hijack()
		if c != nil {
			_ = c.Close()
		}
	}), "")
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(srv.URL + "/s/w0rd")
	if err == nil {
		resp.Body.Close()
	}
	if !errors.Is(hijackErr, http.ErrNotSupported) {
		t.Errorf("a hijack through the public listener: %v, want ErrNotSupported", hijackErr)
	}
}

// TestNoLogLineCarriesAShareLinksToken: the access log, a recovered panic and
// the plugin-host refusal each name a share-link path, plain or
// percent-encoded, without its token. The access-log case is an INVARIANT
// guard (it was already redacted); the other two are the regression.
func TestNoLogLineCarriesAShareLinksToken(t *testing.T) {
	tok := strings.Repeat("T", 43)
	paths := []string{"/s/" + tok, "/%73/" + tok + "/content"}
	pats, err := plugins.ParseOrigins("https://*.preview.example.net")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		var buf bytes.Buffer
		s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "public-pages-operator-pw"}, log.New(&buf, "", 0))
		s.UseExtensions(Extensions{PluginOrigins: pats})
		panicky := s.withMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("fixture panic") }))
		panicky.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
		r := httptest.NewRequest(http.MethodGet, p, nil)
		r.Host = "app.preview.example.net"
		s.Handler().ServeHTTP(httptest.NewRecorder(), r)
		s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
		out := buf.String()
		for _, want := range []string{"PANIC GET /s/[redacted]", "refused GET /s/[redacted] for Host", "GET /s/[redacted] 404"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: no %q line in the log (a positive control: the line must exist):\n%s", p, want, out)
			}
		}
		if strings.Contains(out, tok) {
			t.Errorf("%s: the token is in the log:\n%s", p, out)
		}
	}
}
