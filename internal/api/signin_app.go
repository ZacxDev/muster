package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/signinreq"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// --- Sign in with the muster app ----------------------------------------------
//
// A signed-out browser asks to sign in, shows a QR code, and a phone that is
// already signed in to muster approves it (docs/design/qr-sign-in.md;
// docs/auth.md#sign-in-with-the-muster-app). The store is internal/signinreq.
//
//   - POST /login/app          the sign-in page starts a request: a code for the
//     QR, and the BROWSER BINDING in an HttpOnly cookie (appSignInCookie).
//   - GET  /login/app/status   the page polls the request's state by its cookie.
//   - POST /login/app/redeem   the page spends an approval: the code AND the
//     cookie of the same request, then a session of the approver is issued.
//   - GET  /auth/qr            what the QR opens on the phone (the code in the
//     URL fragment, never sent to a server or a log); it posts the code here:
//   - POST /auth/qr            open (show the details), approve or deny — only
//     for a signed-in session on a KNOWN device.
//
// 🔴 THE PROPERTIES, AND THE TEST THAT PINS EACH (signin_app_pg_test.go):
//   - the code alone signs nothing in: a redeem needs the requesting browser's
//     binding cookie (TestAppSignInRedeemNeedsTheBrowserBinding,
//     TestAppSignInRedeemFromAnotherBrowserIsRefused);
//   - single use (TestAppSignInRedeemsOnce), two-minute expiry
//     (TestAppSignInExpires), a denial signs nothing in (TestAppSignInDeny);
//   - only a signed-in session on a known device may approve
//     (TestAppSignInUnknownDeviceCannotApprove), and a signed-out phone is sent
//     to sign in and back (TestAppSignInSignedOutPhoneSignsInAndComesBack);
//   - in gotrue mode, an approval dies with the approver's session or account
//     (TestAppSignInDisabledApproverSignsNothingIn);
//   - every door is throttled (TestAppSignInRateLimits) and refuses another
//     origin (TestAppSignInRefusesCrossOrigin).
//
// 🔴 A SESSION THIS FLOW ISSUES DOES NOT MARK ITS BROWSER A KNOWN DEVICE. A
// browser signed in by a phone's approval can therefore not approve another
// one until it signs in with a credential itself: an approval cannot mint an
// approver (TestAppSignInDoesNotMarkAKnownDevice).

// The routes. Literal patterns in registerAppSignInRoutes, for the reason
// registerLoginRoutes gives.
const (
	appSignInStartPath  = "/login/app"
	appSignInStatusPath = "/login/app/status"
	appSignInRedeemPath = "/login/app/redeem"
	// AppSignInPhonePath is the page the QR opens: <public>/auth/qr#<code>.
	AppSignInPhonePath = "/auth/qr"
)

// appSignInCookie holds the requesting browser's binding (its own secret,
// never shown or logged). A cookie base, so it is `__Host-`-prefixed where the
// others are (cookie_names.go).
const appSignInCookie = "muster_signin_app"

// appSignInPollInterval is the poll interval the page is told (RFC 8628's
// `interval`); a client polling faster than appSignInPollLimit a minute is
// answered 429 with a longer one (its `slow_down`).
const (
	appSignInPollInterval = 2 * time.Second
	appSignInSlowInterval = 10 * time.Second
	appSignInPollLimit    = 90
)

// The throttles. Every key is the client address (clientIP), plus one
// process-wide count for starting requests.
//
//   - starting a request: appSignInStartLimit a minute per address and
//     appSignInStartGlobal across all of them — each one is a row;
//   - the phone's look-ups of a code that is not live, and the browser's
//     failed redeems: appSignInMissLimit a minute per address, counted AFTER
//     the look-up, so a live code is never refused for its address (the
//     invite links' rule, login_invite.go). The codes are 256-bit random
//     values: this bounds noise, not a guess that could succeed.
const (
	appSignInStartLimit  = 10
	appSignInStartGlobal = 300
	appSignInMissLimit   = 10
	appSignInWindow      = time.Minute
)

// appSignInLimits is the throttle state (zero value ready).
type appSignInLimits struct {
	start  boundedWindow
	global boundedWindow
	poll   boundedWindow
	miss   boundedWindow
	// mint serialises GoTrue session issuance: the magic link it mints shares
	// GoTrue's one recovery-token slot per user, so two issuances for one user
	// at once would end each other's (gotrue.Client.AdminMagicLink).
	mint sync.Mutex
}

// The phone page's words, named so the tests pin them whole.
const (
	AppSignInUnknownDevice = "This phone is not one muster knows yet, so it cannot approve sign-ins. " +
		"Sign out on it, sign in again with your password or passkey, then scan the code again."
	AppSignInInvalid  = "This code is invalid, already used or expired. Start again on the other screen."
	AppSignInApproved = "Approved. The other browser is signing in now."
	AppSignInDenied   = "Denied. Nothing was signed in."
)

// appSignInServed reports whether the option is offered: a store, and a way
// to sign someone in (an operator password, or GoTrue).
func (s *Server) appSignInServed() bool {
	if s.ext.SignInRequests == nil {
		return false
	}
	if s.auth.goTrueMode() {
		return s.gt != nil
	}
	return s.auth.BrowserAuthRefusal() == ""
}

func (s *Server) registerAppSignInRoutes(mux Mux) {
	mux.HandleFunc("POST /login/app", s.handleAppSignInStart)
	mux.HandleFunc("GET /login/app/status", s.handleAppSignInStatus)
	mux.HandleFunc("POST /login/app/redeem", s.handleAppSignInRedeem)
	mux.HandleFunc("GET /auth/qr", s.handleAppSignInPhonePage)
	mux.HandleFunc("POST /auth/qr", s.handleAppSignInPhonePost)
}

// appSignInRefusal answers a request this server will not serve here, and
// reports whether it did: 404 when the option is off, the off-listener
// refusal in gotrue mode, and the same-origin refusal on a POST.
func (s *Server) appSignInRefusal(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case !s.appSignInServed():
		http.NotFound(w, r)
	case s.auth.goTrueMode() && !viaBrowserListener(r):
		s.refuseOffBrowserListener(w, r)
	case r.Method == http.MethodPost && !s.isSameOrigin(r):
		// Login CSRF: a cross-site page must not start, or spend, a sign-in in
		// this browser.
		s.refuseCrossOrigin(w, r)
	default:
		return false
	}
	return true
}

// appSignInCookieSecure is the Secure attribute of the binding cookie: always
// in gotrue mode (setGoTrueCookies' rule), the password-mode session's rule
// otherwise.
func (s *Server) appSignInCookieSecure(r *http.Request) bool {
	return s.auth.goTrueMode() || s.passwordCookieSecure(r)
}

func (s *Server) setAppSignInCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.auth.cookieName(appSignInCookie), Value: value, Path: "/",
		// Strict: only the sign-in page's own requests ever need it.
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.appSignInCookieSecure(r),
		MaxAge: maxAge,
	})
}

// appSignInBase is the origin the QR names: MUSTER_PUBLIC_URL when set, else
// the origin this request came to (a server with no public address, on a LAN
// address the phone can reach too).
func (s *Server) appSignInBase(r *http.Request) string {
	if p := strings.TrimRight(strings.TrimSpace(s.auth.PublicURL), "/"); p != "" {
		return p
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// AppSignInLink is the link a QR carries for code.
func AppSignInLink(base, code string) string {
	return strings.TrimRight(base, "/") + AppSignInPhonePath + "#" + code
}

// --- the requesting browser ---------------------------------------------------

func (s *Server) handleAppSignInStart(w http.ResponseWriter, r *http.Request) {
	if s.appSignInRefusal(w, r) {
		return
	}
	ip, now := clientIP(r), time.Now()
	if !s.appSignIn.start.take(ip, now, appSignInWindow, appSignInStartLimit) ||
		!s.appSignIn.global.take("", now, appSignInWindow, appSignInStartGlobal) {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many sign-in codes. Wait a minute and try again."})
		return
	}
	code, binding, expires, err := s.ext.SignInRequests.Create(r.Context(), ip, userAgentSummary(r.UserAgent()))
	if err != nil {
		s.logger.Printf("login: could not start a sign-in with the muster app: %v", err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Sign-in with the muster app is unavailable right now."})
		return
	}
	link := AppSignInLink(s.appSignInBase(r), code)
	svg, err := qrSVG(link)
	if err != nil {
		s.logger.Printf("login: could not draw the sign-in QR code: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "The sign-in code could not be drawn."})
		return
	}
	s.setAppSignInCookie(w, r, binding, int(signinreq.TTL/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"code": code, "url": link, "svg": svg,
		"expires_in": int(time.Until(expires).Round(time.Second) / time.Second),
		"interval":   int(appSignInPollInterval / time.Second),
	})
}

func (s *Server) handleAppSignInStatus(w http.ResponseWriter, r *http.Request) {
	if s.appSignInRefusal(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// RFC 8628's slow_down: a page polling faster than it was told is told to
	// wait longer, and is not answered.
	if !s.appSignIn.poll.take(clientIP(r), time.Now(), appSignInWindow, appSignInPollLimit) {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "slow_down",
			"interval": int(appSignInSlowInterval / time.Second)})
		return
	}
	req, err := s.ext.SignInRequests.Status(r.Context(), cookieValue(r, s.auth.cookieName(appSignInCookie)))
	switch {
	case errors.Is(err, signinreq.ErrNotFound):
		// No cookie, or a request swept away: the page starts again.
		s.writeJSON(w, http.StatusOK, map[string]any{"state": string(signinreq.StateExpired)})
		return
	case err != nil:
		s.logger.Printf("login: sign-in with the muster app status: %v", err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "unavailable",
			"interval": int(appSignInSlowInterval / time.Second)})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"state": string(req.State),
		"interval": int(appSignInPollInterval / time.Second)})
}

func (s *Server) handleAppSignInRedeem(w http.ResponseWriter, r *http.Request) {
	if s.appSignInRefusal(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "That request could not be read."})
		return
	}
	ip := clientIP(r)
	miss := func(code int, state signinreq.State, msg string) {
		if s.appSignIn.miss.count(ip, time.Now(), appSignInWindow) >= appSignInMissLimit {
			s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many attempts. Wait a minute and try again."})
			return
		}
		s.appSignIn.miss.add(ip, time.Now(), appSignInWindow)
		s.writeJSON(w, code, map[string]any{"state": string(state), "error": msg})
	}
	binding := cookieValue(r, s.auth.cookieName(appSignInCookie))
	req, err := s.ext.SignInRequests.Redeem(r.Context(), strings.TrimSpace(r.PostFormValue("code")), binding, s.auth.goTrueMode())
	switch {
	case errors.Is(err, signinreq.ErrNotApproved):
		s.writeJSON(w, http.StatusConflict, map[string]any{"state": string(signinreq.StatePending), "error": "Not approved yet."})
		return
	case errors.Is(err, signinreq.ErrDenied):
		s.writeJSON(w, http.StatusForbidden, map[string]any{"state": string(signinreq.StateDenied), "error": AppSignInDenied})
		return
	case errors.Is(err, signinreq.ErrApproverEnded):
		s.logger.Printf("login: refused a sign-in with the muster app: the approving session has ended")
		s.writeJSON(w, http.StatusForbidden, map[string]any{"state": string(signinreq.StateDenied),
			"error": "The phone that approved this has been signed out. Start again."})
		return
	case errors.Is(err, signinreq.ErrNotFound):
		miss(http.StatusGone, signinreq.StateExpired, "This code is no longer valid. Get a new one.")
		return
	case err != nil:
		s.logger.Printf("login: sign-in with the muster app redeem: %v", err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Sign-in is unavailable right now. Try again shortly."})
		return
	}
	// Spent: the cookie has done its job.
	s.setAppSignInCookie(w, r, "", -1)
	next := safeReturnTo(r.PostFormValue("next"))
	if !s.auth.goTrueMode() {
		// Password mode has one account, the operator's; the approver was
		// signed in as it.
		clearSiteCaches(w)
		s.setPasswordSessionCookie(w, r)
		s.logger.Printf("login: a browser session was issued (approved in the muster app, request %d)", req.ID)
		s.writeJSON(w, http.StatusOK, map[string]any{"redirect": next})
		return
	}
	sess, u, code, problem := s.issueGoTrueSessionFor(r.Context(), ip, req.ApprovedBy)
	if problem != "" {
		s.writeJSON(w, code, map[string]any{"error": problem})
		return
	}
	clearSiteCaches(w)
	s.setGoTrueCookies(w, sess)
	s.logger.Printf("login: user %d signed in (approved in the muster app, request %d)", u.ID, req.ID)
	s.writeJSON(w, http.StatusOK, map[string]any{"redirect": next})
}

// issueGoTrueSessionFor obtains a GoTrue session for muster user uid, who has
// already proved who they are (an approval in the muster app), WITHOUT their
// password: muster reads the email GoTrue holds for the user's login, mints a
// single-use magic-link token for it through GoTrue's admin API and exchanges
// it at /verify, all server-side (gotrue.Client.AdminMagicLink). The token
// never reaches a browser, an email or a log, and GoTrue issues the session
// itself, so it is an ordinary GoTrue session in every respect (refresh,
// sign-out, revocation; amr "otp").
//
// ⚠ GoTrue keeps one such token per user, shared with the password-reset
// link: a sign-in this way ends a reset link an admin handed the user and they
// have not used yet (measured on v2.197.0; docs/auth.md says so).
//
// 🔴 THE SESSION MUST BE FOR THE APPROVER, AND IS CHECKED TO BE: the link is
// minted by EMAIL, so the subject GoTrue signs is compared with the user's
// gotrue_sub, and a mismatch (an email that now names another GoTrue user)
// ends that session and signs nothing in. admitFreshSession then applies the
// sign-in's own checks (the token verifies, the account exists and is
// enabled).
func (s *Server) issueGoTrueSessionFor(ctx context.Context, ip string, uid int64) (gotrue.Session, users.User, int, string) {
	const unavailable = "Sign-in is unavailable right now. Try again shortly."
	u, err := s.gt.Users.ByID(ctx, uid)
	if err != nil {
		s.logger.Printf("login: sign-in with the muster app: user %d: %v", uid, err)
		return gotrue.Session{}, users.User{}, http.StatusServiceUnavailable, unavailable
	}
	if u.Disabled() || u.GotrueSub == "" {
		return gotrue.Session{}, users.User{}, http.StatusForbidden, "This account cannot sign in."
	}
	// The link is minted by email, so the email is GoTrue's own for the
	// user's login (gotrue_sub), not muster's copy of it.
	email, err := s.gt.Client.AdminUserEmail(ctx, ip, u.GotrueSub)
	if err != nil {
		s.logger.Printf("login: sign-in with the muster app: GoTrue could not read user %d's login: %v", u.ID, err)
		return gotrue.Session{}, users.User{}, http.StatusBadGateway, unavailable
	}
	s.appSignIn.mint.Lock()
	defer s.appSignIn.mint.Unlock()
	hashed, err := s.gt.Client.AdminMagicLink(ctx, ip, email)
	if err != nil {
		s.logger.Printf("login: sign-in with the muster app: GoTrue would not mint a link for user %d: %v", u.ID, err)
		return gotrue.Session{}, users.User{}, http.StatusBadGateway, unavailable
	}
	sess, err := s.gt.Client.VerifyMagicLink(ctx, ip, hashed)
	if err != nil {
		code := http.StatusBadGateway
		if gotrue.IsClientError(err) {
			code = http.StatusForbidden // a ban, most likely
		}
		s.logger.Printf("login: sign-in with the muster app: GoTrue would not issue a session for user %d: %v", u.ID, err)
		return gotrue.Session{}, users.User{}, code, unavailable
	}
	if c, err := s.gt.Verifier.Verify(sess.AccessToken); err == nil && c.Subject != u.GotrueSub {
		s.logger.Printf("login: sign-in with the muster app: GoTrue issued a session for another login than user %d's; ended it", u.ID)
		end, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = s.gt.Client.Logout(end, ip, sess.AccessToken, gotrue.ScopeLocal)
		cancel()
		return gotrue.Session{}, users.User{}, http.StatusForbidden, "This account cannot sign in."
	}
	admitted, problem, code := s.admitFreshSession(ctx, sess)
	if problem != "" {
		return gotrue.Session{}, users.User{}, code, problem
	}
	return sess, admitted, 0, ""
}

// --- the phone -----------------------------------------------------------------

// handleAppSignInPhonePage is what the QR opens. The code is in the URL
// fragment, which no request carries, so the page moves it into a form and
// posts it (fragmentForm); with no fragment (a QR read by an app that drops
// it, or a sign-in page in between) it asks for the link to be pasted.
func (s *Server) handleAppSignInPhonePage(w http.ResponseWriter, r *http.Request) {
	if s.appSignInRefusal(w, r) {
		return
	}
	writeAuthDocument(w, http.StatusOK, s.uiBrand(r.Context()), "Approve a sign-in",
		fragmentForm(AppSignInPhonePath, "Approve a sign-in", "Opening the sign-in request.", "Continue", true,
			`<input type="hidden" name="step" value="open">`, "", ""))
}

// handleAppSignInPhonePost is the phone's one POST: step=open shows the
// request, step=approve / step=deny decides it. A phone with no session is
// sent to sign in, carrying the code back (appSignInSignInPrompt); one with a
// session goes through requireSession and the same-origin check like every
// signed-in write.
func (s *Server) handleAppSignInPhonePost(w http.ResponseWriter, r *http.Request) {
	if s.appSignInRefusal(w, r) {
		return
	}
	if !s.looksSignedIn(r) {
		s.appSignInSignInPrompt(w, r)
		return
	}
	s.requireOperatorWrite(s.handleAppSignInDecide)(w, r)
}

// looksSignedIn reports whether r carries a session requireSession would
// resolve, without resolving it (no refresh is attempted): a valid password
// session, or a gotrue access token whose signature verifies, expired or not,
// with a refresh token beside an expired one. requireSession is still the
// judge; this only decides whether to ask the phone to sign in first.
func (s *Server) looksSignedIn(r *http.Request) bool {
	if !s.auth.goTrueMode() {
		return s.hasValidSession(r)
	}
	_, err := s.gt.Verifier.Verify(cookieValue(r, s.auth.cookieName(accessCookieName)))
	return err == nil || (errors.Is(err, gotrue.ErrExpired) && cookieValue(r, s.auth.cookieName(refreshCookieName)) != "")
}

// appSignInSignInPrompt asks a signed-out phone to sign in, with a link that
// comes back to the request: /login?next=/auth/qr#<code> (the sign-in returns
// to next, and a redirect keeps its fragment).
func (s *Server) appSignInSignInPrompt(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	back := AppSignInPhonePath
	if code := postedLinkToken(r); signinreq.CodeShapeOK(code) {
		back += "#" + code
	}
	href := loginPath + "?next=" + url.QueryEscape(back)
	inner := `<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Sign in to approve</h1>
<p class="text-sm text-fg2">Sign in to muster on this phone first. You will come back here to approve the sign-in.</p>
<a id="qr-signin-phone-login" href="` + html.EscapeString(href) + `" class="press inline-flex min-h-[44px] w-full items-center justify-center rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Sign in</a>
</section>`
	writeAuthDocument(w, http.StatusUnauthorized, s.uiBrand(r.Context()), "Sign in to approve", inner)
}

// handleAppSignInDecide runs behind requireSession and the same-origin check.
func (s *Server) handleAppSignInDecide(w http.ResponseWriter, r *http.Request) {
	p := authz.MustFrom(r.Context())
	page := func(code int, heading, body string) {
		s.writeNavDocument(w, r, code, heading, "", true, `<section id="qr-signin-phone" class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">`+html.EscapeString(heading)+`</h1>
<p class="text-sm text-fg2" id="qr-signin-phone-result" role="status">`+html.EscapeString(body)+`</p>
</section>`)
	}
	// 🔴 A KNOWN DEVICE ONLY: a browser that proved a credential here before
	// (known_device.go). A session alone — a copied cookie, or a browser this
	// very flow signed in — cannot approve.
	if !s.knownDeviceFor(r, p.UserID) {
		s.logger.Printf("login: user %d tried to approve a sign-in with the muster app from a browser that is not a known device", p.UserID)
		page(http.StatusForbidden, "This phone cannot approve sign-ins", AppSignInUnknownDevice)
		return
	}
	ip := clientIP(r)
	invalid := func() {
		if s.appSignIn.miss.count(ip, time.Now(), appSignInWindow) >= appSignInMissLimit {
			page(http.StatusTooManyRequests, "Too many attempts", "Wait a minute and try again.")
			return
		}
		s.appSignIn.miss.add(ip, time.Now(), appSignInWindow)
		page(http.StatusNotFound, "This code is invalid or expired", AppSignInInvalid)
	}
	code := postedLinkToken(r)
	switch step := r.PostFormValue("step"); step {
	case "open":
		req, err := s.ext.SignInRequests.Open(r.Context(), code)
		if errors.Is(err, signinreq.ErrNotFound) {
			invalid()
			return
		}
		if err != nil {
			s.logger.Printf("login: sign-in with the muster app open: %v", err)
			page(http.StatusServiceUnavailable, "Something went wrong", "Try scanning the code again.")
			return
		}
		s.writeNavDocument(w, r, http.StatusOK, "Approve a sign-in", "", true, appSignInDetailsHTML(req, code, ip))
	case "approve", "deny":
		// The approving session's sign-in time is recorded with the approval,
		// so ending that session (Sign out everywhere, a reset, a disable)
		// ends an approval not yet redeemed.
		signin, ok := stepup.AuthTime(r.Context())
		if !ok {
			page(http.StatusUnauthorized, "Sign in again", "This session records no sign-in time. Sign out, sign in again, then scan the code again.")
			return
		}
		approve := step == "approve"
		req, err := s.ext.SignInRequests.Decide(r.Context(), code, approve, users.Actor{UserID: p.UserID, Via: p.Via}, signin)
		if errors.Is(err, signinreq.ErrNotFound) {
			invalid()
			return
		}
		if err != nil {
			s.logger.Printf("login: sign-in with the muster app decide: %v", err)
			page(http.StatusServiceUnavailable, "Something went wrong", "Nothing was approved. Try scanning the code again.")
			return
		}
		if approve {
			s.logger.Printf("login: user %d approved a sign-in with the muster app (request %d) for a browser at %s", p.UserID, req.ID, req.RequesterIP)
			page(http.StatusOK, "Sign-in approved", AppSignInApproved)
			return
		}
		s.logger.Printf("login: user %d denied a sign-in with the muster app (request %d) for a browser at %s", p.UserID, req.ID, req.RequesterIP)
		page(http.StatusOK, "Sign-in denied", AppSignInDenied)
	default:
		invalid()
	}
}

// appSignInDetailsHTML is the approval card: what is asking, from where, and
// Approve / Deny. The phone's own address is shown beside the requester's, so
// a person can see when the two are on different networks.
func appSignInDetailsHTML(req signinreq.Request, code, phoneIP string) string {
	agent := req.RequesterAgent
	if agent == "" {
		agent = "Unknown browser"
	}
	row := func(id, label, value string) string {
		return `<dt class="text-fg2">` + label + `</dt><dd class="break-all font-semibold text-fg" id="` + id + `">` + html.EscapeString(value) + `</dd>`
	}
	form := func(step, id, label, cls string) string {
		return `<form method="POST" action="` + AppSignInPhonePath + `">
<input type="hidden" name="step" value="` + step + `">
<input type="hidden" name="token" value="` + html.EscapeString(code) + `">
<button type="submit" id="` + id + `" class="` + cls + `">` + label + `</button>
</form>`
	}
	left := int(time.Until(req.ExpiresAt).Round(time.Second) / time.Second)
	if left < 0 {
		left = 0
	}
	return `<section id="qr-signin-phone" class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Sign in on another device?</h1>
<p class="text-sm text-fg2">A browser is asking to sign in to muster as you. Approve only if you started this yourself, on a screen in front of you.</p>
<dl class="grid grid-cols-2 gap-x-3 gap-y-1 text-sm">
` + row("qr-signin-phone-agent", "Browser", agent) + `
` + row("qr-signin-phone-ip", "Its address", req.RequesterIP) + `
` + row("qr-signin-phone-own-ip", "This phone's address", phoneIP) + `
` + row("qr-signin-phone-expires", "Expires in", strconv.Itoa(left)+" s") + `
</dl>
<p class="text-sm text-fg2">If someone sent you this code or asked you to scan it, tap Deny.</p>
` + form("approve", "qr-signin-approve", "Approve", "press min-h-[44px] w-full rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90") + `
` + form("deny", "qr-signin-deny", "Deny", "press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge") + `
</section>`
}

// --- the known device in gotrue mode --------------------------------------------
//
// Password mode has the known-device cookie already (known_device.go): it is
// issued on every operator-password sign-in. gotrue mode issues the same
// cookie (muster_device) on every sign-in that proved a credential — a
// password, a passkey, a claim, an invite or a reset — signed with a key
// derived from MUSTER_GOTRUE_JWT_SECRET and BOUND TO THE USER, so it says
// "this browser signed in as this person before" and nothing about anyone
// else. Like the password-mode cookie it admits nothing on its own; it is
// read only by the app sign-in's approval (knownDeviceFor).

// gotrueDeviceSecret is user uid's device-cookie key in gotrue mode.
func (s *Server) gotrueDeviceSecret(uid int64) []byte {
	sum := sha256.Sum256([]byte("muster-ui-device-gotrue-v1\x00" + string(s.gt.Client.Secret) + "\x00" + strconv.FormatInt(uid, 10)))
	return sum[:]
}

// knownDeviceFor reports whether r comes from a known device for user uid.
func (s *Server) knownDeviceFor(r *http.Request, uid int64) bool {
	if !s.auth.goTrueMode() {
		return s.knownDevice(r)
	}
	if s.gt == nil || uid <= 0 {
		return false
	}
	ck, err := r.Cookie(s.auth.cookieName(knownDeviceCookieName))
	if err != nil || ck.Value == "" {
		return false
	}
	_, err = auth.VerifySession(s.gotrueDeviceSecret(uid), ck.Value, time.Now())
	return err == nil
}

// setGoTrueKnownDevice marks the browser that just signed in as uid with a
// credential. Secure always, like every gotrue-mode cookie.
func (s *Server) setGoTrueKnownDevice(w http.ResponseWriter, uid int64) {
	if s.gt == nil || uid <= 0 {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.auth.cookieName(knownDeviceCookieName),
		Value:    auth.SignSession(s.gotrueDeviceSecret(uid), time.Now(), knownDeviceTTL),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		MaxAge:   int(knownDeviceTTL / time.Second),
	})
}

// --- the requesting browser's User-Agent ------------------------------------------

// userAgentSummary is "<browser> on <system>" for the phone to show: enough
// to tell "my laptop's Firefox" from "a Chrome on Windows I do not own", and
// nothing more (the full string is not stored). Order matters: Edge and Opera
// also say Chrome, and Chrome also says Safari.
func userAgentSummary(ua string) string {
	has := func(s string) bool { return strings.Contains(ua, s) }
	browser := ""
	switch {
	case has("Edg/") || has("EdgA/") || has("EdgiOS/"):
		browser = "Edge"
	case has("OPR/") || has("Opera"):
		browser = "Opera"
	case has("SamsungBrowser/"):
		browser = "Samsung Internet"
	case has("Firefox/") || has("FxiOS/"):
		browser = "Firefox"
	case has("Chrome/") || has("CriOS/") || has("Chromium/"):
		browser = "Chrome"
	case has("Safari/"):
		browser = "Safari"
	}
	system := ""
	switch {
	case has("Windows"):
		system = "Windows"
	case has("iPhone") || has("iPad") || has("iPod"):
		system = "iOS"
	case has("Mac OS X") || has("Macintosh"):
		system = "macOS"
	case has("CrOS"):
		system = "ChromeOS"
	case has("Android"):
		system = "Android"
	case has("Linux"):
		system = "Linux"
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	case system != "":
		return "A browser on " + system
	}
	return ""
}
