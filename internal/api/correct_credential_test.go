package api

// A per-address (or process-wide) attempt limit must never refuse a request
// that carries the CORRECT credential or code: it counts failures, and the
// credential is checked before any refusal. Every test below floods junk from
// the SAME address first — a shared proxy, NAT or office address, or simply the
// address of the TCP peer in front of muster when no proxy is trusted — and
// then presents the right credential from that address.

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/clientaddr"
	"github.com/ZacxDev/muster/internal/devices"
)

// sharedAddress is every client's address behind an untrusted proxy.
const sharedAddress = "192.0.2.5"

// passwordLogin posts the operator-password form with opts (a peer, cookies).
func passwordLogin(h http.Handler, pw string, opts ...reqOpt) *httptest.ResponseRecorder {
	return do(h, http.MethodPost, "/login", url.Values{"password": {pw}, "next": {"/"}}, opts...)
}

// withDevice presents a known-device cookie value.
func withDevice(v string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: knownDeviceCookieName, Value: v}) }
}

// knownDeviceOf signs in correctly on h and returns the device cookie issued.
func knownDeviceOf(t *testing.T, h http.Handler) string {
	t.Helper()
	w := passwordLogin(h, testUIPassword, arriveFrom("198.51.100.7"))
	ck := setCookies(w)[knownDeviceCookieName]
	if w.Code != http.StatusSeeOther || ck == nil || ck.Value == "" || !ck.HttpOnly {
		t.Fatalf("CONTROL: a correct sign-in did not issue an HttpOnly device cookie: %d %v", w.Code, ck)
	}
	return ck.Value
}

// flood sends n wrong operator passwords from the shared address.
func flood(h http.Handler, n int) {
	for i := 0; i < n; i++ {
		passwordLogin(h, "not-the-password-at-all", arriveFrom(sharedAddress))
	}
}

// TestAJunkFloodDoesNotLockAKnownDeviceOut: password mode, the known-device
// rule. Past loginRateBurst failures, the correct password from a browser with
// no valid device cookie is REFUSED (a guesser is held to the burst), and from a
// known device it is ADMITTED (the flood cannot lock the operator out).
func TestAJunkFloodDoesNotLockAKnownDeviceOut(t *testing.T) {
	_, h := pwaServer(t, nil)
	dev := knownDeviceOf(t, h)
	flood(h, loginRateBurst)
	if c := passwordLogin(h, testUIPassword, arriveFrom(sharedAddress)).Code; c != http.StatusTooManyRequests {
		t.Errorf("an UNKNOWN device with the correct password past the failure count: %d, want 429", c)
	}
	if c := passwordLogin(h, testUIPassword, arriveFrom(sharedAddress), withDevice(dev)).Code; c != http.StatusSeeOther {
		t.Errorf("a KNOWN device with the correct password past the failure count: %d, want 303", c)
	}
	if c := passwordLogin(h, "still-not-the-password", arriveFrom(sharedAddress), withDevice(dev)).Code; c != http.StatusTooManyRequests {
		t.Errorf("a known device with a WRONG password past the count: %d, want 429", c)
	}
}

// TestAnInvalidDeviceCookieIsAnUnknownDevice: a forged value, an expired one,
// one signed under another key, and a session cookie's value presented as a
// device cookie are all an unknown device past the count (429 with the correct
// password); the genuine one is admitted (the positive control).
func TestAnInvalidDeviceCookieIsAnUnknownDevice(t *testing.T) {
	s, h := pwaServer(t, nil)
	dev := knownDeviceOf(t, h)
	other := AuthConfig{UIPassword: testUIPassword + "-another"}
	for name, v := range map[string]string{
		"forged":          "1|99999999999|" + strings.Repeat("ab", 32),
		"expired":         auth.SignSession(s.auth.deviceSecret(), time.Now().Add(-knownDeviceTTL-time.Hour), knownDeviceTTL),
		"another key":     auth.SignSession(other.deviceSecret(), time.Now(), knownDeviceTTL),
		"a session value": auth.SignSession(s.auth.uiSessionSecret(), time.Now(), knownDeviceTTL),
	} {
		_, h := pwaServer(t, nil)
		flood(h, loginRateBurst)
		if c := passwordLogin(h, testUIPassword, arriveFrom(sharedAddress), withDevice(v)).Code; c != http.StatusTooManyRequests {
			t.Errorf("%s device cookie, correct password past the count: %d, want 429 (an unknown device)", name, c)
		}
	}
	_, h2 := pwaServer(t, nil)
	flood(h2, loginRateBurst)
	if c := passwordLogin(h2, testUIPassword, arriveFrom(sharedAddress), withDevice(dev)).Code; c != http.StatusSeeOther {
		t.Fatalf("CONTROL: the genuine device cookie on another server with the same password: %d, want 303", c)
	}
}

// TestRotatingThePasswordForgetsEveryDevice: a device cookie issued under one
// operator password is an unknown device once the password changes.
func TestRotatingThePasswordForgetsEveryDevice(t *testing.T) {
	_, h := pwaServer(t, nil)
	dev := knownDeviceOf(t, h)
	const rotated = "a-completely-new-operator-password"
	s2 := New(nil, AuthConfig{UIPassword: rotated}, log.New(io.Discard, "", 0))
	h2 := s2.Handler()
	flood(h2, loginRateBurst)
	if c := passwordLogin(h2, rotated, arriveFrom(sharedAddress), withDevice(dev)).Code; c != http.StatusTooManyRequests {
		t.Fatalf("a device cookie from before the rotation, past the count: %d, want 429 (unknown)", c)
	}
}

// TestAKnownDevicesWrongPasswordStillCounts: loginRateBurst-1 failures from
// unknown devices plus ONE from a known device reach the count, so an unknown
// device's correct password is then refused.
func TestAKnownDevicesWrongPasswordStillCounts(t *testing.T) {
	_, h := pwaServer(t, nil)
	dev := knownDeviceOf(t, h)
	flood(h, loginRateBurst-1)
	if c := passwordLogin(h, testUIPassword, arriveFrom(sharedAddress)).Code; c != http.StatusSeeOther {
		t.Fatalf("CONTROL: one below the count, an unknown device's correct password: %d, want 303", c)
	}
	passwordLogin(h, "wrong-from-a-known-device", arriveFrom(sharedAddress), withDevice(dev))
	if c := passwordLogin(h, testUIPassword, arriveFrom(sharedAddress)).Code; c != http.StatusTooManyRequests {
		t.Fatalf("after the known device's wrong password made the count: %d, want 429", c)
	}
}

// TestThePasswordModeConfirmFollowsTheKnownDeviceRule: the step-up confirm
// shares the counts and the rule. A confirm always carries a valid session,
// and a valid session is a known device: past the count the correct password
// is admitted (with or without a device cookie), a wrong one is 429 (counted,
// throttled), and a successful confirm issues the device cookie.
func TestThePasswordModeConfirmFollowsTheKnownDeviceRule(t *testing.T) {
	s, h := pwaServer(t, nil)
	dev := knownDeviceOf(t, h)
	flood(h, loginRateBurst)
	session := auth.SignSession(s.auth.uiSessionSecret(), time.Now().Add(-time.Hour), sessionTTL)
	confirm := func(pw string, opts ...reqOpt) *httptest.ResponseRecorder {
		base := []reqOpt{func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session}) }, asXHR, arriveFrom(sharedAddress)}
		return do(h, http.MethodPost, stepUpConfirmPath, url.Values{"password": {pw}}, append(base, opts...)...)
	}
	w := confirm(testUIPassword)
	if w.Code != http.StatusOK {
		t.Errorf("confirm from a signed-in browser with no device cookie past the count: %d %s, want 200", w.Code, w.Body)
	}
	if ck := setCookies(w)[knownDeviceCookieName]; ck == nil || ck.Value == "" {
		t.Errorf("a successful confirm issued no device cookie")
	}
	if w := confirm(testUIPassword, withDevice(dev)); w.Code != http.StatusOK {
		t.Errorf("confirm from a KNOWN device past the count: %d %s, want 200", w.Code, w.Body)
	}
	if w := confirm("not-the-password", withDevice(dev)); w.Code != http.StatusTooManyRequests || errorOf(w) != stepUpThrottled {
		t.Errorf("a known device's WRONG password confirm past the count: %d %s, want 429 %q", w.Code, w.Body, stepUpThrottled)
	}
}

// TestTheDeviceCookieIsLaxAndHttpOnly: the cookie's attributes as issued.
func TestTheDeviceCookieIsLaxAndHttpOnly(t *testing.T) {
	_, h := pwaServer(t, nil)
	ck := setCookies(passwordLogin(h, testUIPassword))[knownDeviceCookieName]
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.MaxAge != int(knownDeviceTTL/time.Second) {
		t.Fatalf("device cookie %+v, want HttpOnly, SameSite=Lax, Path=/, MaxAge=%d", ck, int(knownDeviceTTL/time.Second))
	}
}

// TestASignedInBrowserIsAKnownDevice: a valid session cookie with the correct
// password signs in past the count, with no device cookie; a forged session
// value is an unknown device (the positive control).
func TestASignedInBrowserIsAKnownDevice(t *testing.T) {
	s, h := pwaServer(t, nil)
	flood(h, loginRateBurst)
	withSession := func(v string) reqOpt {
		return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: v}) }
	}
	good := auth.SignSession(s.auth.uiSessionSecret(), time.Now().Add(-time.Hour), sessionTTL)
	if c := passwordLogin(h, testUIPassword, arriveFrom(sharedAddress), withSession(good)).Code; c != http.StatusSeeOther {
		t.Errorf("a valid session with the correct password past the count: %d, want 303", c)
	}
	if c := passwordLogin(h, testUIPassword, arriveFrom(sharedAddress), withSession("1|2|"+strings.Repeat("00", 32))).Code; c != http.StatusTooManyRequests {
		t.Errorf("CONTROL: a forged session value past the count: %d, want 429", c)
	}
}

// TestAFloodFromOneAddressDoesNotRefuseAnotherAddress: with a trusted proxy
// naming each client, loginRateBurst failures from client A refuse A's unknown
// device but not B's; failures spread over many addresses past
// loginRateGlobalBurst refuse an unknown device at a fresh address too.
func TestAFloodFromOneAddressDoesNotRefuseAnotherAddress(t *testing.T) {
	pol, err := clientaddr.New(clientaddr.Config{TrustedProxies: "192.0.2.0/24", Header: "X-Forwarded-For"})
	if err != nil {
		t.Fatal(err)
	}
	build := func() http.Handler {
		s := New(nil, AuthConfig{UIPassword: testUIPassword}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{ClientAddr: pol})
		return s.Handler()
	}
	client := func(ip string) reqOpt {
		return func(r *http.Request) { arriveFrom(sharedAddress)(r); r.Header.Set("X-Forwarded-For", ip) }
	}
	h := build()
	for i := 0; i < loginRateBurst; i++ {
		passwordLogin(h, "not-the-password-at-all", client("198.51.100.1"))
	}
	if c := passwordLogin(h, testUIPassword, client("198.51.100.1")).Code; c != http.StatusTooManyRequests {
		t.Errorf("CONTROL: the flooding client's own unknown device: %d, want 429", c)
	}
	if c := passwordLogin(h, testUIPassword, client("198.51.100.2")).Code; c != http.StatusSeeOther {
		t.Errorf("another client's unknown device after a flood from the first: %d, want 303", c)
	}

	h = build()
	for i := 0; i < loginRateGlobalBurst; i++ {
		passwordLogin(h, "not-the-password-at-all", client("203.0.113."+strconv.Itoa(1+i%(loginRateBurst*2))))
	}
	if c := passwordLogin(h, testUIPassword, client("198.51.100.9")).Code; c != http.StatusTooManyRequests {
		t.Errorf("a fresh client's unknown device after %d failures spread over many addresses: %d, want 429 (the process-wide bound)",
			loginRateGlobalBurst, c)
	}
}

// TestARefusedUnknownDeviceIsLoggedOnceAMinute: a refusal is logged, with the
// address and which count, at most once a minute.
func TestARefusedUnknownDeviceIsLoggedOnceAMinute(t *testing.T) {
	var buf syncBuf
	s := New(nil, AuthConfig{UIPassword: testUIPassword}, log.New(&buf, "", 0))
	h := s.Handler()
	flood(h, loginRateBurst)
	for i := 0; i < 3; i++ {
		passwordLogin(h, testUIPassword, arriveFrom(sharedAddress))
	}
	got := strings.Count(buf.String(), "login: refused an operator-password attempt from "+sharedAddress+" with no known device: its address's failure count")
	if got != 1 {
		t.Fatalf("refusal log lines: %d, want 1 (rate-limited)\n%s", got, buf.String())
	}
}

// TestAShareAddressPastItsFailureLimitStillAdmitsACorrectSignIn: gotrue mode,
// every request from one address with no forwarding header (the shared proxy).
// loginFailureLimit junk sign-ins at invented emails, then a member's correct
// password signs in, and the member's step-up confirm with the correct
// password succeeds.
func TestAShareAddressPastItsFailureLimitStillAdmitsACorrectSignIn(t *testing.T) {
	f := newGoTrueFixture(t)
	f.member("victim@example.com", 7, authz.RoleMember)
	for i := 0; i < loginFailureLimit; i++ {
		do(f.edge, http.MethodPost, "/login", url.Values{"email": {"nobody" + strconv.Itoa(i) + "@example.com"}, "password": {"x"}},
			arriveFrom(sharedAddress))
	}
	if w := do(f.edge, http.MethodPost, "/login", url.Values{"email": {"victim@example.com"}, "password": {"correct horse battery"}},
		arriveFrom(sharedAddress)); w.Code != http.StatusSeeOther {
		t.Errorf("the victim's CORRECT sign-in after %d junk attempts from the shared address: %d, want 303", loginFailureLimit, w.Code)
	}
	if w := do(f.edge, http.MethodPost, "/login", url.Values{"email": {"victim@example.com"}, "password": {"wrong password!!"}},
		arriveFrom(sharedAddress)); w.Code != http.StatusTooManyRequests {
		t.Errorf("CONTROL: a wrong password from the address past its limit: %d, want 429", w.Code)
	}

	r := newStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	for i := 0; i < loginFailureLimit; i++ {
		do(r.edge, http.MethodPost, "/login", url.Values{"email": {"nobody" + strconv.Itoa(i) + "@example.com"}, "password": {"x"}},
			arriveFrom(sharedAddress))
	}
	if w := r.confirm(at, rt, url.Values{"password": {stepUpOwnerPW}}, arriveFrom(sharedAddress)); w.Code != http.StatusOK {
		t.Errorf("a step-up confirm with the CORRECT password from the address past its limit: %d %s, want 200", w.Code, w.Body)
	}
}

type pairingCodes struct {
	devices.Store
	calls *[]string
}

func (p pairingCodes) Redeem(_ context.Context, code string, _ devices.PairIn) (string, devices.Device, error) {
	*p.calls = append(*p.calls, code)
	if code == "good-code" {
		return "mdv_test", devices.Device{ID: 1, OwnerID: 1, Name: "phone"}, nil
	}
	return "", devices.Device{}, devices.ErrBadCode
}

// TestAValidPairingCodeIsRedeemedAfterJunkFromItsAddress: devicePairPerAddr
// junk codes from one address, then a VALID code from it is redeemed (201, and
// Redeem was asked about it); one more junk code is 429.
func TestAValidPairingCodeIsRedeemedAfterJunkFromItsAddress(t *testing.T) {
	var calls []string
	s := New(nil, AuthConfig{UIPassword: testUIPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Devices: pairingCodes{calls: &calls}})
	h := s.Handler()
	pair := func(code string) int {
		req := httptest.NewRequest(http.MethodPost, devicePairPath, strings.NewReader(`{"code":"`+code+`"}`))
		req.RemoteAddr = sharedAddress + ":51234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < devicePairPerAddr; i++ {
		if c := pair("junk"); c != http.StatusUnauthorized {
			t.Fatalf("junk attempt %d: %d, want 401", i, c)
		}
	}
	c := pair("good-code")
	asked := false
	for _, x := range calls {
		asked = asked || x == "good-code"
	}
	if c != http.StatusCreated || !asked {
		t.Fatalf("a VALID pairing code after %d junk attempts from its address: %d (Redeem asked: %v), want 201 and asked",
			devicePairPerAddr, c, asked)
	}
	if c := pair("junk"); c != http.StatusTooManyRequests {
		t.Fatalf("CONTROL: junk past the limit: %d, want 429", c)
	}
}
