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
//     expires every cookie it names, at every path prefix of the request.
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
//     whose destination is a document passes — or an iframe, on
//     /__muster/check only. Refused: every same-site or cross-site
//     subresource, fetch, form POST and WebSocket — preview A reaching
//     preview B — and every other iframe navigation;
//   - with no Sec-Fetch-Site (a browser that sends no Fetch Metadata), every
//     method but GET and HEAD must carry the preview's own Origin.
//
// 🔴 THE IFRAME NAVIGATION PASSES ON /__muster/check ONLY, which the gateway
// answers itself and which never reaches the app. That is muster's frame
// (Show here): w., same-site, navigates its frame to the entry route, which
// redirects to check, all in one same-site chain. Check's page then navigates
// on to the app itself (auth.go), which makes the app's request same-origin.
// Allowed on any path, the iframe rule would let preview A's script load
// p-B/<anything> in a hidden frame with B's cookie, unseen: frame-ancestors
// stops it being SHOWN, not the request reaching B's app.
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
		case "document":
			return ""
		case "iframe":
			if r.URL.Path == checkPath {
				return ""
			}
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

// The cookie-bomb reset's bounds: the path prefixes it expires cookies at,
// and the Set-Cookie headers it writes in all (so a crafted header cannot
// make the answer large).
const (
	maxBombPaths      = 8
	maxBombSetCookies = 512
)

// bombPaths are the paths a cookie sent with r can have been set at: every
// prefix of r's path at a "/" boundary, shortest first ("/", "/app",
// "/app/x" for /app/x), at most maxBombPaths. A prefix holding a character
// a Path attribute cannot carry ends the list.
func bombPaths(r *http.Request) []string {
	out := []string{"/"}
	p := r.URL.EscapedPath()
	for i := 1; i <= len(p) && len(out) < maxBombPaths; i++ {
		if i < len(p) && p[i] != '/' {
			continue
		}
		pre := strings.TrimSuffix(p[:i], "/")
		if pre == "" || pre == out[len(out)-1] {
			continue
		}
		if strings.ContainsFunc(pre, func(c rune) bool { return c <= ' ' || c >= 0x7f || c == ';' || c == ',' }) {
			break
		}
		out = append(out, pre)
	}
	return out
}

// cookieBomb answers a request whose Cookie header passed MaxCookieHeader: a
// page that expires every cookie it names (up to maxBombNames) wherever one
// of that name can live for this request — at every path prefix (bombPaths),
// host-only and at Domain=<zone> (where a tossed cookie lives), each plain
// and Partitioned (one tossed inside muster's frame is partitioned). A
// __Host- cookie can carry neither Domain nor a Path other than /, so it is
// expired host-only at / only. Shortest paths first, at most
// maxBombSetCookies headers.
func (g *Gateway) cookieBomb(w http.ResponseWriter, r *http.Request) {
	names := cookieNames(r, maxBombNames)
	n := 0
	add := func(v string) {
		if n < maxBombSetCookies {
			w.Header().Add("Set-Cookie", v)
			n++
		}
	}
	for _, p := range bombPaths(r) {
		for _, name := range names {
			host := strings.HasPrefix(name, "__Host-")
			if host && p != "/" {
				continue
			}
			for _, dom := range []string{"", "; Domain=" + g.cfg.Domain} {
				if host && dom != "" {
					continue
				}
				v := name + "=; Path=" + p + dom + "; Max-Age=0; Secure"
				add(v)
				add(v + "; Partitioned")
			}
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
