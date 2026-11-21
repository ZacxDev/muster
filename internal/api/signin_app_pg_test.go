package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/signinreq"
)

// Sign in with the muster app, end to end over the real store, a gotrue-mode
// server on its browser listener (the fake GoTrue), and — once — a password
// server. Each test is a browser (jar) and a phone (jar).

// jar is one browser's cookies, by base name (the `__Host-` prefix dropped).
type jar map[string]string

// opt sends the jar's cookies under the names the server reads: prefixed when
// prefixed is set (every gotrue fixture here is https, so prefixed).
func (j jar) opt(prefixed bool) reqOpt {
	return func(r *http.Request) {
		for k, v := range j {
			name := k
			if prefixed {
				name = "__Host-" + k
			}
			r.AddCookie(&http.Cookie{Name: name, Value: v})
		}
	}
}

// take applies a response's Set-Cookies.
func (j jar) take(w *httptest.ResponseRecorder) {
	for _, c := range w.Result().Cookies() {
		base := strings.TrimPrefix(c.Name, "__Host-")
		if c.MaxAge < 0 || c.Value == "" {
			delete(j, base)
			continue
		}
		j[base] = c.Value
	}
}

type appRig struct {
	*deviceRig
	prefixed bool
	h        http.Handler // the handler browsers reach (the edge in gotrue mode)
}

func newAppRig(t *testing.T) *appRig {
	t.Helper()
	r := newDeviceRig(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ext.SignInRequests = signinreq.NewPG(pool)
	})
	t.Cleanup(func() {
		_, _ = r.pool.Exec(authz.System(context.Background()), `DELETE FROM signin_requests WHERE approved_by = $1`, r.uid)
	})
	return &appRig{deviceRig: r, prefixed: true, h: r.edge}
}

func (a *appRig) do(method, path string, form url.Values, j jar, opts ...reqOpt) *httptest.ResponseRecorder {
	w := do(a.h, method, path, form, append([]reqOpt{j.opt(a.prefixed)}, opts...)...)
	j.take(w)
	return w
}

// phone signs the rig's user in with the password, as a phone would: the jar
// then holds the session and the known-device cookie.
func (a *appRig) phone(t *testing.T) jar {
	t.Helper()
	u, _ := a.fake.User(a.subject())
	j := jar{}
	w := a.do("POST", "/login", url.Values{"email": {u.Email}, "password": {devOwnerPW}}, j)
	if w.Code != http.StatusSeeOther || j[accessCookieName] == "" || j[knownDeviceCookieName] == "" {
		t.Fatalf("phone sign-in: %d %v", w.Code, j)
	}
	return j
}

type started struct {
	Code      string `json:"code"`
	URL       string `json:"url"`
	SVG       string `json:"svg"`
	ExpiresIn int    `json:"expires_in"`
	Interval  int    `json:"interval"`
}

// start is the sign-in page's "Sign in with the muster app".
func (a *appRig) start(t *testing.T, desk jar, opts ...reqOpt) started {
	t.Helper()
	w := a.do("POST", appSignInStartPath, nil, desk, opts...)
	var s started
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &s) != nil || !signinreq.CodeShapeOK(s.Code) {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if desk[appSignInCookie] == "" {
		t.Fatalf("start set no binding cookie: %v", desk)
	}
	return s
}

func (a *appRig) status(t *testing.T, desk jar) string {
	t.Helper()
	w := a.do("GET", appSignInStatusPath, nil, desk)
	var s struct {
		State string `json:"state"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &s) != nil {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	return s.State
}

func (a *appRig) phonePost(ph jar, step, code string, opts ...reqOpt) *httptest.ResponseRecorder {
	return a.do("POST", AppSignInPhonePath, url.Values{"step": {step}, "token": {code}}, ph, opts...)
}

func (a *appRig) approve(t *testing.T, ph jar, code string) {
	t.Helper()
	if w := a.phonePost(ph, "open", code); w.Code != http.StatusOK {
		t.Fatalf("phone open: %d %s", w.Code, w.Body.String())
	}
	if w := a.phonePost(ph, "approve", code); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), AppSignInApproved) {
		t.Fatalf("phone approve: %d %s", w.Code, w.Body.String())
	}
}

func (a *appRig) redeem(desk jar, code string, opts ...reqOpt) *httptest.ResponseRecorder {
	return a.do("POST", appSignInRedeemPath, url.Values{"code": {code}, "next": {"/tasks"}}, desk, opts...)
}

func (a *appRig) expire(t *testing.T, code string) {
	t.Helper()
	sum := signinHash(code)
	if _, err := a.pool.Exec(authz.System(context.Background()),
		`UPDATE signin_requests SET expires_at = now() - interval '1 second' WHERE code_sha256 = $1`, sum); err != nil {
		t.Fatal(err)
	}
}

func signinHash(code string) []byte {
	sum := sha256.Sum256([]byte(code))
	return sum[:]
}

// sessionCookies reports whether a response set a session.
func sessionCookies(w *httptest.ResponseRecorder) bool {
	ck := setCookies(w)
	return ck[accessCookieName] != nil && ck[accessCookieName].Value != "" || ck[sessionCookieName] != nil && ck[sessionCookieName].Value != ""
}

// TestAppSignInApprovedOnAKnownPhoneSignsTheBrowserIn: the whole flow. The
// QR names <public>/auth/qr#<code>; the browser's status walks pending →
// scanned → approved; the redeem issues a session FOR THE APPROVER (the access
// token's subject is the phone user's) through ONE magic link; the browser
// then reads its account page as that user.
func TestAppSignInApprovedOnAKnownPhoneSignsTheBrowserIn(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk, withHeaders(map[string]string{"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"}))
	if s.URL != "https://muster.example.com/auth/qr#"+s.Code || !strings.HasPrefix(s.SVG, "<svg ") || s.Interval != 2 ||
		s.ExpiresIn < 110 || s.ExpiresIn > 120 {
		t.Fatalf("start answered %+v", s)
	}
	if st := a.status(t, desk); st != "pending" {
		t.Fatalf("status before the scan: %s", st)
	}
	// The page the QR opens: a bootstrap that posts the fragment.
	if w := a.do("GET", AppSignInPhonePath, nil, ph); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="step" value="open"`) {
		t.Fatalf("GET %s: %d", AppSignInPhonePath, w.Code)
	}
	w := a.phonePost(ph, "open", s.Code, fromIP("198.51.100.20"))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `id="qr-signin-phone-agent">Chrome on Windows<`) ||
		!strings.Contains(body, `id="qr-signin-approve"`) || !strings.Contains(body, `id="qr-signin-deny"`) ||
		!strings.Contains(body, `id="qr-signin-phone-own-ip">198.51.100.20<`) {
		t.Fatalf("phone open: %d %s", w.Code, body)
	}
	if st := a.status(t, desk); st != "scanned" {
		t.Fatalf("status after the scan: %s", st)
	}
	if w := a.phonePost(ph, "approve", s.Code); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), AppSignInApproved) {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if st := a.status(t, desk); st != "approved" {
		t.Fatalf("status after the approval: %s", st)
	}
	minted := a.fake.LinksMinted()
	w = a.redeem(desk, s.Code)
	var out struct {
		Redirect string `json:"redirect"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Redirect != "/tasks" {
		t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	if n := a.fake.LinksMinted() - minted; n != 1 {
		t.Fatalf("the redeem minted %d magic links; want 1", n)
	}
	c, err := a.fake.Verifier().Verify(desk[accessCookieName])
	if err != nil || c.Subject != a.subject() {
		t.Fatalf("the browser's session: %+v %v; want the approver %s", c, err, a.subject())
	}
	if desk[appSignInCookie] != "" {
		t.Fatalf("the binding cookie outlived the redeem")
	}
	if w := a.do("GET", "/account", nil, desk); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="account-who"`) {
		t.Fatalf("the signed-in browser's account page: %d", w.Code)
	}
}

// TestAppSignInDoesNotMarkAKnownDevice: a browser the app signed in gets a
// session and NO known-device cookie, so it cannot approve the next one.
func TestAppSignInDoesNotMarkAKnownDevice(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk)
	a.approve(t, ph, s.Code)
	w := a.redeem(desk, s.Code)
	if w.Code != http.StatusOK || desk[accessCookieName] == "" {
		t.Fatalf("redeem: %d", w.Code)
	}
	if _, ok := setCookies(w)[knownDeviceCookieName]; ok || desk[knownDeviceCookieName] != "" {
		t.Fatalf("the app sign-in set a known-device cookie")
	}
	other := jar{}
	s2 := a.start(t, other)
	if w := a.phonePost(desk, "open", s2.Code); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), AppSignInUnknownDevice) {
		t.Fatalf("a browser the app signed in approving another: %d %s", w.Code, w.Body.String())
	}
}

// TestAppSignInRedeemNeedsTheBrowserBinding: the code alone (no binding
// cookie) redeems nothing and sets no session; the browser that started it
// still can afterwards.
func TestAppSignInRedeemNeedsTheBrowserBinding(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk)
	a.approve(t, ph, s.Code)
	thief := jar{}
	w := a.redeem(thief, s.Code)
	if w.Code != http.StatusGone || sessionCookies(w) {
		t.Fatalf("redeem with the code and no binding: %d %s", w.Code, w.Body.String())
	}
	if w := a.redeem(desk, s.Code); w.Code != http.StatusOK || !sessionCookies(w) {
		t.Fatalf("the requesting browser's redeem after the refused one: %d %s", w.Code, w.Body.String())
	}
}

// TestAppSignInRedeemFromAnotherBrowserIsRefused: a second browser that
// started its own request (so holds a real binding) cannot redeem the first
// one's approved code with it.
func TestAppSignInRedeemFromAnotherBrowserIsRefused(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	desk, other := jar{}, jar{}
	s := a.start(t, desk)
	a.start(t, other)
	a.approve(t, ph, s.Code)
	if w := a.redeem(other, s.Code); w.Code != http.StatusGone || sessionCookies(w) {
		t.Fatalf("another browser's redeem: %d %s", w.Code, w.Body.String())
	}
	if w := a.redeem(desk, s.Code); w.Code != http.StatusOK {
		t.Fatalf("the requesting browser's redeem: %d", w.Code)
	}
}

// TestAppSignInRedeemsOnce: the same browser replaying its binding after a
// successful redeem gets nothing.
func TestAppSignInRedeemsOnce(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk)
	binding := desk[appSignInCookie]
	a.approve(t, ph, s.Code)
	if w := a.redeem(desk, s.Code); w.Code != http.StatusOK {
		t.Fatalf("first redeem: %d", w.Code)
	}
	replay := jar{appSignInCookie: binding}
	if w := a.redeem(replay, s.Code); w.Code != http.StatusGone || sessionCookies(w) {
		t.Fatalf("second redeem: %d %s", w.Code, w.Body.String())
	}
}

// TestAppSignInExpires: past its expiry a request cannot be opened on the
// phone, its status says expired, and an approval that expired before its
// redeem signs nothing in.
func TestAppSignInExpires(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk)
	a.expire(t, s.Code)
	if st := a.status(t, desk); st != "expired" {
		t.Fatalf("status: %s", st)
	}
	if w := a.phonePost(ph, "open", s.Code); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), AppSignInInvalid) {
		t.Fatalf("open an expired code: %d", w.Code)
	}
	desk2 := jar{}
	s2 := a.start(t, desk2)
	a.approve(t, ph, s2.Code)
	a.expire(t, s2.Code)
	if w := a.redeem(desk2, s2.Code); w.Code != http.StatusGone || sessionCookies(w) {
		t.Fatalf("redeem an approval past its expiry: %d %s", w.Code, w.Body.String())
	}
}

// TestAppSignInDeny: a denial is shown as one on both sides and redeems
// nothing.
func TestAppSignInDeny(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk)
	if w := a.phonePost(ph, "open", s.Code); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	if w := a.phonePost(ph, "deny", s.Code); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), AppSignInDenied) {
		t.Fatalf("deny: %d", w.Code)
	}
	if st := a.status(t, desk); st != "denied" {
		t.Fatalf("status: %s", st)
	}
	if w := a.redeem(desk, s.Code); w.Code != http.StatusForbidden || sessionCookies(w) {
		t.Fatalf("redeem a denial: %d %s", w.Code, w.Body.String())
	}
	if w := a.phonePost(ph, "approve", s.Code); w.Code != http.StatusNotFound {
		t.Fatalf("approve after the denial: %d", w.Code)
	}
}

// TestAppSignInUnknownDeviceCannotApprove: a valid session with no
// known-device cookie, or with another user's, or a forged one, cannot open
// or approve; the request stays pending.
func TestAppSignInUnknownDeviceCannotApprove(t *testing.T) {
	a := newAppRig(t)
	desk := jar{}
	s := a.start(t, desk)
	at, rt, _ := a.fake.Mint(a.subject())
	phone := a.phone(t)
	cases := map[string]jar{
		"no device cookie":              {accessCookieName: at, refreshCookieName: rt},
		"another user's":                {accessCookieName: at, refreshCookieName: rt, knownDeviceCookieName: a.s.otherUsersDevice(a.uid + 1000)},
		"a password-mode cookie":        {accessCookieName: at, refreshCookieName: rt, knownDeviceCookieName: phone[knownDeviceCookieName] + "x"},
		"the device cookie, no session": {knownDeviceCookieName: phone[knownDeviceCookieName]},
	}
	for name, j := range cases {
		for _, step := range []string{"open", "approve"} {
			w := a.phonePost(j, step, s.Code)
			if w.Code == http.StatusOK {
				t.Errorf("%s, %s: 200", name, step)
			}
			if name != "the device cookie, no session" && !strings.Contains(w.Body.String(), AppSignInUnknownDevice) {
				t.Errorf("%s, %s: %d without the unknown-device refusal", name, step, w.Code)
			}
		}
	}
	if st := a.status(t, desk); st != "pending" {
		t.Fatalf("status after the refusals: %s", st)
	}
	// The control: the same session WITH its own device cookie approves.
	a.approve(t, phone, s.Code)
}

// otherUsersDevice is a gotrue-mode device cookie for user uid.
func (s *Server) otherUsersDevice(uid int64) string {
	w := httptest.NewRecorder()
	s.setGoTrueKnownDevice(w, uid)
	return w.Result().Cookies()[0].Value
}

// TestAppSignInSignedOutPhoneSignsInAndComesBack: a phone with no session is
// asked to sign in with a link back to the request; signing in follows it
// (the fragment kept); the request then opens.
func TestAppSignInSignedOutPhoneSignsInAndComesBack(t *testing.T) {
	a := newAppRig(t)
	desk := jar{}
	s := a.start(t, desk)
	w := a.phonePost(jar{}, "open", s.Code)
	back := "/auth/qr#" + s.Code
	wantHref := `href="/login?next=` + url.QueryEscape(back) + `"`
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), wantHref) {
		t.Fatalf("signed-out phone: %d %s", w.Code, w.Body.String())
	}
	ph := jar{}
	u, _ := a.fake.User(a.subject())
	w = a.do("POST", "/login", url.Values{"email": {u.Email}, "password": {devOwnerPW}, "next": {back}}, ph)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != back {
		t.Fatalf("sign-in with next: %d Location %q; want %q", w.Code, w.Header().Get("Location"), back)
	}
	if w := a.phonePost(ph, "open", s.Code); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="qr-signin-approve"`) {
		t.Fatalf("open after signing in: %d", w.Code)
	}
}

// TestAppSignInDisabledApproverSignsNothingIn: an approval whose approver is
// disabled before the redeem issues no session and mints no magic link.
func TestAppSignInDisabledApproverSignsNothingIn(t *testing.T) {
	a := newAppRig(t)
	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk)
	a.approve(t, ph, s.Code)
	if _, err := a.pool.Exec(authz.System(context.Background()), `UPDATE users SET disabled_at = now() WHERE id = $1`, a.uid); err != nil {
		t.Fatal(err)
	}
	minted := a.fake.LinksMinted()
	if w := a.redeem(desk, s.Code); w.Code != http.StatusForbidden || sessionCookies(w) {
		t.Fatalf("redeem after the approver was disabled: %d %s", w.Code, w.Body.String())
	}
	if a.fake.LinksMinted() != minted {
		t.Fatal("a magic link was minted for a disabled approver")
	}
}

// TestAppSignInRateLimits: starting is limited per address (another address
// is unaffected); the phone's look-ups of dead codes are limited per address
// while a live code still opens; failed redeems likewise; and a fast poller
// is told to slow down.
func TestAppSignInRateLimits(t *testing.T) {
	a := newAppRig(t)
	for i := 0; i < appSignInStartLimit; i++ {
		if w := a.do("POST", appSignInStartPath, nil, jar{}, fromIP("203.0.113.1")); w.Code != http.StatusOK {
			t.Fatalf("start %d: %d", i, w.Code)
		}
	}
	if w := a.do("POST", appSignInStartPath, nil, jar{}, fromIP("203.0.113.1")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("start past the limit: %d", w.Code)
	}
	if w := a.do("POST", appSignInStartPath, nil, jar{}, fromIP("203.0.113.2")); w.Code != http.StatusOK {
		t.Fatalf("another address: %d", w.Code)
	}

	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk, fromIP("203.0.113.3"))
	dead := signinreq.CodePrefix + strings.Repeat("A", 43)
	for i := 0; i < appSignInMissLimit; i++ {
		if w := a.phonePost(ph, "open", dead, fromIP("203.0.113.4")); w.Code != http.StatusNotFound {
			t.Fatalf("miss %d: %d", i, w.Code)
		}
	}
	if w := a.phonePost(ph, "open", dead, fromIP("203.0.113.4")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("miss past the limit: %d", w.Code)
	}
	if w := a.phonePost(ph, "open", s.Code, fromIP("203.0.113.4")); w.Code != http.StatusOK {
		t.Fatalf("a live code from the throttled address: %d", w.Code)
	}

	for i := 0; i < appSignInMissLimit; i++ {
		if w := a.redeem(jar{}, dead, fromIP("203.0.113.5")); w.Code != http.StatusGone {
			t.Fatalf("redeem miss %d: %d", i, w.Code)
		}
	}
	if w := a.redeem(jar{}, dead, fromIP("203.0.113.5")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("redeem miss past the limit: %d", w.Code)
	}

	for i := 0; i < appSignInPollLimit; i++ {
		if w := a.do("GET", appSignInStatusPath, nil, desk, fromIP("203.0.113.6")); w.Code != http.StatusOK {
			t.Fatalf("poll %d: %d", i, w.Code)
		}
	}
	w := a.do("GET", appSignInStatusPath, nil, desk, fromIP("203.0.113.6"))
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"slow_down"`) {
		t.Fatalf("poll past the limit: %d %s", w.Code, w.Body.String())
	}
}

// TestAppSignInRefusesCrossOrigin: starting, redeeming and approving from
// another origin are refused, and change nothing; on the machine listener the
// routes are not served.
func TestAppSignInRefusesCrossOrigin(t *testing.T) {
	a := newAppRig(t)
	evil := withHeaders(map[string]string{"Origin": "https://evil.example"})
	if w := a.do("POST", appSignInStartPath, nil, jar{}, evil); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin start: %d", w.Code)
	}
	ph := a.phone(t)
	desk := jar{}
	s := a.start(t, desk)
	if w := a.phonePost(ph, "open", s.Code); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	if w := a.phonePost(ph, "approve", s.Code, evil); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin approve: %d", w.Code)
	}
	if st := a.status(t, desk); st != "scanned" {
		t.Fatalf("status after the refused approve: %s", st)
	}
	a.approve(t, ph, s.Code)
	if w := a.redeem(desk, s.Code, evil); w.Code != http.StatusForbidden || sessionCookies(w) {
		t.Fatalf("cross-origin redeem: %d", w.Code)
	}
	if w := do(a.mach, "POST", appSignInStartPath, nil); w.Code != http.StatusForbidden {
		t.Fatalf("start on the machine listener: %d", w.Code)
	}
	if w := a.redeem(desk, s.Code); w.Code != http.StatusOK {
		t.Fatalf("the same-origin redeem afterwards: %d", w.Code)
	}
}

// TestAppSignInPasswordMode: on a password server the flow signs the browser
// in with the operator session; a signed-in browser with no device cookie
// cannot approve; and the sign-in page offers the option only with the store.
func TestAppSignInPasswordMode(t *testing.T) {
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
	const pw = "app-sign-in-operator-password"
	s := New(nil, AuthConfig{UIPassword: pw, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	if w := do(s.Handler(), "GET", "/login", nil); strings.Contains(w.Body.String(), `id="qr-signin-start"`) {
		t.Fatal("the option is offered with no store")
	}
	s.UseExtensions(Extensions{SignInRequests: signinreq.NewPG(pool)})
	a := &appRig{h: s.Handler()}
	if w := a.do("GET", "/login", nil, jar{}); !strings.Contains(w.Body.String(), `id="qr-signin-start"`) {
		t.Fatal("the option is not offered with the store")
	}
	ph := jar{}
	if w := a.do("POST", "/login", url.Values{"password": {pw}}, ph); w.Code != http.StatusSeeOther || ph[knownDeviceCookieName] == "" {
		t.Fatalf("phone sign-in: %d %v", w.Code, ph)
	}
	desk := jar{}
	st := a.start(t, desk)
	if !strings.HasPrefix(st.URL, "http://example.com/auth/qr#") {
		t.Fatalf("with no public URL the QR names the request's origin: %s", st.URL)
	}
	noDevice := jar{sessionCookieName: ph[sessionCookieName]}
	if w := a.phonePost(noDevice, "open", st.Code); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), AppSignInUnknownDevice) {
		t.Fatalf("a session with no device cookie: %d", w.Code)
	}
	a.approve(t, ph, st.Code)
	w := a.redeem(desk, st.Code)
	if w.Code != http.StatusOK || desk[sessionCookieName] == "" || desk[knownDeviceCookieName] != "" {
		t.Fatalf("redeem: %d %v", w.Code, desk)
	}
	if w := a.do("GET", "/login", nil, desk); w.Code != http.StatusSeeOther {
		t.Fatalf("the redeemed session is not a session: GET /login %d", w.Code)
	}
	_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM signin_requests WHERE approved_by = 1`)
}

// TestUserAgentSummary pins the summaries for the common browsers, including
// the ones that also say "Chrome" or "Safari".
func TestUserAgentSummary(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36":                         "Chrome on Windows",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0":           "Edge on Windows",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15":                      "Safari on macOS",
		"Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0":                                                                  "Firefox on Linux",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1": "Safari on iOS",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36 OPR/85.0":                       "Opera on Android",
		"Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36":                              "Chrome on ChromeOS",
		"curl/8.5.0": "",
		"":           "",
	} {
		if got := userAgentSummary(ua); got != want {
			t.Errorf("userAgentSummary(%q) = %q; want %q", ua, got, want)
		}
	}
}

// TestTheAppSignInCodeNeverReachesTheLog: a hand-made /auth/qr/<code> path is
// cut before the access log writes it (the code rides in the fragment, which
// no request carries).
func TestTheAppSignInCodeNeverReachesTheLog(t *testing.T) {
	code := signinreq.CodePrefix + strings.Repeat("x", 43)
	if got := accessLogPath(&url.URL{Path: "/auth/qr/" + code}); strings.Contains(got, code) || got != "/auth/qr/[redacted]" {
		t.Fatalf("logged %q", got)
	}
	if got := accessLogPath(&url.URL{Path: "/auth/qr"}); got != "/auth/qr" {
		t.Fatalf("the bare page logs as %q", got)
	}
}
