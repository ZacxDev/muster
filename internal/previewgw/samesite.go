package previewgw

// samesite.go — the defences a preview's host needs because every p-<id>
// host is SAME-SITE with every other until the zone is a public suffix
// (docs/design/previews.md, "Previews are same-site with each other"): script
// in preview A can send requests that carry preview B's cookie, and can toss
// Domain=<zone> cookies at every preview.
//
//   - Fetch Metadata and Origin (crossSiteRefusal): a request is refused unless
//     it is same-origin, user-initiated, or a GET navigation; a WebSocket must
//     carry its own preview's Origin.
//   - Cookie bombing (cookieBomb): a Cookie header past 8 KiB gets a page that
//     expires every cookie it names.
//   - What reaches the app (forwardCookies): never the gateway's own cookies.

import (
	"net/http"
	"strings"
)

// MaxCookieHeader is the most Cookie header bytes a p- request may carry;
// past it, the reset page answers instead (cookieBomb).
const MaxCookieHeader = 8 << 10

// maxBombNames bounds the cookies the reset page expires, so a crafted header
// of tiny names cannot make the answer large.
const maxBombNames = 128

// codeCrossSite is the refusal code of a cross-site or sibling request.
const codeCrossSite = "cross_site"

// gatewayCookie reports whether a cookie name is one the gateway sets or
// reserves: the owner cookie, the share-link cookie (phase 3), and muster's
// own muster_* cookies. None of them reaches an app, and an app may not set
// one.
func gatewayCookie(name string) bool {
	return strings.EqualFold(name, OwnerCookie) || strings.EqualFold(name, ShareCookie) || strings.HasPrefix(name, "muster_")
}

// ShareCookie is the phase-3 share-link cookie's name, reserved now so the
// proxy already keeps it from apps.
const ShareCookie = "__Host-mps"

// previewOrigin is the origin a browser gives p-<id>'s own pages.
func (g *Gateway) previewOrigin(id string) string {
	return "https://p-" + id + "." + g.cfg.Domain
}

// isWebSocket reports whether r asks for a WebSocket upgrade.
func isWebSocket(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// crossSiteRefusal is codeCrossSite when r must not reach the preview whose
// origin is origin, else "". The rules (the design's part 3):
//
//   - a WebSocket upgrade must carry Origin equal to the preview's own, with
//     or without Fetch Metadata;
//   - with Sec-Fetch-Site: same-origin and none (a typed or bookmarked URL)
//     pass; otherwise only a GET or HEAD navigation (Sec-Fetch-Mode: navigate)
//     whose destination is a document or an iframe passes. Refused: every
//     same-site or cross-site subresource, fetch, form POST and WebSocket —
//     preview A reaching preview B;
//   - with no Sec-Fetch-Site (a browser that sends no Fetch Metadata), every
//     method but GET and HEAD must carry the preview's own Origin.
//
// The iframe navigation is what muster's frame does (the session page on w.,
// same-site, navigates its own frame here). Another site cannot use it to
// show a preview: every preview response carries frame-ancestors with
// muster's origin only (proxy.go).
func crossSiteRefusal(r *http.Request, origin string) string {
	if isWebSocket(r) && r.Header.Get("Origin") != origin {
		return codeCrossSite
	}
	readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "":
		if !readOnly && r.Header.Get("Origin") != origin {
			return codeCrossSite
		}
		return ""
	case "same-origin", "none":
		return ""
	}
	if readOnly && r.Header.Get("Sec-Fetch-Mode") == "navigate" {
		switch r.Header.Get("Sec-Fetch-Dest") {
		case "document", "iframe":
			return ""
		}
	}
	return codeCrossSite
}

// cookieLen is the total size of r's Cookie headers.
func cookieLen(r *http.Request) int {
	n := 0
	for _, v := range r.Header.Values("Cookie") {
		n += len(v)
	}
	return n
}

// cookieNames are the distinct, well-formed cookie names in r's Cookie
// headers, in order, at most max.
func cookieNames(r *http.Request, max int) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range r.Header.Values("Cookie") {
		for _, pair := range strings.Split(v, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(pair), "=")
			if !cookieToken(name) || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
			if len(out) == max {
				return out
			}
		}
	}
	return out
}

// cookieToken reports whether s is a valid cookie name (an RFC 7230 token).
func cookieToken(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, c := range []byte(s) {
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`()<>@,;:\"/[]?={}`, c) >= 0 {
			return false
		}
	}
	return true
}

// cookieBomb answers a request whose Cookie header passed MaxCookieHeader: a
// page that expires every cookie it names (up to maxBombNames), both
// host-only and at Domain=<zone>, where a tossed cookie lives. A __Host-
// cookie cannot carry Domain, so it is expired host-only only.
func (g *Gateway) cookieBomb(w http.ResponseWriter, r *http.Request) {
	for _, name := range cookieNames(r, maxBombNames) {
		w.Header().Add("Set-Cookie", name+"=; Path=/; Max-Age=0; Secure")
		if !strings.HasPrefix(name, "__Host-") {
			w.Header().Add("Set-Cookie", name+"=; Path=/; Domain="+g.cfg.Domain+"; Max-Age=0; Secure")
		}
	}
	g.renderCode(w, http.StatusRequestHeaderFieldsTooLarge, codeTooManyCookies)
}

// forwardCookies is r's Cookie header without the gateway's cookies
// (gatewayCookie), or "" when nothing is left. Every other pair passes as the
// browser sent it.
func forwardCookies(h http.Header) string {
	var keep []string
	for _, v := range h.Values("Cookie") {
		for _, pair := range strings.Split(v, ";") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			name, _, _ := strings.Cut(pair, "=")
			if gatewayCookie(strings.TrimSpace(name)) {
				continue
			}
			keep = append(keep, pair)
		}
	}
	return strings.Join(keep, "; ")
}
