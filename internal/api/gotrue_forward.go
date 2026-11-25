package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue"
)

// Forwarding the signed-in person's own access token to GoTrue.
//
// 🔴 MUSTER AND GoTrue DISAGREE ABOUT WHEN A TOKEN EXPIRES. muster's Verifier
// accepts an access token until exp PLUS a clock-skew allowance
// (gotrue.DefaultSkew), and the session gate refreshes only a token it refuses.
// GoTrue gives no allowance: from the exp second on it answers 403 bad_jwt
// "token is expired". So inside the allowance a request is admitted on a token
// GoTrue will not take, and every handler that called GoTrue AS the user with
// it failed there — the passkey list rendered 502, a sign-out left the session
// alive in GoTrue. asGoTrueUser is the one way such a call is made
// (TestEveryUserTokenForwardGoesThroughTheHelper keeps it that way).

// userTokenMargin is how much life an access token must have left to be
// forwarded as it is; one with less is refreshed first, so it cannot expire
// between muster's check and GoTrue's.
const userTokenMargin = 10 * time.Second

// userTokenRefreshError is a refresh asGoTrueUser needed and did not get. The
// call was not made. answerUserTokenRefreshFailure turns it into the same
// answer the session gate gives a failed refresh.
type userTokenRefreshError struct{ err error }

func (e *userTokenRefreshError) Error() string {
	return "refreshing the session before calling GoTrue as its user: " + e.err.Error()
}

func (e *userTokenRefreshError) Unwrap() error { return e.err }

// asGoTrueUser makes call — one GoTrue request authenticated as the person
// whose session creds is — with an access token GoTrue will accept:
//
//  1. a token past its exp, or within userTokenMargin of it, is refreshed
//     BEFORE the call (refreshGoTrueSession, every guard of the session gate).
//     If that refresh fails only for now (not a verdict on the session) and
//     the token is still before its exp, the call goes ahead with it;
//  2. if GoTrue still refuses the token as expired (its clock ahead of
//     muster's), the session is refreshed once and the call made once more.
//
// A refresh updates creds in place and hands the browser the new pair
// (setGoTrueCookies, as the session gate does), so the caller must not have
// written its response yet. A refresh that fails, other than the case 1
// forwards anyway, returns a *userTokenRefreshError without making the call.
func (s *Server) asGoTrueUser(w http.ResponseWriter, r *http.Request, creds *gotrueCreds, call func(accessToken string) error) error {
	refreshed := false
	if exp := time.Unix(creds.claims.ExpiresAt, 0); !s.gt.now().Add(userTokenMargin).Before(exp) {
		err := s.refreshUserToken(w, r, creds)
		switch {
		case err == nil:
			refreshed = true
		case s.gt.now().Before(exp) && (creds.refresh == "" || !endsTheSession(errors.Unwrap(err))):
			// 🔴 A MARGIN REFRESH THAT COULD NOT BE MADE RIGHT NOW (GoTrue 5xx,
			// a throttle, a store error) IS NOT A REASON TO FAIL THE CALL while
			// the token is still before its exp: GoTrue takes it, so it is
			// forwarded as it is (TestAFailedMarginRefreshForwardsTheLiveToken).
			// Sign out matters most here — skipping GoTrue's /logout would
			// leave the refresh side alive. No refresh cookie is the same case
			// (nothing to refresh with, but nothing wrong with the token). A
			// session the refresh ENDED is never forwarded.
			s.logger.Printf("gotrue: forwarding a token %s from its exp; the early refresh failed: %v",
				exp.Sub(s.gt.now()).Round(time.Second), err)
		default:
			return err
		}
	}
	err := call(creds.access)
	if refreshed || !gotrueRefusedAsExpired(err) {
		return err
	}
	s.logger.Printf("gotrue: refused as expired an access token muster still had %s on; refreshing once (are the clocks apart?)",
		time.Unix(creds.claims.ExpiresAt, 0).Sub(s.gt.now()).Round(time.Second))
	if err := s.refreshUserToken(w, r, creds); err != nil {
		return err
	}
	return call(creds.access)
}

// refreshUserToken refreshes creds' session and hands the browser the new pair.
func (s *Server) refreshUserToken(w http.ResponseWriter, r *http.Request, creds *gotrueCreds) error {
	if creds.refresh == "" {
		return &userTokenRefreshError{gotrue.ErrExpired}
	}
	fresh, c, err := s.refreshGoTrueSession(r, creds.claims, creds.refresh)
	if err != nil {
		return &userTokenRefreshError{err}
	}
	s.setGoTrueCookies(w, fresh)
	creds.access, creds.refresh, creds.claims = fresh.AccessToken, fresh.RefreshToken, c
	return nil
}

// gotrueRefusedAsExpired reports whether err is GoTrue refusing a user access
// token because it has expired: bad_jwt, 401 or 403, "expired" in the message.
// MEASURED (a failing e2e run, as muster logged it):
// `gotrue: HTTP 403 bad_jwt: invalid JWT: … token is expired`.
func gotrueRefusedAsExpired(err error) bool {
	var ae *gotrue.APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) &&
		ae.Code == "bad_jwt" && strings.Contains(strings.ToLower(ae.Msg), "expired")
}

// answerUserTokenRefreshFailure answers (and returns true) when err is a
// refresh asGoTrueUser could not make, exactly as the session gate answers its
// own (answerSessionFailure): a session that is over is cleared and sent to
// sign in again — never shown as a GoTrue error — and one that could not be
// judged right now keeps its cookies and is asked to retry. Any other err is
// the caller's to answer.
func (s *Server) answerUserTokenRefreshFailure(w http.ResponseWriter, r *http.Request, err error) bool {
	var re *userTokenRefreshError
	if !errors.As(err, &re) {
		return false
	}
	s.logger.Printf("session: %v", err)
	s.answerSessionFailure(w, r, re.err)
	return true
}
