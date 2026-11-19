package api

import (
	"bytes"
	"net/http"
	"strings"
)

// THE NOT-FOUND PAGE (jargon audit F096): a browser that asks for a page
// muster does not have gets a page in muster's own look that says so in
// plain words, never Go's bare "404 page not found" text.
//
// 🔴 ONE PLACE, EVERY SOURCE. That text has two writers: the mux itself, for
// a path no route matches (/nonexistent), and every handler that calls
// http.NotFound (/agents/<unknown>, /invite or /auth/claim with no link, …).
// Swapping it where it leaves the server catches both, and every future
// http.NotFound, without touching a single handler.
//
// 🔴 ONLY A BROWSER'S PAGE LOAD, AND ONLY THAT EXACT BODY. A GET or HEAD that
// asks for HTML (a navigation) is answered with the page; an API client, a
// script or curl keeps the plain 404 it may parse. A 404 whose body is
// anything else — a handler's own JSON error, its own notice page — passes
// through untouched: the swap matches http.NotFound's exact text, nothing
// broader.

// goNotFoundBody is what http.NotFound (and the mux's no-match) writes.
const goNotFoundBody = "404 page not found\n"

// NotFoundHeading and NotFoundBody are the page's words.
const (
	NotFoundHeading = "We couldn't find that page."
	NotFoundBody    = "The link may be old, or the page may have moved."
)

// notFoundInner is the page's card. It is drawn as the standalone page
// (writeAuthDocument) whoever asks: this layer sits outside every route's
// sign-in check, so it does not know who is signed in, and the page names
// nothing a signed-out visitor should not see.
const notFoundInner = `<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">` + NotFoundHeading + `</h1>
<p class="text-sm text-fg2">` + NotFoundBody + `</p>
<a href="/" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to muster</a>
</section>`

// wantsPage reports whether r is a browser loading a page.
func wantsPage(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return r.Header.Get("Sec-Fetch-Dest") == "document" || strings.Contains(r.Header.Get("Accept"), "text/html")
}

// plainNotFound swaps Go's bare not-found text for the not-found page, on a
// browser's page load.
func (s *Server) plainNotFound(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wantsPage(r) {
			next.ServeHTTP(w, r)
			return
		}
		nf := &notFoundWriter{ResponseWriter: w}
		next.ServeHTTP(nf, r)
		if !nf.held {
			return
		}
		if nf.buf.String() == goNotFoundBody {
			h := w.Header()
			h.Del("Content-Type")
			h.Del("Content-Length")
			writeAuthDocument(w, http.StatusNotFound, s.uiBrand(r.Context()), NotFoundHeading, notFoundInner)
			return
		}
		nf.release()
	})
}

// notFoundWriter holds back a plain-text 404 until its body shows whether it
// is http.NotFound's; everything else is written straight through.
type notFoundWriter struct {
	http.ResponseWriter
	wrote bool // the status has been decided
	held  bool // a plain-text 404 is being held back
	buf   bytes.Buffer
}

func (n *notFoundWriter) WriteHeader(code int) {
	if n.wrote {
		return
	}
	// An informational status (103 Early Hints) is not the response's
	// status: passed on, and the final one still to come. Except 101
	// Switching Protocols, which IS final (the connection becomes another
	// protocol): it is decided like any other status, below.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		n.ResponseWriter.WriteHeader(code)
		return
	}
	n.wrote = true
	if code == http.StatusNotFound && strings.HasPrefix(n.Header().Get("Content-Type"), "text/plain") {
		n.held = true
		return
	}
	n.ResponseWriter.WriteHeader(code)
}

func (n *notFoundWriter) Write(p []byte) (int, error) {
	if !n.wrote {
		n.WriteHeader(http.StatusOK)
	}
	if !n.held {
		return n.ResponseWriter.Write(p)
	}
	n.buf.Write(p)
	if n.buf.Len() > len(goNotFoundBody) {
		// Longer than http.NotFound's text: someone else's 404. Let it go.
		n.release()
	}
	return len(p), nil
}

// release writes a held 404 as it was and stops holding.
func (n *notFoundWriter) release() {
	if !n.held {
		return
	}
	n.held = false
	n.ResponseWriter.WriteHeader(http.StatusNotFound)
	_, _ = n.ResponseWriter.Write(n.buf.Bytes())
	n.buf.Reset()
}

func (n *notFoundWriter) Flush() {
	if n.held {
		n.release()
	}
	if f, ok := n.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (n *notFoundWriter) Unwrap() http.ResponseWriter { return n.ResponseWriter }
