package api

import (
	"io"
	"net/http"
	"path"
	"strings"

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
// (requirePageLink); this listener also takes every cookie and credential
// header off the request before muster sees it, and every Set-Cookie off the
// response, so an owner's session cookie sent to the public address grants
// nothing — not even on the allowlisted routes — and none is ever set there.
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
// PagesPublicRoutes, without any cookie or credential header, and answers 404
// for everything else.
func PagesPublicSurface(h http.Handler) http.Handler {
	allowed := http.NewServeMux()
	pass := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&noCookieWriter{ResponseWriter: w}, withoutCredentials(r))
	})
	for _, p := range PagesPublicRoutes {
		allowed.Handle(p, pass)
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
		allowed.ServeHTTP(w, r)
	})
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

// pagesPublicDroppedHeaders are the request headers that carry a credential
// to some muster route. Every X-Muster-* header is dropped as well.
var pagesPublicDroppedHeaders = []string{"Cookie", "Authorization", "Proxy-Authorization", "X-Api-Key"}

// withoutCredentials is r without a cookie or a credential header.
func withoutCredentials(r *http.Request) *http.Request {
	r2 := r.Clone(r.Context())
	for _, k := range pagesPublicDroppedHeaders {
		r2.Header.Del(k)
	}
	for k := range r2.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Muster-") {
			r2.Header.Del(k)
		}
	}
	return r2
}

// noCookieWriter drops every Set-Cookie before the header is written.
type noCookieWriter struct{ http.ResponseWriter }

func (w *noCookieWriter) WriteHeader(code int) {
	w.ResponseWriter.Header().Del("Set-Cookie")
	w.ResponseWriter.WriteHeader(code)
}

func (w *noCookieWriter) Write(b []byte) (int, error) {
	w.ResponseWriter.Header().Del("Set-Cookie")
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection.
func (w *noCookieWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
