package api

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
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
// cookie this server signed (or, at sign-in, a valid session), and is NEVER refused for the
// failure counts with the correct password; any other request is REFUSED once
// its address's count, or the process-wide one, is past its limit, correct
// password or not (passwordGate).
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
	ck, err := r.Cookie(s.auth.cookieName(knownDeviceCookieName))
	if err != nil || ck.Value == "" {
		return false
	}
	_, err = auth.VerifySession(s.auth.deviceSecret(), ck.Value, time.Now())
	return err == nil
}

// setKnownDeviceCookie marks the browser that just proved the password.
func (s *Server) setKnownDeviceCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.auth.cookieName(knownDeviceCookieName),
		Value:    auth.SignSession(s.auth.deviceSecret(), time.Now(), knownDeviceTTL),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.passwordCookieSecure(r),
		MaxAge:   int(knownDeviceTTL / time.Second),
	})
}

// loginRateGlobalBurst bounds wrong operator passwords per loginRateWindow
// across ALL addresses: a guesser spread over many addresses is held to it.
// Each address has its own loginRateBurst (Server.loginRateAddr), so a flood
// from one address does not refuse an unknown device at another.
const loginRateGlobalBurst = 100

// passwordGate is the operator-password check behind the failure counts.
//
// A request is from a KNOWN device when it carries a valid device cookie, or —
// for the SIGN-IN only (sessionIsKnown) — a valid session cookie (that session
// already proved the current password) — and either counts only on a
// SAME-ORIGIN request (isSameOrigin).
//
// 🔴 WHY THE EXEMPTION NEEDS THE ORIGIN. SameSite=Lax attaches both cookies to
// a POST from any same-site page, and a known device is never refused before
// the compare, so without the check a sibling page in such a browser could post
// wrong passwords past the refusal (blind, but unthrottled). muster's own
// sign-in and step-up forms always send their own Origin.
//
// 🔴 NEVER A SESSION AT THE STEP-UP CONFIRM. Step-up exists for the case where
// the session itself is in the wrong hands; a session proving "known device"
// there would let whoever holds a stolen cookie guess the operator password with
// no refusal. At the confirm only the signed device cookie counts.
//   - refused: an UNKNOWN device whose address is past loginRateBurst, or
//     while the process-wide count is past loginRateGlobalBurst; the password
//     was not compared (answer 429). Logged at most once a minute.
//   - ok: the password is correct.
//   - !ok && !refused: a wrong password, counted against its address and the
//     process; throttled reports that either is now past its burst (answer
//     429 rather than 401).
//
// ⚠ Behind a proxy that is not declared trusted (MUSTER_TRUSTED_PROXIES),
// every client is the proxy's address, so the per-address count is one count
// for everyone: a flood refuses every unknown device. Known devices and
// signed-in browsers are unaffected either way.
func (s *Server) passwordGate(r *http.Request, password string, now time.Time, sessionIsKnown bool) (ok, refused, throttled bool) {
	ip := clientIP(r)
	known := s.knownDevice(r) || (sessionIsKnown && s.hasValidSession(r))
	if !(known && s.isSameOrigin(r)) {
		addrOver := s.loginRateAddr.count(ip, now, loginRateWindow) >= loginRateBurst
		if addrOver || s.loginRate.over(now, loginRateGlobalBurst, loginRateWindow) {
			s.loginRefusedLog.do(now, func(suppressed int) {
				which := "the whole server's failure count"
				if addrOver {
					which = "its address's failure count"
				}
				s.logger.Printf("login: refused an operator-password attempt from %s with no known device: %s is past "+
					"its limit, so the password was not checked%s", ip, which, suppressedNote(suppressed))
			})
			return false, true, false
		}
	}
	if password != "" && auth.ConstantTimeEqual(password, s.auth.UIPassword) {
		return true, false, false
	}
	s.loginRateAddr.add(ip, now, loginRateWindow)
	globalOver := s.loginRate.fail(now, loginRateGlobalBurst, loginRateWindow)
	return false, false, globalOver || s.loginRateAddr.count(ip, now, loginRateWindow) > loginRateBurst
}

// everyMinute runs a function at most once a minute and counts what it
// skipped (the refusal log above).
type everyMinute struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func (e *everyMinute) do(now time.Time, f func(suppressed int)) {
	e.mu.Lock()
	if !e.last.IsZero() && now.Sub(e.last) < time.Minute {
		e.suppressed++
		e.mu.Unlock()
		return
	}
	n := e.suppressed
	e.last, e.suppressed = now, 0
	e.mu.Unlock()
	f(n)
}

func suppressedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d more like it in the last minute not logged)", n)
}
