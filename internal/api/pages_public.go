package api

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE PUBLIC PAGES LISTENER (MUSTER_PAGES_PUBLIC_LISTEN; docs/pages.md
// "Sharing with people outside your network"). An opt-in listener that serves
// a share link's viewer and NOTHING ELSE, so an operator can make it reachable
// from the internet while the rest of muster stays on their private network.
//
// 🔴 ONE LIST. PagesPublicRoutes is the whole surface: a request whose method
// and path match none of its patterns is answered 404 here, before muster's
// own handler runs at all. TestThePublicPagesListenerServesOnlyItsAllowlist
// walks every route RegisterRoutes records and demands that exactly the
// ledgered ones get through, so a route added to this list, or one of these
// routes removed from muster, fails it.
//
// 🔴 NO CREDENTIAL CROSSES IT. The share-link routes read no cookie already
// (requirePageLink); this listener also passes on ONLY the request headers
// PagesPublicRequestHeaders names (an allowlist: a cookie, a bearer, an
// X-Muster-* or any header a future route reads never reaches muster), and
// takes every Set-Cookie off the response, so an owner's session cookie sent
// to the public address grants nothing — not even on the allowlisted routes —
// and none is ever set there. Its server has its own read, write and idle
// timeouts (NewPagesPublicServer).
//
// It is a filter in front of the SAME handler the main listener serves
// (Server.Handler), not a second registration site: the routes, their rate
// limits, caps and headers are the main listener's own, unchanged.
// ---------------------------------------------------------------------------

// PagesPublicRoutes is every request the public pages listener passes on, as
// ServeMux patterns (a GET pattern also matches HEAD). Each one is a pattern
// RegisterRoutes registers, except the one static file the shell links, which
// is served by muster's "GET /static/" handler.
var PagesPublicRoutes = []string{
	// The viewer's own routes (pages.go, page_assets.go): the token is their
	// only credential.
	"GET " + pages.SharePrefix + "{token}",
	"GET " + pages.SharePrefix + "{token}/content",
	"POST " + pages.SharePrefix + "{token}/submit",
	"POST " + pages.SharePrefix + "{token}/upload",
	"GET " + pages.SharePrefix + "{token}/assets/{asset}",
	// The shell's header logo and its icon on a branded server
	// (ShellView.BrandLogo, Brand.Icon192), and muster's own icon on an
	// unbranded one. Nothing else under /static/ or /brand/.
	"GET " + ui.BrandLogoPath,
	"GET " + ui.BrandIcon192Path,
	"GET " + pagesPublicStaticIcon,
}

// pagesPublicStaticIcon is the shell's icon on an unbranded server
// (Brand.Icon192 without a logo), without its ?v= query.
const pagesPublicStaticIcon = "/static/icons/icon-192.png"

// pagesPublicNotFound is the body of every request the listener refuses.
const pagesPublicNotFound = "404 page not found\n"

// PagesPublicSurface wraps h (Server.Handler) so it serves only
// PagesPublicRoutes, with only PagesPublicRequestHeaders on the request, and
// answers 404 for everything else.
//
// clientIPHeader is MUSTER_CLIENT_IP_HEADER (clientaddr.Policy.Header; "" when
// unset), the one header added to the list: muster's client-address policy
// (internal/clientaddr, the outermost layer of h) believes it only from a peer
// in MUSTER_TRUSTED_PROXIES, so passing it on lets a proxy name each viewer
// for the rate limits without letting a viewer name itself.
func PagesPublicSurface(h http.Handler, clientIPHeader string) http.Handler {
	keep := pagesPublicHeaderSet(clientIPHeader)
	allowed := http.NewServeMux()
	pass := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = onlyHeaders(r, keep)
		h.ServeHTTP(&noCookieWriter{ResponseWriter: w}, r.WithContext(context.WithValue(r.Context(), pagesPublicKey{}, true)))
	})
	// The mux only MATCHES (Handler, below); it never serves. Its handler is a
	// declared no-op so the package's route analysers can name it.
	for _, p := range PagesPublicRoutes {
		allowed.HandleFunc(p, pagesPublicMatchOnly)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler reports "" for no match and for a method the path's pattern
		// does not take (ServeMux's own 405): both are the one 404 here, so
		// nothing says which paths exist. A path that is not canonical
		// (ServeMux's redirect) is refused too, rather than redirected.
		if !canonicalPath(r.URL.Path) {
			refusePagesPublic(w)
			return
		}
		if _, pat := allowed.Handler(r); pat == "" {
			refusePagesPublic(w)
			return
		}
		pass(w, r)
	})
}

// pagesPublicMatchOnly is the handler PagesPublicSurface's matching mux holds
// for each allowlisted pattern. It is never served: a match is passed to
// muster's own handler instead.
func pagesPublicMatchOnly(http.ResponseWriter, *http.Request) {}

// pagesPublicKey marks a request that arrived on the public pages listener.
// The key is unexported, so no request can carry the mark any other way.
type pagesPublicKey struct{}

// viaPagesPublic reports whether r arrived on the public pages listener.
func viaPagesPublic(r *http.Request) bool {
	v, _ := r.Context().Value(pagesPublicKey{}).(bool)
	return v
}

// The public pages server's timeouts (NewPagesPublicServer). Vars so a test
// can shorten them.
var (
	// pagesPublicReadTimeout bounds reading a request, its body included,
	// unless the route sets its own read deadline: an upload does
	// (pageUploadReadTimeout, a longer one), and so does a submit
	// (pageSubmitReadTimeout).
	pagesPublicReadTimeout = 30 * time.Second
	// pagesPublicIdleTimeout bounds a kept-alive connection between requests.
	pagesPublicIdleTimeout = time.Minute
)

// pagesPublicWriteTimeout bounds a request from its headers to the end of
// its answer. 🔴 IT IS DERIVED FROM THE UPLOAD'S OWN BUDGET, so it can never
// cut an upload off that the upload's deadlines let through: the body's read
// deadline (pageUploadReadTimeout), then the store's (30 s after it,
// pageUpload's context), then a margin to write the answer. An asset (at
// most 15 MiB) has the same time to reach a slow viewer.
func pagesPublicWriteTimeout() time.Duration {
	return pageUploadReadTimeout + 30*time.Second + 30*time.Second
}

// NewPagesPublicServer is the public pages listener's server on addr:
// PagesPublicSurface over h, with its own read, write and idle timeouts, so a
// client that sends slowly, reads slowly or idles is cut off.
func NewPagesPublicServer(addr string, h http.Handler, clientIPHeader string, readHeaderTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           PagesPublicSurface(h, clientIPHeader),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       pagesPublicReadTimeout,
		WriteTimeout:      pagesPublicWriteTimeout(),
		IdleTimeout:       pagesPublicIdleTimeout,
	}
}

// canonicalPath reports whether p is a path ServeMux serves as it is, rather
// than redirecting: absolute, with no empty, "." or ".." segment.
func canonicalPath(p string) bool {
	return strings.HasPrefix(p, "/") && path.Clean(p) == strings.TrimSuffix(p, "/") || p == "/"
}

// refusePagesPublic is the public listener's one refusal.
func refusePagesPublic(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, pagesPublicNotFound)
}

// PagesPublicRequestHeaders is EVERY request header the public pages
// listener passes on; any other is removed before muster sees the request.
// A name ending in "*" is a prefix. It is what the share routes, the shell's
// files and the middleware in front of them read: the body's type, the
// content negotiation and caching headers, Sec-Fetch-Dest (the content
// document is served only into a frame), Origin, and the two a log or a
// probe may want. No credential header is on it, and none may be added.
var PagesPublicRequestHeaders = []string{
	"Accept", "Accept-Encoding", "Accept-Language",
	"Content-Type", "Content-Length",
	"If-Modified-Since", "If-None-Match", "Range",
	"Origin", "Referer", "User-Agent",
	"Sec-Fetch-*",
}

// pagesPublicHeaderSet is PagesPublicRequestHeaders, canonical, plus the
// configured client-address header ("" adds nothing).
func pagesPublicHeaderSet(clientIPHeader string) (set map[string]bool) {
	set = map[string]bool{}
	for _, k := range append(append([]string(nil), PagesPublicRequestHeaders...), clientIPHeader) {
		if prefix, ok := strings.CutSuffix(k, "*"); ok {
			set[http.CanonicalHeaderKey(prefix)+"*"] = true
		} else if k != "" {
			set[http.CanonicalHeaderKey(k)] = true
		}
	}
	return set
}

// keepsHeader reports whether canonical header k is in set.
func keepsHeader(set map[string]bool, k string) bool {
	if set[k] {
		return true
	}
	for p := range set {
		if strings.HasSuffix(p, "*") && strings.HasPrefix(k, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// onlyHeaders is a copy of r carrying only the headers in set.
func onlyHeaders(r *http.Request, set map[string]bool) *http.Request {
	r2 := r.Clone(r.Context())
	for k := range r2.Header {
		if !keepsHeader(set, http.CanonicalHeaderKey(k)) {
			r2.Header.Del(k)
		}
	}
	return r2
}

// noCookieWriter drops every Set-Cookie before the header can go out:
// before WriteHeader, a first Write and a Flush, whichever comes first.
// Hijacking is refused, so nothing writes past it.
type noCookieWriter struct{ http.ResponseWriter }

func (w *noCookieWriter) strip() { w.ResponseWriter.Header().Del("Set-Cookie") }

func (w *noCookieWriter) WriteHeader(code int) {
	w.strip()
	w.ResponseWriter.WriteHeader(code)
}

func (w *noCookieWriter) Write(b []byte) (int, error) {
	w.strip()
	return w.ResponseWriter.Write(b)
}

// Flush strips, then flushes: http.ResponseController finds this before it
// would unwrap to the connection's own Flush.
func (w *noCookieWriter) Flush() {
	w.strip()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack is refused: a hijacked connection could write any header.
func (w *noCookieWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

// Unwrap lets http.ResponseController reach the connection's read and write
// deadlines (an upload sets its own); Flush and Hijack are answered above,
// before it unwraps.
func (w *noCookieWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
