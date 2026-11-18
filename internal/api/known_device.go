package api

import (
	"crypto/sha256"
	"net/http"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
)

// The password-mode known-device cookie (the OWASP "device cookie" pattern for
// online guessing).
//
// 🔴 WHY: password mode has ONE shared credential. A failure limit that refuses
// before the compare lets any anonymous caller lock the operator out (ten junk
// POSTs a minute); one that only counts lets a guesser run at line rate, since a
// correct guess is admitted whatever the count. The device cookie separates the
// two populations: a browser that has signed in correctly before carries a
// cookie this server signed, and is NEVER refused for the shared count with the
// correct password; every other request shares the failure count and is
// REFUSED past it, correct password or not (loginGate).
//
//   - Issued on every successful operator-password sign-in and step-up confirm.
//   - HttpOnly, SameSite=Lax, Secure as the session cookie is
//     (passwordCookieSecure), long-lived (knownDeviceTTL).
//   - Signed with deviceSecret, which is derived from the operator password like
//     the session key but under its own label: rotating MUSTER_UI_PASSWORD
//     invalidates every device cookie at once, and a session cookie can never
//     pass as a device cookie or the other way round.
//   - It is not a credential: it admits nothing on its own. It only decides
//     whether the shared failure count may refuse a request before the
//     password is compared. A wrong password from a known device still counts.
const (
	knownDeviceCookieName = "muster_device"
	knownDeviceTTL        = 400 * 24 * time.Hour
)

// deviceSecret is the device cookie's signing key (see uiSessionSecret for why
// it is derived from the password).
func (c AuthConfig) deviceSecret() []byte {
	sum := sha256.Sum256([]byte("muster-ui-device-v1\x00" + c.UIPassword))
	return sum[:]
}

// knownDevice reports whether r carries a device cookie this server signed,
// unexpired, under the current operator password.
func (s *Server) knownDevice(r *http.Request) bool {
	ck, err := r.Cookie(knownDeviceCookieName)
	if err != nil || ck.Value == "" {
		return false
	}
	_, err = auth.VerifySession(s.auth.deviceSecret(), ck.Value, time.Now())
	return err == nil
}

// setKnownDeviceCookie marks the browser that just proved the password.
func (s *Server) setKnownDeviceCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     knownDeviceCookieName,
		Value:    auth.SignSession(s.auth.deviceSecret(), time.Now(), knownDeviceTTL),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.passwordCookieSecure(r),
		MaxAge:   int(knownDeviceTTL / time.Second),
	})
}

// passwordGate is the operator-password check behind the shared failure count.
//   - refused: an UNKNOWN device while the count is past loginRateBurst; the
//     password was not compared (answer 429).
//   - ok: the password is correct.
//   - !ok && !refused: a wrong password, counted; throttled reports that the
//     count is now past the burst (answer 429 rather than 401).
func (s *Server) passwordGate(r *http.Request, password string, now time.Time) (ok, refused, throttled bool) {
	if !s.knownDevice(r) && s.loginRate.over(now, loginRateBurst, loginRateWindow) {
		return false, true, false
	}
	if password != "" && auth.ConstantTimeEqual(password, s.auth.UIPassword) {
		return true, false, false
	}
	return false, false, s.loginRate.fail(now, loginRateBurst, loginRateWindow)
}
