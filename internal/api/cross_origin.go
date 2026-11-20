package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/metrics"
)

// The central origin check for cookie-authenticated writes
// (docs/design/meetings.md, "a central origin check", core PR 1g).
//
// 🔴 SameSite=Lax DOES NOT STOP A SIBLING HOST. "Site" is the registrable
// domain, so a page on any sibling subdomain of muster's host is SAME-SITE,
// and the browser attaches muster's session cookie to a write it makes to
// muster. requireSameOrigin closed that only on the routes that opted in to
// it; this check closes it on every route a cookie admits, because it runs in
// admit, the one place every auth wrapper hands its request on.
//
// The rule, for a request whose method is not GET, HEAD or OPTIONS and whose
// principal was admitted by a browser session (authz.ViaSession: the password
// cookie or the GoTrue cookies alike):
//
//  1. Sec-Fetch-Site, FIRST. same-origin admits; any other value (same-site,
//     cross-site, none) refuses.
//  2. Only when it is absent, Origin: one of muster's own origins admits;
//     "null" or any other origin refuses.
//  3. Only when both are absent, Referer: a same-origin Referer admits.
//  4. None of the three: refused.
//
// 🔴 WHY Sec-Fetch-Site COMES FIRST: muster's app-wide Referrer-Policy is
// no-referrer, and under it a browser sends `Origin: null` on a native form
// POST navigation — muster's own no-JS forms — while still sending
// `Sec-Fetch-Site: same-origin` (measured in Chromium, Firefox and WebKit). An
// Origin-first rule would refuse muster's own forms.
//
// ⚠ A browser sends Sec-Fetch-Site only to a potentially trustworthy origin
// (https, or loopback). A plain-http deployment on a non-loopback address gets
// none, falls to Origin, and a native form POST there carries `Origin: null`:
// refused. htmx and fetch writes carry the real Origin and are admitted. See
// docs/auth.md, "Cross-origin writes".
//
// Steps 1 and 2 are net/http.CrossOriginProtection's, with muster's public
// origin trusted beside the request's own Host. What it does not do, and this
// adds before it: refuse `Sec-Fetch-Site: none`, refuse `Origin: null`, and
// refuse a request carrying none of the headers (the standard library lets
// that through for non-browser clients; a cookie request with none of them is
// not one of muster's pages, and every non-browser client of muster presents a
// bearer token, which this check does not read).

// The refusal reasons: the reason label of muster_csrf_refusals_total and the
// log line's. A closed set.
const (
	csrfReasonFetchSite = "sec-fetch-site"
	csrfReasonNullOrig  = "origin-null"
	csrfReasonForeign   = "origin-foreign"
	csrfReasonReferer   = "referer-foreign"
	csrfReasonNoHeaders = "no-headers"
)

// isSafeMethod reports whether a request with method m changes nothing by
// muster's own rule (handleShare documents it): GET, HEAD and OPTIONS.
func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// crossOriginWriteRefusal is the check: "" admits r, anything else is the
// reason it is refused. It does not look at who admitted r; the caller does.
func (s *Server) crossOriginWriteRefusal(r *http.Request) string {
	if isSafeMethod(r.Method) {
		return ""
	}
	site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	switch {
	case site == "none":
		// CrossOriginProtection admits "none" (a user-initiated request). A
		// write muster's pages make is never one.
		return csrfReasonFetchSite
	case site == "" && origin == "null":
		return csrfReasonNullOrig
	case site == "" && origin == "":
		// CrossOriginProtection admits this (a non-browser client). Here only
		// a same-origin Referer does.
		if strings.TrimSpace(r.Header.Get("Referer")) == "" {
			return csrfReasonNoHeaders
		}
		if s.isOwnOriginURL(r.Header.Get("Referer"), r) {
			return ""
		}
		return csrfReasonReferer
	}
	if err := s.crossOriginProtection().Check(r); err != nil {
		if site != "" {
			return csrfReasonFetchSite
		}
		return csrfReasonForeign
	}
	return ""
}

// crossOriginProtection is the standard library's check with muster's public
// origin (MUSTER_PUBLIC_URL) trusted: behind a proxy that rewrites Host, the
// browser's Origin is the public one. The request's own Host is compared by
// the check itself. Built per write; it is a map and a mutex.
//
// ⚠ A TRUSTED ORIGIN IS ADMITTED WHATEVER Sec-Fetch-Site SAYS (the standard
// library exempts it before reading the header's value). That is safe: a
// browser sets Origin itself, so only a page ON muster's public origin can
// send it.
func (s *Server) crossOriginProtection() *http.CrossOriginProtection {
	c := http.NewCrossOriginProtection()
	if o := s.publicOrigin(); o != "" {
		// An origin that does not parse as one is refused by
		// AddTrustedOrigin; nothing is trusted in its place.
		_ = c.AddTrustedOrigin(o)
	}
	return c
}

// publicOrigin is MUSTER_PUBLIC_URL's origin, lowercased as a browser sends
// it ("" when unset or not a URL).
func (s *Server) publicOrigin() string {
	if base := s.ext.PublicURL; base != "" {
		if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
			return strings.ToLower(u.Scheme + "://" + u.Host)
		}
	}
	return ""
}

// isOwnOriginURL is the Origin step's own-origin rule applied to a Referer
// (step 3), so the two agree: its host is the request's Host (compared as
// CrossOriginProtection compares Origin with Host), or its scheme and host are
// MUSTER_PUBLIC_URL's origin. (requireSameOrigin's isSameOrigin is looser: any
// scheme, host compared without case. It is left as it is.)
func (s *Server) isOwnOriginURL(raw string, r *http.Request) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	po := s.publicOrigin()
	return po != "" && strings.ToLower(u.Scheme+"://"+u.Host) == po
}

// refuseCrossOriginCookieWrite answers 403 and reports true when p was
// admitted by a browser session and r is a write the check refuses. admit
// calls it before anything else, so a refused request reaches no handler.
//
// 🔴 IT KEYS ON THE DOOR, NOT ON THE HEADERS. A bearer credential (a personal
// mst_ token, a plugin's mpl_, a runner's, an agent's, the hook and service
// secrets, a device's) is something a cross-site page cannot make a browser
// attach, so its request is not checked. A request carrying both a cookie and
// a bearer is judged by the credential that admitted it: p.Via.
func (s *Server) refuseCrossOriginCookieWrite(w http.ResponseWriter, r *http.Request, p authz.Principal) bool {
	if p.Via != authz.ViaSession {
		return false
	}
	return s.refuseCrossOriginWrite(w, r)
}

// refuseCrossOriginWrite applies the check to r whoever admitted it, and
// answers, counts and logs a refusal. admit's cookie door calls it, and so
// does password-mode POST /logout, which reads the session itself.
func (s *Server) refuseCrossOriginWrite(w http.ResponseWriter, r *http.Request) bool {
	reason := s.crossOriginWriteRefusal(r)
	if reason == "" {
		return false
	}
	metrics.CSRFRefusals.WithLabelValues(csrfRouteClass(r.Pattern), reason).Inc()
	// The body is never logged; the headers the check read are.
	s.logger.Printf("refused a cookie-authenticated cross-origin %s %s (%s; Sec-Fetch-Site %q, Origin %q)",
		r.Method, r.URL.Path, reason, r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin"))
	s.refuseCrossOrigin(w, r)
	return true
}

// csrfRouteClass is the route_class label: the first path segment of the matched
// mux pattern ("POST /tasks/{id}/edit" → "tasks"), so its values are bounded
// by the registered routes. A wildcard segment or no pattern is "other".
func csrfRouteClass(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	pattern = strings.TrimPrefix(pattern, "/")
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		pattern = pattern[:i]
	}
	if pattern == "" || strings.ContainsAny(pattern, "{}") {
		return "other"
	}
	return pattern
}
