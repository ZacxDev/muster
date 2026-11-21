package api

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/signinreq"
	"github.com/ZacxDev/muster/internal/stepup"
)

// The audit round's properties of sign in with the muster app. Each test here
// was RED on the first version of the feature.

// stalePhone signs the rig's user in on a phone whose sign-in is older than
// the step-up window: the fake's clock is wound back for the sign-in and
// forward again, so the session's amr (its sign-in time) is stale while the
// session itself is valid (refreshed on its next request).
func (a *appRig) stalePhone(t *testing.T) jar {
	t.Helper()
	back := stepup.Window() + time.Hour
	a.fake.Advance(-back)
	ph := a.phone(t)
	a.fake.Advance(back)
	return ph
}

// TestAppSignInApprovalNeedsAFreshSignIn (gotrue): a known device whose
// sign-in is past the step-up window can open a request and deny it, but not
// approve it — the 403 carries the step-up header, the page offers "Confirm
// it's you", and the request stays undecided. Without this, a stale browser
// could approve a second browser (or a new tab) and launder itself into a
// session that is fresh for the whole window. The control: a fresh phone
// approves the same kind of request.
func TestAppSignInApprovalNeedsAFreshSignIn(t *testing.T) {
	a := newAppRig(t)
	ph := a.stalePhone(t)
	desk := jar{}
	s := a.start(t, desk)
	if w := a.phonePost(ph, "open", s.Code); w.Code != http.StatusOK {
		t.Fatalf("a stale phone opening the request: %d", w.Code)
	}
	w := a.phonePost(ph, "approve", s.Code)
	if w.Code != http.StatusForbidden || w.Header().Get("X-Muster-Step-Up") != "required" ||
		!strings.Contains(w.Body.String(), html.EscapeString(AppSignInStale)) || !strings.Contains(w.Body.String(), `data-step-up-form`) {
		t.Fatalf("a stale phone approving: %d step-up=%q", w.Code, w.Header().Get("X-Muster-Step-Up"))
	}
	if st := a.status(t, desk); st != "scanned" {
		t.Fatalf("status after the refused approval: %s", st)
	}
	if w := a.redeem(desk, s.Code); w.Code == http.StatusOK || sessionCookies(w) {
		t.Fatalf("the desktop redeemed after a stale approval: %d", w.Code)
	}
	// Denying needs no fresh sign-in.
	if w := a.phonePost(ph, "deny", s.Code); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), AppSignInDenied) {
		t.Fatalf("a stale phone denying: %d", w.Code)
	}
	fresh := a.phone(t)
	desk2 := jar{}
	s2 := a.start(t, desk2)
	a.approve(t, fresh, s2.Code)
}

// TestAppSignInApprovalNeedsAFreshSignInPasswordMode: the same gate on a
// password server, where the sign-in time is the session cookie's issue time.
func TestAppSignInApprovalNeedsAFreshSignInPasswordMode(t *testing.T) {
	a, s := newPasswordAppRig(t)
	ph := jar{}
	if w := a.do("POST", "/login", url.Values{"password": {passwordAppPW}}, ph); w.Code != http.StatusSeeOther {
		t.Fatalf("phone sign-in: %d", w.Code)
	}
	ph[sessionCookieName] = auth.SignSession(s.auth.uiSessionSecret(), time.Now().Add(-(stepup.Window() + time.Hour)), sessionTTL)
	desk := jar{}
	st := a.start(t, desk)
	if w := a.phonePost(ph, "open", st.Code); w.Code != http.StatusOK {
		t.Fatalf("open: %d", w.Code)
	}
	if w := a.phonePost(ph, "approve", st.Code); w.Code != http.StatusForbidden || w.Header().Get("X-Muster-Step-Up") != "required" {
		t.Fatalf("a stale password session approving: %d", w.Code)
	}
	if w := a.redeem(desk, st.Code); w.Code == http.StatusOK {
		t.Fatalf("redeemed after a stale approval")
	}
}

// TestAppSignInTheRequestingBrowserCannotApproveItself: a signed-in, fresh,
// known browser that started a request (it holds that request's binding
// cookie) cannot approve it — approving your own browser would only refresh
// your own sign-in. The control: the same browser approves ANOTHER browser's.
func TestAppSignInTheRequestingBrowserCannotApproveItself(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	own := a.start(t, ph)
	if w := a.phonePost(ph, "open", own.Code); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), AppSignInSameBrowser) {
		t.Fatalf("opening its own request: %d", w.Code)
	}
	if w := a.phonePost(ph, "approve", own.Code); w.Code != http.StatusForbidden {
		t.Fatalf("approving its own request: %d", w.Code)
	}
	if st := a.status(t, ph); st == "approved" {
		t.Fatal("a browser approved its own sign-in")
	}
	desk := jar{}
	other := a.start(t, desk)
	a.approve(t, ph, other.Code)
}

// TestAppSignInTheCodeNeverRidesInAURL: the signed-out phone's page links to
// the sign-in with NO code in it (the query string is logged by proxies and
// kept in history); the code is in the page body only, which its script keeps
// in sessionStorage across the sign-in.
func TestAppSignInTheCodeNeverRidesInAURL(t *testing.T) {
	a := newAppRig(t)
	s := a.start(t, jar{})
	w := a.phonePost(jar{}, "open", s.Code)
	body := w.Body.String()
	if w.Code != http.StatusUnauthorized || !strings.Contains(body, `href="/login?next=`+url.QueryEscape(AppSignInPhonePath)+`"`) {
		t.Fatalf("signed-out prompt: %d %s", w.Code, body)
	}
	for _, m := range regexp.MustCompile(`(?:href|action|src)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[1], s.Code) || strings.Contains(m[1], url.QueryEscape(s.Code)) {
			t.Fatalf("a URL on the page carries the code: %s", m[1])
		}
	}
	if !strings.Contains(body, `data-qr-code="`+s.Code+`"`) {
		t.Fatal("the page does not hand the code to its script")
	}
}

// TestAppSignInTheDesktopIsToldWhoApproved: once approved, the requesting
// browser's status names the account it would be signed in as (before it
// redeems; the page asks "Continue as …"), and before that it names nobody.
func TestAppSignInTheDesktopIsToldWhoApproved(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk)
	approver := func() (string, map[string]any) {
		w := a.do("GET", appSignInStatusPath, nil, desk)
		var out struct {
			State    string         `json:"state"`
			Approver map[string]any `json:"approver"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.State, out.Approver
	}
	if st, who := approver(); st != "pending" || who != nil {
		t.Fatalf("pending: %s %v", st, who)
	}
	a.approve(t, ph, s.Code)
	var email string
	if err := a.pool.QueryRow(authz.System(context.Background()), `SELECT email FROM users WHERE id = $1`, a.uid).Scan(&email); err != nil {
		t.Fatal(err)
	}
	st, who := approver()
	if st != "approved" || who == nil || who["email"] != email {
		t.Fatalf("approved: %s %v; want the approver %s", st, who, email)
	}
}

// TestAppSignInKnownDeviceIsRevocable (gotrue): the known-device cookie stops
// vouching after each event that ends the user's sessions — this browser's
// sign-out (which also clears it), Sign out everywhere, a password reset's
// session cutoff, and a disable. The control: before any of them it vouches.
func TestAppSignInKnownDeviceIsRevocable(t *testing.T) {
	a := newAppRig(t)
	sys := authz.System(context.Background())
	known := func(ph jar) bool {
		r := httptest.NewRequest("GET", "/", nil)
		for k, v := range ph {
			r.AddCookie(&http.Cookie{Name: "__Host-" + k, Value: v})
		}
		return a.s.knownDeviceFor(r, a.uid)
	}
	reset := func() {
		if _, err := a.pool.Exec(sys, `UPDATE users SET sessions_not_before = NULL, signed_out_at = NULL, disabled_at = NULL WHERE id = $1`, a.uid); err != nil {
			t.Fatal(err)
		}
		a.s.gt.forgetUser(a.uid)
	}
	// Sign out on this browser: the response clears the cookie.
	ph := a.phone(t)
	if !known(ph) {
		t.Fatal("control: a fresh sign-in's device cookie does not vouch")
	}
	w := a.do("POST", "/logout", nil, ph)
	if w.Code != http.StatusSeeOther || ph[knownDeviceCookieName] != "" {
		t.Fatalf("sign-out: %d, device cookie still %q", w.Code, ph[knownDeviceCookieName])
	}
	for name, q := range map[string]string{
		"sign out everywhere": `UPDATE users SET signed_out_at = now() + interval '1 second' WHERE id = $1`,
		"a reset's cutoff":    `UPDATE users SET sessions_not_before = now() + interval '1 second' WHERE id = $1`,
		"a disable":           `UPDATE users SET disabled_at = now() WHERE id = $1`,
	} {
		reset()
		ph := a.phone(t)
		if !known(ph) {
			t.Fatalf("%s: control: the device cookie does not vouch before the event", name)
		}
		if _, err := a.pool.Exec(sys, q, a.uid); err != nil {
			t.Fatal(err)
		}
		a.s.gt.forgetUser(a.uid)
		if known(ph) {
			t.Errorf("%s: the old device cookie still vouches", name)
		}
	}
	reset()
}

// TestAppSignInARevocationDuringTheMintEndsTheNewSession (gotrue): a
// revocation that lands after the redeem committed but before GoTrue issued
// the session (the credentials lock is already released then) must not leave
// that session standing: the redeem re-reads the account, ends the session at
// GoTrue and sets no cookie. One case per arm of the re-check — a Sign out
// everywhere, a reset's session cutoff, a disable — each landing ONLY its own
// column, so removing any one arm fails its own case. The control is every
// other redeem in this file.
func TestAppSignInARevocationDuringTheMintEndsTheNewSession(t *testing.T) {
	for _, c := range []struct{ name, q string }{
		{"a sign-out everywhere", `UPDATE users SET signed_out_at = now() + interval '1 second' WHERE id = $1`},
		{"a reset's cutoff", `UPDATE users SET sessions_not_before = now() + interval '1 second' WHERE id = $1`},
		{"a disable", `UPDATE users SET disabled_at = now() WHERE id = $1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAppRig(t)
			ph := a.phone(t)
			desk := jar{}
			s := a.start(t, desk)
			a.approve(t, ph, s.Code)
			a.s.appSignInAfterMint = func() {
				if _, err := a.pool.Exec(authz.System(context.Background()), c.q, a.uid); err != nil {
					t.Error(err)
				}
				a.s.gt.forgetUser(a.uid)
			}
			logouts := a.fake.CountCalls("POST", "/logout")
			w := a.redeem(desk, s.Code)
			if w.Code != http.StatusForbidden || sessionCookies(w) {
				t.Fatalf("redeem with %s during the mint: %d %s", c.name, w.Code, w.Body.String())
			}
			if a.fake.CountCalls("POST", "/logout") != logouts+1 {
				t.Fatalf("%s during the mint: the session GoTrue issued was not ended", c.name)
			}
		})
	}
}

// TestAppSignInForgedBindingsArePolledUnderTheAddress: a poll is counted under
// its binding only when that binding names a request; a well-shaped cookie
// naming none (a fresh random one on every poll) is counted under the client
// address, so it cannot buy itself an unlimited budget. A real request's own
// polls, and a second real desktop's, from that same address are unaffected.
func TestAppSignInForgedBindingsArePolledUnderTheAddress(t *testing.T) {
	a := newAppRig(t)
	desk, other := jar{}, jar{}
	a.start(t, desk, fromIP("203.0.113.40"))
	a.start(t, other, fromIP("203.0.113.40"))
	refused := 0
	for i := 0; i < appSignInPollLimit+10; i++ {
		forged, err := auth.RandomToken(32)
		if err != nil {
			t.Fatal(err)
		}
		if !signinreq.BindingShapeOK(forged) {
			t.Fatalf("the forged binding %q is not well shaped: the test would not reach the guard", forged)
		}
		w := a.do("GET", appSignInStatusPath, nil, jar{appSignInCookie: forged}, fromIP("203.0.113.40"))
		if w.Code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused < 10 {
		t.Fatalf("forged bindings from one address: %d of %d polls refused; want the %d past the address's limit",
			refused, appSignInPollLimit+10, 10)
	}
	for name, j := range map[string]jar{"the real request": desk, "a second real desktop": other} {
		if w := a.do("GET", appSignInStatusPath, nil, j, fromIP("203.0.113.40")); w.Code != http.StatusOK {
			t.Errorf("%s polling from the same address: %d", name, w.Code)
		}
	}
}

const passwordAppPW = "app-sign-in-operator-password"

// newPasswordAppRig is a password-mode server with the sign-in store.
func newPasswordAppRig(t *testing.T) (*appRig, *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM signin_requests WHERE approved_by = 1`)
	})
	s := New(nil, AuthConfig{UIPassword: passwordAppPW, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{SignInRequests: signinreq.NewPG(pool)})
	return &appRig{h: s.Handler()}, s
}
