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
	"github.com/ZacxDev/muster/internal/devices"
)

// sharedAddress is every client's address behind an untrusted proxy.
const sharedAddress = "192.0.2.5"

// TestAJunkFloodDoesNotLockTheOperatorOut: password mode. loginRateBurst wrong
// passwords, then the operator's correct one — from the same address and from
// another — signs in. A wrong one past the burst is 429 (failures still count).
func TestAJunkFloodDoesNotLockTheOperatorOut(t *testing.T) {
	_, h := pwaServer(t, nil)
	post := func(ip, pw string) int {
		return do(h, http.MethodPost, "/login", url.Values{"password": {pw}, "next": {"/"}}, arriveFrom(ip)).Code
	}
	for i := 0; i < loginRateBurst; i++ {
		if c := post(sharedAddress, "not-the-password-at-all"); c != http.StatusUnauthorized {
			t.Fatalf("junk %d: %d, want 401", i, c)
		}
	}
	if c := post(sharedAddress, testUIPassword); c != http.StatusSeeOther {
		t.Errorf("the operator's CORRECT password after %d junk attempts from the shared address: %d, want 303", loginRateBurst, c)
	}
	if c := post("198.51.100.7", testUIPassword); c != http.StatusSeeOther {
		t.Errorf("the operator's CORRECT password from another address after the flood: %d, want 303", c)
	}
	if c := post(sharedAddress, "still-not-the-password"); c != http.StatusTooManyRequests {
		t.Errorf("CONTROL: a wrong password past the burst: %d, want 429 (failures are still counted)", c)
	}
}

// TestAJunkFloodDoesNotBlockThePasswordModeConfirm: the step-up confirm shares
// the operator-password count, so the same flood must not refuse the correct
// password there either.
func TestAJunkFloodDoesNotBlockThePasswordModeConfirm(t *testing.T) {
	s, h := pwaServer(t, nil)
	for i := 0; i < loginRateBurst; i++ {
		do(h, http.MethodPost, "/login", url.Values{"password": {"not-the-password-at-all"}}, arriveFrom(sharedAddress))
	}
	cookie := auth.SignSession(s.auth.uiSessionSecret(), time.Now().Add(-time.Hour), sessionTTL)
	w := do(h, http.MethodPost, stepUpConfirmPath, url.Values{"password": {testUIPassword}},
		func(r *http.Request) { r.Header.Set("Cookie", sessionCookieName+"="+cookie) }, asXHR, arriveFrom(sharedAddress))
	if w.Code != http.StatusOK {
		t.Fatalf("a step-up confirm with the CORRECT password after %d junk sign-ins: %d %s, want 200", loginRateBurst, w.Code, w.Body)
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
