package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// The user-token window: muster's Verifier accepts an access token until exp
// plus a clock-skew allowance (gotrue.DefaultSkew), while GoTrue refuses it from
// exp on (403 bad_jwt "token is expired"; the fake models that strictly). A
// handler that forwards the session's own access token to GoTrue inside that
// window must refresh the session first, or GoTrue refuses the call and the
// person sees an error page for a session muster itself still honours.

// isUserAPICall reports whether c went to a GoTrue route that takes a USER
// access token as bearer.
func isUserAPICall(c gotruetest.Call) bool {
	return c.Path == "/logout" || c.Path == "/passkeys" || c.Path == "/passkeys/" ||
		strings.HasPrefix(c.Path, "/passkeys/registration/") ||
		(c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "/passkeys/"))
}

// expiredRefusals counts user-API calls GoTrue refused as an expired token.
func expiredRefusals(f *gotruetest.Fake) int {
	n := 0
	for _, c := range f.Calls() {
		if isUserAPICall(c) && c.Status == http.StatusForbidden {
			n++
		}
	}
	return n
}

// okCalls counts calls to method+path GoTrue answered 2xx.
func okCalls(f *gotruetest.Fake, method, path string) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Method == method && c.Path == path && c.Status >= 200 && c.Status < 300 {
			n++
		}
	}
	return n
}

// skewWorld is a passkey world with a member signed in on a one-minute access
// token and one passkey, the clock moved to `past` after that token's exp.
// It returns the world, the member's GoTrue sub, the passkey's id and the
// session's (access, refresh) cookies.
func skewWorld(t *testing.T, past time.Duration) (*invWorld, string, string, string, string) {
	t.Helper()
	w := newPasskeyWorld(t)
	w.fake.SetAccessTTL(time.Minute)
	_, sub, at, rt := w.joinMember("skew@invite.test")
	pkID := w.fake.AddPasskey(sub, "cred-skew")
	c, err := w.fake.Verifier().Verify(at)
	if err != nil {
		t.Fatal(err)
	}
	w.fake.Advance(time.Unix(c.ExpiresAt, 0).Sub(w.fake.Now()) + past)
	return w, sub, pkID, at, rt
}

// forwardSites is every handler that calls GoTrue as the signed-in person,
// with the user-API call it must make and the answer it must give.
var forwardSites = []struct {
	name         string
	method, path string // the GoTrue call ("" path: DELETE /passkeys/<the passkey id>)
	want         int
	run          func(w *invWorld, sub, pkID string, ck reqOpt) *httptest.ResponseRecorder
}{
	{"the passkey list", http.MethodGet, "/passkeys", http.StatusOK,
		func(w *invWorld, _, _ string, ck reqOpt) *httptest.ResponseRecorder {
			return do(w.edge, "GET", accountPasskeysPath, nil, ck, asNavigation)
		}},
	{"passkey registration options", http.MethodPost, "/passkeys/registration/options", http.StatusOK,
		func(w *invWorld, _, _ string, ck reqOpt) *httptest.ResponseRecorder {
			return postJSON(w.edge, accountPasskeysPath+"/options", map[string]any{}, ck)
		}},
	{"passkey registration verify", http.MethodPost, "/passkeys/registration/verify", http.StatusOK,
		func(w *invWorld, sub, _ string, ck reqOpt) *httptest.ResponseRecorder {
			return postJSON(w.edge, accountPasskeysPath+"/verify", map[string]any{
				"challenge_id": w.fake.NewChallenge(sub), "credential": gotruetest.Attestation("cred-skew-new", gotruetest.FlagUPUV)}, ck)
		}},
	{"passkey removal", http.MethodDelete, "", http.StatusSeeOther,
		func(w *invWorld, _, pkID string, ck reqOpt) *httptest.ResponseRecorder {
			return do(w.edge, "POST", accountPasskeysPath+"/"+pkID+"/delete", url.Values{}, ck)
		}},
	{"the step-up passkey options", http.MethodGet, "/passkeys", http.StatusOK,
		func(w *invWorld, _, _ string, ck reqOpt) *httptest.ResponseRecorder {
			return postJSON(w.edge, "/auth/confirm/passkey/options", map[string]any{}, ck)
		}},
	{"the reset note's passkey signal", http.MethodGet, "/passkeys", http.StatusOK,
		func(w *invWorld, _, _ string, ck reqOpt) *httptest.ResponseRecorder {
			return do(w.edge, "GET", "/account?reset=1&"+resetPasskeysParam+"=1", nil, ck)
		}},
	{"sign out", http.MethodPost, "/logout", http.StatusSeeOther,
		func(w *invWorld, _, _ string, ck reqOpt) *httptest.ResponseRecorder {
			return do(w.edge, "POST", "/logout", url.Values{}, ck)
		}},
}

// TestAUserTokenInsideTheSkewIsRefreshedBeforeItIsForwarded: an access token
// 2 s past its exp — inside muster's allowance, so the request is admitted
// without a refresh — reaches every forwarding site. Each must refresh the
// session BEFORE calling GoTrue (no call refused as expired), make its call
// with the new token, and hand the browser the new pair. Sign out, which
// clears the cookies, must still end the session in GoTrue.
//
// Red before the fix: GoTrue refused each call (403 bad_jwt) and the list
// rendered 502, the JSON routes 401, the removal 404 "not one of yours", and a
// sign-out left the session alive in GoTrue.
func TestAUserTokenInsideTheSkewIsRefreshedBeforeItIsForwarded(t *testing.T) {
	for _, site := range forwardSites {
		t.Run(site.name, func(t *testing.T) {
			w, sub, pkID, at, rt := skewWorld(t, 2*time.Second)
			// CONTROL: muster still accepts the token, so nothing upstream of
			// the handler refreshes it.
			if _, err := w.fake.Verifier().Verify(at); err != nil {
				t.Fatalf("CONTROL: muster's verifier refuses the token 2 s past exp: %v", err)
			}
			path := site.path
			if path == "" {
				path = "/passkeys/" + pkID
			}
			tokens := w.fake.CountCalls("POST", "/token")
			rec := site.run(w, sub, pkID, withCookies(at, rt))
			if rec.Code != site.want {
				t.Fatalf("%s: %d, want %d: %s", site.name, rec.Code, site.want, truncate(rec.Body.String(), 300))
			}
			if n := expiredRefusals(w.fake); n != 0 {
				t.Fatalf("GoTrue refused %d user-API call(s) as expired: the token was forwarded before the refresh", n)
			}
			if okCalls(w.fake, site.method, path) == 0 {
				t.Fatalf("GoTrue never answered %s %s with a 2xx: %+v", site.method, path, w.fake.Calls())
			}
			if n := w.fake.CountCalls("POST", "/token") - tokens; n != 1 {
				t.Fatalf("refresh calls = %d, want 1", n)
			}
			ck := setCookies(rec)
			if site.path == "/logout" {
				c, _ := w.fake.Verifier().Verify(at)
				if !w.fake.SessionRevoked(c.SessionID) {
					t.Fatal("GoTrue still holds the session after sign out")
				}
				return
			}
			nat, nrt := ck[accessCookieName], ck[refreshCookieName]
			if nat == nil || nrt == nil || nat.Value == at || nrt.Value == rt || nat.MaxAge <= 0 {
				t.Fatalf("the refreshed pair was not handed to the browser: %v", ck)
			}
			c, err := w.fake.Verifier().Verify(nat.Value)
			if err != nil || !w.fake.Now().Before(time.Unix(c.ExpiresAt, 0)) {
				t.Fatalf("the new access token: %v (exp %d)", err, c.ExpiresAt)
			}
			// The new pair works on its own, with no further refresh.
			if r := do(w.edge, "GET", "/account", nil, withCookies(nat.Value, nrt.Value), asXHR); r.Code != http.StatusOK {
				t.Fatalf("the refreshed pair: %d", r.Code)
			}
		})
	}
}

// TestTheForwardRefreshMargin measures the margin at two points: a token 5 s
// before its exp is refreshed before it is forwarded (GoTrue would take it, but
// it could expire in flight), one 20 s before its exp is forwarded as is.
func TestTheForwardRefreshMargin(t *testing.T) {
	for _, tc := range []struct {
		before  time.Duration
		refresh int
	}{{5 * time.Second, 1}, {20 * time.Second, 0}} {
		w, _, _, at, rt := skewWorld(t, -tc.before)
		tokens := w.fake.CountCalls("POST", "/token")
		rec := do(w.edge, "GET", accountPasskeysPath, nil, withCookies(at, rt), asNavigation)
		if rec.Code != http.StatusOK || okCalls(w.fake, "GET", "/passkeys") == 0 {
			t.Fatalf("%s before exp: %d", tc.before, rec.Code)
		}
		if n := w.fake.CountCalls("POST", "/token") - tokens; n != tc.refresh {
			t.Fatalf("%s before exp: refresh calls = %d, want %d", tc.before, n, tc.refresh)
		}
		if got := setCookies(rec)[accessCookieName] != nil; got != (tc.refresh == 1) {
			t.Fatalf("%s before exp: cookies set = %v, want %v", tc.before, got, tc.refresh == 1)
		}
	}
}

// TestAForwardRefreshFailureIsACleanSignIn: the token is inside muster's
// allowance but the refresh token is no good. The forwarding site must end the
// session the way the session gate does — a navigation to sign-in, a 401 for a
// fetch, the cookies cleared — not show a GoTrue error.
func TestAForwardRefreshFailureIsACleanSignIn(t *testing.T) {
	w, _, _, at, _ := skewWorld(t, 2*time.Second)
	bad := withCookies(at, "not-a-refresh-token")
	nav := do(w.edge, "GET", accountPasskeysPath, nil, bad, asNavigation)
	if nav.Code != http.StatusSeeOther || !strings.HasPrefix(nav.Header().Get("Location"), "/login") {
		t.Fatalf("the passkey list: %d Location=%q, want 303 to /login", nav.Code, nav.Header().Get("Location"))
	}
	if c := setCookies(nav)[accessCookieName]; c == nil || c.MaxAge >= 0 {
		t.Fatal("a failed refresh did not clear the access cookie")
	}
	for _, path := range []string{accountPasskeysPath + "/options", "/auth/confirm/passkey/options"} {
		rec := postJSON(w.edge, path, map[string]any{}, bad)
		if rec.Code != http.StatusUnauthorized || jsonBody(t, rec)["error"] != "not signed in" {
			t.Fatalf("%s: %d %s, want 401 not signed in", path, rec.Code, rec.Body)
		}
		if c := setCookies(rec)[refreshCookieName]; c == nil || c.MaxAge >= 0 {
			t.Fatalf("%s: a failed refresh did not clear the refresh cookie", path)
		}
	}
	// No refresh cookie at all is the same: nothing to refresh with.
	if rec := do(w.edge, "GET", accountPasskeysPath, nil, withCookies(at, ""), asNavigation); rec.Code != http.StatusSeeOther ||
		!strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("no refresh cookie: %d Location=%q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if n := expiredRefusals(w.fake); n != 0 {
		t.Fatalf("an expired token reached GoTrue %d time(s)", n)
	}
	// A refresh GoTrue could not answer says nothing about the session: the
	// cookies are kept and the caller is asked to retry, as the session gate
	// does (TestATransientFailureKeepsTheSession).
	w.fake.FailToken(http.StatusBadGateway, 1)
	rec := postJSON(w.edge, accountPasskeysPath+"/options", map[string]any{}, withCookies(at, "a-refresh-token"))
	if rec.Code != http.StatusServiceUnavailable || len(setCookies(rec)) != 0 {
		t.Fatalf("GoTrue 502 on the refresh: %d, cookies %v; want 503 with the cookies kept", rec.Code, setCookies(rec))
	}
}

// TestAGoTrueExpiredRefusalIsRetriedOnceAfterARefresh: GoTrue's clock runs
// 20 s ahead of muster's, so a token muster thinks has 18 s left is already
// expired to GoTrue. The pre-check cannot see that; GoTrue's own refusal (403
// bad_jwt "token is expired") is answered by one refresh and one retry.
func TestAGoTrueExpiredRefusalIsRetriedOnceAfterARefresh(t *testing.T) {
	w := newPasskeyWorld(t)
	lagging := w.fake.Verifier()
	lagging.Now = func() time.Time { return w.fake.Now().Add(-20 * time.Second) }
	w.s.UseGoTrue(GoTrueAuth{Client: w.fake.Client(), Verifier: lagging, Users: w.store, Accounts: w.store})
	w.fake.SetAccessTTL(time.Minute)
	_, _, at, rt := w.joinMember("skew-clock@invite.test")
	c, err := w.fake.Verifier().Verify(at)
	if err != nil {
		t.Fatal(err)
	}
	w.fake.Advance(time.Unix(c.ExpiresAt, 0).Sub(w.fake.Now()) + 2*time.Second)
	tokens := w.fake.CountCalls("POST", "/token")
	rec := do(w.edge, "GET", accountPasskeysPath, nil, withCookies(at, rt), asNavigation)
	if rec.Code != http.StatusOK {
		t.Fatalf("the passkey list: %d", rec.Code)
	}
	if n := expiredRefusals(w.fake); n != 1 {
		t.Fatalf("GoTrue expired refusals = %d, want exactly 1 (the one the retry answers)", n)
	}
	if okCalls(w.fake, "GET", "/passkeys") != 1 || w.fake.CountCalls("POST", "/token")-tokens != 1 {
		t.Fatalf("want one refresh and one successful retry: %+v", w.fake.Calls())
	}
	if setCookies(rec)[accessCookieName] == nil {
		t.Fatal("the refreshed pair was not handed to the browser")
	}
}

// TestAFailedMarginRefreshForwardsTheLiveToken: 5 s before exp the helper
// refreshes early, and here GoTrue cannot answer the refresh (502). That says
// nothing about the session and the token is still good, so the call goes
// ahead with it: the list renders, and a sign-out still ends the session in
// GoTrue. The same with no refresh cookie at all. A refresh that ENDS the
// session (a refresh token GoTrue does not know) is never forwarded past.
func TestAFailedMarginRefreshForwardsTheLiveToken(t *testing.T) {
	t.Run("the passkey list", func(t *testing.T) {
		w, _, _, at, rt := skewWorld(t, -5*time.Second)
		w.fake.FailToken(http.StatusBadGateway, 1)
		tokens := w.fake.CountCalls("POST", "/token")
		rec := do(w.edge, "GET", accountPasskeysPath, nil, withCookies(at, rt), asNavigation)
		if rec.Code != http.StatusOK || okCalls(w.fake, "GET", "/passkeys") != 1 {
			t.Fatalf("a failed early refresh: %d, list calls %d; want 200 on the live token", rec.Code, okCalls(w.fake, "GET", "/passkeys"))
		}
		if w.fake.CountCalls("POST", "/token")-tokens != 1 || setCookies(rec)[accessCookieName] != nil {
			t.Fatalf("want one (failed) refresh attempt and no new cookies: %+v", setCookies(rec))
		}
	})
	t.Run("sign out", func(t *testing.T) {
		w, _, _, at, rt := skewWorld(t, -5*time.Second)
		w.fake.FailToken(http.StatusBadGateway, 1)
		rec := do(w.edge, "POST", "/logout", url.Values{}, withCookies(at, rt))
		c, _ := w.fake.Verifier().Verify(at)
		if rec.Code != http.StatusSeeOther || okCalls(w.fake, "POST", "/logout") != 1 || !w.fake.SessionRevoked(c.SessionID) {
			t.Fatalf("sign out after a failed early refresh: %d, GoTrue /logout ok %d, revoked %v",
				rec.Code, okCalls(w.fake, "POST", "/logout"), w.fake.SessionRevoked(c.SessionID))
		}
	})
	t.Run("no refresh cookie", func(t *testing.T) {
		w, _, _, at, _ := skewWorld(t, -5*time.Second)
		if rec := do(w.edge, "GET", accountPasskeysPath, nil, withCookies(at, ""), asNavigation); rec.Code != http.StatusOK {
			t.Fatalf("a live token with no refresh cookie: %d, want 200", rec.Code)
		}
	})
	t.Run("a refresh that ends the session is not forwarded past", func(t *testing.T) {
		w, _, _, at, _ := skewWorld(t, -5*time.Second)
		rec := do(w.edge, "GET", accountPasskeysPath, nil, withCookies(at, "not-a-refresh-token"), asNavigation)
		if rec.Code != http.StatusSeeOther || okCalls(w.fake, "GET", "/passkeys") != 0 {
			t.Fatalf("a refresh GoTrue refused: %d, list calls %d; want 303 to sign in and no call", rec.Code, okCalls(w.fake, "GET", "/passkeys"))
		}
	})
}

// TestARefreshReadsTheCutoffFresh: a session cutoff (or a disable) written by
// another process — straight to the database, as the CLI or a second replica
// does — does not invalidate this process's user cache. The refresh must read
// the user fresh, or it mints a new token past a cutoff the cache has not
// seen. The cache is warmed first; the change lands while it is still warm.
func TestARefreshReadsTheCutoffFresh(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"a session cutoff", `UPDATE users SET sessions_not_before = $2 WHERE id = $1`},
		{"a disable", `UPDATE users SET disabled_at = $2 WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPasskeyWorld(t)
			w.fake.SetAccessTTL(time.Minute)
			uid, _, at, rt := w.joinMember("skew-cutoff@invite.test")
			c, err := w.fake.Verifier().Verify(at)
			if err != nil {
				t.Fatal(err)
			}
			w.fake.Advance(time.Unix(c.ExpiresAt, 0).Sub(w.fake.Now()) - 8*time.Second)
			// Warm the cache: an ordinary request on the live token.
			if rec := do(w.edge, "GET", "/account", nil, withCookies(at, rt), asXHR); rec.Code != http.StatusOK {
				t.Fatalf("CONTROL: the session before the change: %d", rec.Code)
			}
			if _, err := w.pool.Exec(authz.System(context.Background()), tc.sql, uid, time.Unix(c.IssuedAt, 0)); err != nil {
				t.Fatal(err)
			}
			w.fake.Advance(3 * time.Second) // 5 s before exp: inside the margin, cache still warm
			tokens := w.fake.CountCalls("POST", "/token")
			rec := do(w.edge, "GET", accountPasskeysPath, nil, withCookies(at, rt), asNavigation)
			if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
				t.Fatalf("after %s: %d Location=%q, want 303 to sign in", tc.name, rec.Code, rec.Header().Get("Location"))
			}
			if n := w.fake.CountCalls("POST", "/token") - tokens; n != 0 {
				t.Fatalf("after %s: GoTrue was asked for %d refresh(es); want none", tc.name, n)
			}
		})
	}
}
