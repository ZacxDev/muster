package api

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/loopback"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/web"
)

// --- The human login tier ----------------------------------------------------
//
// 🔴 THIS PAGE IS DELIBERATELY NOT AN internal/ui DOCUMENT, and the reason is
// operational rather than stylistic. It is the one page that must render while
// the rest of the app is refusing, so it carries no shared shell, no htmx, no
// sidebar and no dependency on the ui package's component graph — a refactor
// there cannot lock the operator out. It also keeps it out of that package's
// standaloneDocuments registry, which would otherwise subject a bare auth form
// to ~20 shell/tab/panel sweeps that have nothing to say about it.
//
// ⚠ THAT DODGES THE a11y SWEEPS, SO THE COVERAGE IS REPLACED, NOT DROPPED:
// TestLoginPageIsAccessible pins the properties those sweeps would have checked
// (exactly one h1, a label bound to the input, no low-contrast idiom, a typed
// password field with the right autocomplete) against this markup specifically.

// loginPath is the login route, spelled once so the handler, the redirect in
// refuseUnauthenticated and the tests cannot drift.
const loginPath = "/login"

// loginRateBurst / loginRateWindow count wrong operator passwords, process-wide.
//
// 🔴 THE COUNTS HOLD FAILURES ONLY, AND THEY REFUSE ONLY UNKNOWN DEVICES
// (passwordGate, known_device.go). Past loginRateBurst in a window from one
// client address (or loginRateGlobalBurst across all of them), a request with
// no valid known-device cookie (and, at sign-in, no valid session) is refused before the
// compare, correct password or not, so a guesser is held to loginRateBurst a
// minute per address; a browser that signed in correctly before is never
// refused with the correct password, so a junk flood cannot lock the operator
// out of a browser they use (TestAJunkFloodDoesNotLockAKnownDeviceOut). A wrong
// password from a known device still counts.
const (
	loginRateBurst  = 10
	loginRateWindow = time.Minute
)

// loginPathFor returns where an unauthenticated caller should be sent, carrying
// the path it was trying to reach so login can return it there.
//
// Only the path+query of the CURRENT request is preserved, and it is re-emitted
// as a relative URL — never a caller-supplied absolute one. That is what stops
// this becoming an open redirect: see safeReturnTo.
func loginPathFor(r *http.Request) string {
	want := r.URL.RequestURI()
	if want == "" || want == "/" || strings.HasPrefix(want, loginPath) {
		return loginPath
	}
	return loginPath + "?next=" + url.QueryEscape(want)
}

// safeReturnTo sanitises the ?next= parameter.
//
// 🔴 AN OPEN REDIRECT IS THE STANDARD BUG IN EXACTLY THIS PARAMETER, so the rule
// is allow-list shaped rather than deny-list shaped: the value must begin with a
// single "/" and must not begin with "//" (which a browser reads as a
// scheme-relative URL to ANOTHER HOST — the case a naive `strings.HasPrefix(v,
// "/")` check lets straight through). Anything else falls back to "/".
func safeReturnTo(v string) string {
	if !localPath(v) {
		return "/"
	}
	u, err := url.Parse(v)
	// The DECODED path must pass the same test: "/%2F%2Fevil.example" and
	// "/%5Cevil" decode to a second slash or a backslash.
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil || !localPath(u.Path) || dotSegment(u.Path) {
		return "/"
	}
	return v
}

// dotSegment reports whether the DECODED path p has a "." or ".." segment
// ("/agents/../tasks", and so "/agents/%2e%2e/tasks"): a browser resolves it
// to another path than the one a prefix check read, so a return address that
// names one is refused rather than normalised.
func dotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// localPath reports whether p is a path on THIS origin: one leading "/", no
// "//" or "/\" start, and no byte a browser strips or rewrites before
// resolving — every control byte and space (≤ 0x20), DEL (0x7f) and a
// backslash anywhere.
//
// 🔴 THE CONTROL BYTES ARE THE CASE THE FIRST VERSION MISSED. It rejected
// CR/LF and "//" but not a TAB: "/\t/evil.example/x" passed every check,
// and browsers DELETE tab and newline from a URL before parsing it, leaving
// "//evil.example/x" — another host (TestSafeReturnToRefusesEveryOffOriginShape).
func localPath(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c <= 0x20 || c == 0x7f || c == '\\' {
			return false
		}
	}
	return !strings.HasPrefix(p, "//")
}

// registerLoginRoutes wires the human login tier. These routes are OPEN by
// necessity — they are how a caller stops being anonymous — and they are the
// only browser-facing routes that are.
// 🔴 THE PATTERNS ARE WRITTEN AS LITERALS, NOT AS "GET "+loginPath, AND THAT IS
// LOAD-BEARING. Four ledgers in this package reason about routes by reading the
// SOURCE (terminal_write_ledger_test.go, and the human-tier ledger in
// browser_auth_test.go), and a computed pattern is invisible to every one of
// them — a `send-keys` route added that way would reach a real host with no
// ledger and nothing red. TestTheLoginPathConstantMatchesItsRoutes pins the
// literal against loginPath, so the duplication cannot drift.
func (s *Server) registerLoginRoutes(mux Mux) {
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("POST /auth/clear-caches", s.handleClearCaches)
	s.registerAccountRoutes(mux)
	s.registerAppSignInRoutes(mux)
}

// handleLoginPage renders the form. An already-signed-in caller is sent on
// rather than shown a second login.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.goTrueMode() {
		s.handleGoTrueLoginPage(w, r)
		return
	}
	if reason := s.auth.BrowserAuthRefusal(); reason != "" {
		// 🔴 THE REASON IS SHOWN, and it is safe to show: it names an env var the
		// operator controls and discloses no secret. Without it a fail-closed
		// deploy presents as a login form that rejects the correct password, and
		// the operator debugs the password instead of the deployment.
		s.renderLoginPage(w, http.StatusServiceUnavailable, "",
			"This server has no operator password configured ("+reason+"), so it cannot sign anyone in. "+
				"Set MUSTER_UI_PASSWORD in the muster secret and redeploy.")
		return
	}
	if s.hasValidSession(r) {
		http.Redirect(w, r, safeReturnTo(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.renderLoginPage(w, http.StatusOK, r.URL.Query().Get("next"), "")
}

// handleLoginSubmit checks the password and mints the session cookie.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if s.auth.goTrueMode() {
		s.handleGoTrueLoginSubmit(w, r)
		return
	}
	if reason := s.auth.BrowserAuthRefusal(); reason != "" {
		s.renderLoginPage(w, http.StatusServiceUnavailable, "",
			"This server has no operator password configured ("+reason+").")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLoginPage(w, http.StatusBadRequest, "", "That form could not be read.")
		return
	}
	next := r.FormValue("next")
	ok, refused, throttled := s.passwordGate(r, r.FormValue("password"), time.Now(), true)
	if refused || throttled {
		if !refused {
			s.logger.Printf("login: rejected a password attempt")
		}
		s.renderLoginPage(w, http.StatusTooManyRequests, next,
			"Too many attempts. Wait a minute and try again.")
		return
	}
	if !ok {
		s.logger.Printf("login: rejected a password attempt")
		s.renderLoginPage(w, http.StatusUnauthorized, next, "That password is not right.")
		return
	}

	s.setPasswordSessionCookie(w, r)
	s.setKnownDeviceCookie(w, r)
	s.logger.Printf("login: a browser session was issued")
	http.Redirect(w, r, safeReturnTo(next), http.StatusSeeOther)
}

// setPasswordSessionCookie mints a password-mode session issued NOW — the
// sign-in and the step-up confirm (stepup.go) are its two callers, and the
// issue time is what the step-up gate reads.
func (s *Server) setPasswordSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:  s.auth.cookieName(sessionCookieName),
		Value: auth.SignSession(s.auth.uiSessionSecret(), time.Now(), sessionTTL),
		Path:  "/",
		// HttpOnly: the cookie is never read by script, so keep it away from any
		// XSS that reaches this origin.
		HttpOnly: true,
		// Lax, not Strict: Strict withholds the cookie on a cross-site TOP-LEVEL
		// navigation, so following a link to muster from anywhere else would
		// land on the login page despite a valid session. Lax still withholds it
		// from cross-site POSTs, which is the CSRF case that matters here.
		SameSite: http.SameSiteLaxMode,
		Secure:   s.passwordCookieSecure(r),
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// passwordCookieSecure is the Secure attribute of a password-mode cookie set on
// the response to r: MUSTER_SECURE_COOKIES, EXCEPT on a page this machine
// serves itself over plain http on its loopback interface (loopbackHTTPPage).
// On a `__Host-` deployment (HostPrefixedCookies) it is always true: there is
// no exception, and a loopback http sign-in is refused instead (hostCookieNames).
//
// 🔴 WHY THE EXCEPTION, AND WHY IT IS SAFE. Chromium keeps a Secure cookie set
// over http://localhost (a loopback origin is a secure context); WebKit drops
// it, so a Secure session cookie there signs nobody in. MEASURED, not assumed:
// e2e/localhost's measure test logs it on every CI run (Playwright's Chromium
// KEPT, its WebKit DROPPED; that is Playwright's WebKit build on Linux — Safari
// on macOS itself is not measured there). The Compose stack sets
// MUSTER_SECURE_COOKIES=1 by default and listens on 127.0.0.1, so the first
// thing a new install does (open http://localhost:8105) would fail in a WebKit
// browser, and its sign-in is checked in both engines. Dropping Secure there
// costs nothing: Secure keeps a cookie off plain-http requests, and EVERY
// request to that origin is plain http on the loopback interface, which never
// leaves the machine. Anywhere else — https, a LAN address, a proxy in front
// (a Host it did not keep, a forwarded https) — the flag is exactly as set.
func (s *Server) passwordCookieSecure(r *http.Request) bool {
	if s.auth.HostPrefixedCookies() {
		return true // a `__Host-` cookie without Secure is refused by every browser
	}
	return s.auth.SecureCookies && !loopbackHTTPPage(r)
}

// loopbackHTTPPage reports whether r is a browser request from a page this
// server serves over plain http on the loopback interface: the request itself
// is plain http (no TLS, no forwarded https), and its Origin — or Referer, when
// the browser sent no Origin — is http://<loopback host> naming exactly r's
// own Host (so r's Host is loopback too: that comparison is the Host check).
// All of it, so a request through any proxy (a Host it rewrote to localhost
// keeps its https Origin; one that kept the Host is not loopback) and a request
// with neither header (curl, a script) keep the configured flag.
func loopbackHTTPPage(r *http.Request) bool {
	if r == nil || r.TLS != nil {
		return false
	}
	if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); p != "" && !strings.EqualFold(p, "http") {
		return false
	}
	src := strings.TrimSpace(r.Header.Get("Origin"))
	if src == "" {
		src = strings.TrimSpace(r.Header.Get("Referer"))
	}
	if !loopback.HTTPURL(src) {
		return false
	}
	u, err := url.Parse(src)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// handleLogout clears the session cookie. It is a POST because a GET logout is
// triggerable by any third-party page embedding an image.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth.goTrueMode() {
		s.handleGoTrueLogout(w, r)
		return
	}
	// 🔴 A sign-out READS the session (and deletes this browser's push
	// subscription with it), so a sibling page must not be able to post one:
	// the central origin check's rule (cross_origin.go), as on every cookie
	// write. The GoTrue path checks isSameOrigin itself.
	if s.refuseCrossOriginWrite(w, r) {
		return
	}
	// This browser's push subscription goes with its session (push_routes.go).
	if s.hasValidSession(r) {
		s.forgetPushDevice(w, r, s.sessionUserID(r))
	}
	clearSiteCaches(w)
	http.SetCookie(w, &http.Cookie{
		Name:     s.auth.cookieName(sessionCookieName),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.passwordCookieSecure(r),
		MaxAge:   -1,
	})
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

// clearSiteCaches asks the browser to drop everything it CACHED for this
// origin — the HTTP cache and the back/forward cache. The agent page, the
// Rendered view and the terminals are cacheable (viewDocument), so without it
// Back after a sign-out could show one of them from a cache to whoever holds
// the device next, and Back after the next person signs in could show them
// the last one's. Two kinds of response send it:
//
//   - a SIGN-OUT (handleLogout, handleGoTrueLogout), on the redirect itself:
//     the person handing the device on waits for it, and the sign-in page
//     only draws once the caches are gone;
//   - the sign-in pages' background request (handleClearCaches, which every
//     writeAuthDocument page starts: clearCachesHTML), for the session that
//     ended WITHOUT a sign-out (it expired, or was ended on another device).
//
// 🔴 NEVER ON A SIGN-IN RESPONSE. A browser does not follow a redirect that
// carries Clear-Site-Data until the whole clear has finished, and Chromium's
// clear walks EVERY entry in the browser's disk cache, every site's, to find
// this origin's — so its cost grows with the size of the person's cache, not
// with muster's. Measured in Chromium 153 between the sign-in's 303 arriving
// and the browser requesting its Location: 1–2 ms with an almost empty cache,
// 100–140 ms with 5,000 entries, 350–500 ms with 20,000, and 10.7 s for the
// first clear after the browser started with those 20,000 — all of it with
// the sign-in page still on screen. The sign-in page's own request clears the
// same caches while the person types, so nothing waits on it
// (TestNoSignInWaitsOnClearingTheCaches).
//
// 🔴 "cache" ONLY, NEVER "storage". "storage" also unregisters the service
// worker (and with it this device's push subscription) and empties IndexedDB,
// where the Rendered view's snapshots live — and those have their own purge
// (ui.SnapPurgeHTML, on the sign-in page every sign-out lands on). Asked for
// the caches only, the next person's device keeps its app.
//
// ⚠ IT IS A BEST EFFORT, AND NOT THE GUARD THAT MUST HOLD. Browsers honour
// Clear-Site-Data only on a secure origin (HTTPS, or localhost), so on a
// plain-HTTP deployment it does nothing at all. What holds everywhere is the
// restore's own check (ui restoreJS): a restored view page re-requests itself
// and reloads when that is refused, when the answer is another user's page
// (the user marker every view page carries, ui.UserMarker), or when what it
// showed is gone. Until that answer lands — one round trip — the restored
// page's content IS on screen; this header is what keeps it from being
// restored at all where it is honoured.
//
// Its callers are ledgered (TestEverySignInAndSignOutClearsTheCaches).
func clearSiteCaches(w http.ResponseWriter) {
	w.Header().Set("Clear-Site-Data", `"cache"`)
}

// clearCachesPath is the sign-in pages' background cache clear
// (handleClearCaches). Spelled once; the route literal in registerLoginRoutes
// is pinned to it (TestTheClearCachesPathMatchesItsRoute).
const clearCachesPath = "/auth/clear-caches"

// handleClearCaches answers the sign-in pages' background request with
// Clear-Site-Data: "cache" (clearSiteCaches): the clear a sign-in response used
// to carry, moved to where nobody waits for it. It reads no cookie and writes
// no state on the server; the only effect is in the asking browser, on this
// origin's caches.
//
// Same-origin only, so another site cannot make a visitor's browser drop
// muster's caches (a nuisance, not a leak: the next page load refetches).
func (s *Server) handleClearCaches(w http.ResponseWriter, r *http.Request) {
	if !s.isSameOrigin(r) {
		s.refuseCrossOrigin(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	clearSiteCaches(w)
	w.WriteHeader(http.StatusNoContent)
}

// clearCachesHTML is the inline script every writeAuthDocument page carries:
// it asks handleClearCaches to clear this origin's caches, in the background.
// A sign-in starts on one of these pages, so the previous session's cached
// pages are dropped while the person types — and the sign-in's own redirect
// carries no Clear-Site-Data for the browser to wait on (clearSiteCaches).
//
// keepalive, so a sign-in submitted before the answer lands (a password
// manager that fills and submits at once) does not cancel it. Measured in
// Chromium: the clear still ran, and still evicted the earlier page from the
// back/forward cache, with the page navigated away the moment it was sent.
func clearCachesHTML() string {
	return `<script>try{fetch('` + clearCachesPath + `',{method:'POST',keepalive:true,credentials:'same-origin'}).catch(function(){})}catch(e){}</script>`
}

// renderLoginPage writes the whole document. Hand-written rather than composed,
// for the independence reason at the top of this file.
func (s *Server) renderLoginPage(w http.ResponseWriter, code int, next, problem string) {
	inner := s.loginBrandHTML(context.Background()) + `<form method="POST" action="` + loginPath + `" class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Sign in to muster</h1>
<p class="text-sm text-fg2">Enter the password to continue.</p>
` + problemNode(problem) + `
<label for="muster-password" class="text-sm font-medium text-fg">Password</label>
<input id="muster-password" name="password" type="password" autocomplete="current-password" autofocus required
 class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">
<input type="hidden" name="next" value="` + html.EscapeString(safeReturnTo(next)) + `">
<button type="submit" class="press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Sign in</button>
</form>` + signInProgressJS
	inner += s.appSignInHTML(next)
	writeAuthDocument(w, code, s.uiBrand(context.Background()), "Sign in", inner)
}

// problemNode renders a server-authored failure message, or nothing.
func problemNode(problem string) string {
	if problem == "" {
		return ""
	}
	// role=alert so a screen reader announces the failure without the operator
	// hunting for it; the text is server-authored, and escaped regardless.
	return `<p role="alert" class="rounded-lg bg-st-error-bg px-3 py-2 text-sm text-st-error-fg ring-1 ring-inset ring-st-error-fg/40">` +
		html.EscapeString(problem) + `</p>`
}

// writeAuthDocument writes one of the BARE standalone pages — sign-in, the
// gotrue sign-in, the admin claim, an invite, a password reset, a notice — the
// same document with no navigation, so none of them depends on internal/ui's
// shell. title is escaped; inner is trusted markup built by this package.
//
// 🔴 ONLY A PAGE A SIGNED-OUT CALLER CAN BE SHOWN BELONGS HERE. A page behind
// requireSession goes through writeNavDocument, which draws the side nav.
// TestEveryAPIDocumentIsNavOrNamedBare holds the two sets.
//
// Every such page deletes the mirrored views' snapshots in this browser
// (ui.SnapPurgeHTML): a sign-out lands here, and so does any sign-in, so one
// user's cached conversations never outlive their session on a shared device.
// For the same reason each starts the background cache clear
// (clearCachesHTML). They also keep a server switcher hand-off (#muster-from)
// across the sign-in (ui.ServerHandoffKeepHTML).
func writeAuthDocument(w http.ResponseWriter, code int, brand ui.Brand, title, inner string) {
	doc := `<!doctype html>
<html class="dark" lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="color-scheme" content="dark light">
` + ui.ThemeHeadHTML() + `
` + ui.DocHeadHTML(brand) + `
<meta name="robots" content="noindex">
<title>` + html.EscapeString(title) + ` · muster</title>
<link rel="stylesheet" href="` + html.EscapeString(web.Asset("/static/app.css")) + `">
` + ui.SnapPurgeHTML() + `
` + clearCachesHTML() + `
` + ui.ServerHandoffKeepHTML() + `
</head>
<body class="flex min-h-dvh items-center justify-center bg-bg px-4 text-fg antialiased">
<main class="w-full max-w-sm">
` + inner + `
</main>
</body>
</html>`
	writeDocumentHeaders(w, code)
	_, _ = w.Write([]byte(doc))
}

// writeNavDocument writes a SIGNED-IN server-authored page — Attention,
// Account, API tokens, Credentials, Passkeys, Plugins, Browser flows, the
// admin pages — inside the app's navigation: ui.NavDocument, the same sidebar
// every other signed-in document draws. entry is the nav entry the page
// belongs to (ui.NavAttention, ui.NavAccount, ui.NavAdmin, or "" for none).
//
// The headers are writeAuthDocument's, because the forms on these pages need
// the same two things: no-store (a token shown once must not be restored from
// a cache), and same-origin referrers (the sign-out and every operator-write
// form are refused without a same-origin Origin).
//
// secret says the page shows or takes a secret (a minted token, a reset or
// invite link; an OAuth token or API key typed in; a passkey ceremony): it then
// loads no telemetry script (ui.NavDocumentView.Secret). Each caller's value is
// stated in TestEveryNavPageStatesItsTelemetry.
//
// app, when given, lays the page out as a phone app screen (ui.NavApp: the ask
// page a notification opens).
//
// opts lay the page out: a *ui.NavApp (a phone app screen), navWide (the wide
// column: a page of cards, the Asks page).
func (s *Server) writeNavDocument(w http.ResponseWriter, r *http.Request, code int, title, entry string, secret bool, inner string, opts ...any) {
	v := ui.NavDocumentView{Title: title, Entry: entry, Feat: s.shellFeatures(r.Context()), Main: inner, Secret: secret}
	for _, o := range opts {
		switch o := o.(type) {
		case *ui.NavApp:
			v.App = o
		case navLayout:
			v.Wide = o == navWide
		}
	}
	writeDocumentHeaders(w, code)
	if err := ui.RenderNavDocument(w, v); err != nil {
		s.logger.Printf("render %s: %v", title, err)
	}
}

// navLayout is a writeNavDocument option: navWide gives the page the wide
// column on a desktop (ui.NavDocumentView.Wide).
type navLayout int

const navWide navLayout = 1

// writeDocumentHeaders is the header set both document writers send, then the
// status.
func writeDocumentHeaders(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A login page must never be cached: a shared or restored cache entry would
	// show a stale form (or a stale error) to the next person at this origin.
	w.Header().Set("Cache-Control", "no-store")
	// 🔴 same-origin, set here as well as by securityHeaders (whose default
	// was no-referrer until the central origin check). Under no-referrer a browser sends `Origin: null` on a form POST
	// navigation, so the gotrue sign-in, claim and sign-out forms — which
	// refuse a POST whose Origin is not muster's (login CSRF) — refused
	// muster's OWN forms. Measured in Chromium by the e2e spec: the claim POST
	// carried `Origin: null` and got 403. same-origin still sends nothing to
	// any other site, which is what no-referrer protects (a token in a URL).
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(code)
}
