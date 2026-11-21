package api

import (
	"html"
	"net/http"
	"net/url"
	"strings"
)

// hostCookiePrefix is the cookie-name prefix a browser enforces: it stores a
// cookie named `__Host-…` only when the cookie is Secure, has Path=/ and has
// NO Domain attribute. A page on any other host — a sibling subdomain above
// all — can therefore never set one that this host would read ("cookie
// tossing"), which is the whole point of using it.
const hostCookiePrefix = "__Host-"

// cookieBases is every cookie name muster-server reads or writes, unprefixed.
// It is the ledger the legacy-name expiry walks and the cookie golden pins;
// a cookie added without a row here is one that would keep its old name on a
// prefixed deployment and never be expired, so TestCookieLiteralsAndSignInCallersAreLedgered
// pins every http.Cookie literal in the package by its enclosing function.
var cookieBases = []string{
	sessionCookieName,
	accessCookieName,
	refreshCookieName,
	knownDeviceCookieName,
	pushDeviceCookie,
}

// HostPrefixedCookies reports whether this deployment names its cookies with
// the `__Host-` prefix: the public URL is https AND its cookies are Secure —
// in gotrue mode always (its cookies are unconditionally Secure, whatever
// MUSTER_SECURE_COOKIES says), in password mode when the RESOLVED
// secure-cookie setting is on.
//
// 🔴 THE NAME IS CHOSEN PER DEPLOYMENT, NEVER PER REQUEST. The Secure flag on
// an unprefixed deployment is per request (passwordCookieSecure drops it on a
// loopback http page), and loopbackHTTPPage reads Origin/Referer — which a
// sign-in POST carries and the next GET (no Origin; Referrer-Policy:
// no-referrer) does not. A name chosen that way would be written under one name
// and read under the other: a sign-in loop. So it is a function of
// configuration only. It is the one place the rule lives: every reader and
// writer goes through cookieName, and cmd/muster-server's boot banner and boot
// refusal read this same method.
//
// Off for the Compose stack (MUSTER_SECURE_COOKIES=1 on an http://localhost
// URL): there the loopback exception drops Secure, and a `__Host-` cookie
// without Secure is refused by every browser.
func (c AuthConfig) HostPrefixedCookies() bool {
	if !c.SecureCookies && !c.goTrueMode() {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(c.PublicURL))
	return err == nil && strings.EqualFold(u.Scheme, "https") && u.Host != ""
}

// cookieName is base as this deployment names it: `__Host-`+base when
// HostPrefixedCookies, base otherwise. A prefixed deployment never reads the
// unprefixed name.
func (c AuthConfig) cookieName(base string) string {
	if c.HostPrefixedCookies() {
		return hostCookiePrefix + base
	}
	return base
}

// cookieSignInPaths are the browser POSTs that mint a sign-in or step-up
// cookie. On a prefixed deployment one of them arriving as a loopback plain-http
// page (loopbackHTTPPage) is refused before its handler runs: the cookie it
// would set is `__Host-`, so it must be Secure, and WebKit drops a Secure cookie
// on http://localhost — the sign-in would "succeed" and stick in one engine
// only. TestCookieLiteralsAndSignInCallersAreLedgered pins the functions that set a
// sign-in cookie, so a new one has to be routed here or argued out.
var cookieSignInPaths = map[string]bool{
	loginPath:                             true,
	InvitePath:                            true,
	ResetPath:                             true,
	ClaimPath:                             true,
	passkeySignInVerifyPath:               true,
	stepUpConfirmPath:                     true,
	stepUpConfirmPath + "/passkey/verify": true,
}

// hostCookieNames is the middleware half of the `__Host-` rename. On a prefixed
// deployment it (1) refuses a sign-in POST from a loopback plain-http page with
// a page naming MUSTER_PUBLIC_URL, setting no cookie, and (2) expires every
// old-named cookie the request carries, so no stale unprefixed copy lingers
// after the upgrade. On an unprefixed deployment it does nothing.
func (s *Server) hostCookieNames(next http.Handler) http.Handler {
	if !s.auth.HostPrefixedCookies() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && cookieSignInPaths[r.URL.Path] && loopbackHTTPPage(r) {
			s.refuseLoopbackSignIn(w, r)
			return
		}
		// 🔴 NOT ON THE UI KIT. /ui-kit/ answers are public and long-cached
		// (?v= immutable): a Set-Cookie on one would be cached with it and
		// replayed to whoever fetched it next, and the contract says the kit
		// carries none (docs/ui-kit.md). The legacy cookie is expired on the
		// next page instead.
		if !strings.HasPrefix(r.URL.Path, uiKitPathPrefix) {
			expireLegacyCookies(w, r)
		}
		next.ServeHTTP(w, r)
	})
}

// refuseLoopbackSignIn is the page a loopback http sign-in gets on a prefixed
// deployment: it names MUSTER_PUBLIC_URL and links to its sign-in page (the
// Compose + Tailscale quickstart's desktop path), and sets no cookie.
func (s *Server) refuseLoopbackSignIn(w http.ResponseWriter, r *http.Request) {
	public := strings.TrimRight(strings.TrimSpace(s.auth.PublicURL), "/")
	const heading = "Sign in at this muster's address"
	inner := `<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">` + heading + `</h1>
<p class="text-sm text-fg2">This muster is set up for ` + html.EscapeString(public) + `. Sign in there.</p>
<p class="text-sm"><a class="font-semibold text-accent underline" href="` + html.EscapeString(public+loginPath) + `">Sign in at ` +
		html.EscapeString(public) + `</a></p>
</section>`
	writeAuthDocument(w, http.StatusForbidden, s.uiBrand(r.Context()), heading, inner)
}

// expireLegacyCookies adds a Max-Age=0 Set-Cookie for each unprefixed muster
// cookie r carries. Secure so it can overwrite a Secure original; Path=/ and no
// Domain, as every original was.
func expireLegacyCookies(w http.ResponseWriter, r *http.Request) {
	for _, base := range cookieBases {
		if _, err := r.Cookie(base); err != nil {
			continue
		}
		http.SetCookie(w, &http.Cookie{
			Name: base, Value: "", Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: true, MaxAge: -1,
		})
	}
}
