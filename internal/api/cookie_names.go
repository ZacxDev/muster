package api

import (
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
// the `__Host-` prefix: the RESOLVED secure-cookie setting is on AND the public
// URL is https.
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
	if !c.SecureCookies {
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
// only. TestCookieMintingFunctionsAreLedgered pins the functions that set a
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
			s.renderNotice(w, r, http.StatusForbidden, "Sign in at this muster's address",
				"This muster is set up for "+s.auth.PublicURL+". Sign in there.")
			return
		}
		expireLegacyCookies(w, r)
		next.ServeHTTP(w, r)
	})
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
