package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// TestALiveLinkWorksFromAnAddressPastItsMissLimit: inviteIPLimit junk invite
// tokens from one address put it past its miss limit (a junk one is now 429),
// and a LIVE invite link from that same address still opens and redeems, and a
// live password-reset link from it still resets. The address count holds
// misses only, taken after the lookup.
func TestALiveLinkWorksFromAnAddressPastItsMissLimit(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("")
	for i := 0; i < inviteIPLimit; i++ {
		if rec := do(w.edge, http.MethodPost, InvitePath, url.Values{"step": {"open"}, "token": {"nope" + strconv.Itoa(i)}},
			arriveFrom(sharedAddress)); rec.Code != http.StatusNotFound {
			t.Fatalf("junk %d: %d, want the invalid page (404)", i, rec.Code)
		}
	}
	if rec := do(w.edge, http.MethodPost, InvitePath, url.Values{"step": {"open"}, "token": {"nope"}},
		arriveFrom(sharedAddress)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("CONTROL: junk past the limit: %d, want 429", rec.Code)
	}
	if rec := do(w.edge, http.MethodPost, InvitePath, url.Values{"step": {"open"}, "token": {tok}},
		arriveFrom(sharedAddress)); rec.Code != http.StatusOK {
		t.Fatalf("a LIVE invite link opened from the address past its miss limit: %d, want 200", rec.Code)
	}
	if rec := do(w.edge, http.MethodPost, InvitePath, redeemForm(tok, "shared@invite.test", "", goodPW, goodPW),
		arriveFrom(sharedAddress)); rec.Code != http.StatusSeeOther {
		t.Fatalf("a LIVE invite redeemed from the address past its miss limit: %d, want 303", rec.Code)
	}

	var uid int64
	if err := w.pool.QueryRow(authz.System(context.Background()), `SELECT id FROM users WHERE email = 'shared@invite.test'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	rec := do(w.edge, http.MethodPost, "/admin/users/"+strconv.FormatInt(uid, 10)+"/reset-link", url.Values{}, w.asAdmin())
	m := regexp.MustCompile(regexp.QuoteMeta(invitePublic+ResetPath+"#") + `([A-Za-z0-9_-]+)`).FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("reset link: %d", rec.Code)
	}
	const newPW = "a-brand-new-password"
	if r := do(w.edge, http.MethodPost, ResetPath, url.Values{"token": {m[1]}, "password": {newPW}, "password2": {newPW}},
		arriveFrom(sharedAddress)); r.Code != http.StatusSeeOther {
		t.Fatalf("a LIVE reset link used from the address past its miss limit: %d, want 303", r.Code)
	}
}

// TestASharedAddressIsNeverBlockedFromPasskeySignIn: the NAT case. Fifty
// sign-in page loads from one address (each fetches a challenge) — then a
// passkey sign-in from that same address succeeds.
func TestASharedAddressIsNeverBlockedFromPasskeySignIn(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-nat@invite.test")
	w.fake.AddPasskey(sub, "cred-nat")
	for i := 0; i < 50; i++ {
		if rec := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{}, arriveFrom(sharedAddress)); rec.Code != http.StatusOK {
			t.Fatalf("page load %d's challenge from the shared address: %d, want 200 (never refused per address)", i+1, rec.Code)
		}
	}
	if rec := w.passkeySignIn("cred-nat", gotruetest.FlagUPUV, arriveFrom(sharedAddress)); rec.Code != http.StatusOK {
		t.Fatalf("a passkey sign-in from the shared address after 50 page loads: %d %s, want 200", rec.Code, rec.Body)
	}
}

// TestAPasskeyVerifyNeedsAChallengeMusterIssued: a verify naming a challenge
// muster never issued, one already used, or one evicted from the capped table
// is refused with PasskeyChallengeGone WITHOUT asking GoTrue; a held one is
// verified (the positive control).
func TestAPasskeyVerifyNeedsAChallengeMusterIssued(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-ch@invite.test")
	w.fake.AddPasskey(sub, "cred-ch")
	verify := func(ch string) *httptest.ResponseRecorder {
		return postJSON(w.edge, passkeySignInVerifyPath, map[string]any{
			"challenge_id": ch, "credential": gotruetest.Assertion("cred-ch", gotruetest.FlagUPUV), "next": "/"})
	}
	options := func() string {
		o := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{})
		ch, _ := jsonBody(t, o)["challenge_id"].(string)
		if o.Code != http.StatusOK || ch == "" {
			t.Fatalf("options: %d %s", o.Code, o.Body)
		}
		return ch
	}
	gone := func(what string, rec *httptest.ResponseRecorder, before int) {
		t.Helper()
		if rec.Code != http.StatusBadRequest || errorOf(rec) != PasskeyChallengeGone {
			t.Errorf("%s: %d %s, want 400 %q", what, rec.Code, rec.Body, PasskeyChallengeGone)
		}
		if n := w.fake.CountCalls(http.MethodPost, "/passkeys/authentication/verify"); n != before {
			t.Errorf("%s reached GoTrue's verify (%d calls, was %d)", what, n, before)
		}
	}
	calls := func() int { return w.fake.CountCalls(http.MethodPost, "/passkeys/authentication/verify") }

	n := calls()
	gone("a challenge muster never issued", verify("never-issued-challenge"), n)

	ch := options()
	if rec := verify(ch); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: a held challenge: %d %s, want 200", rec.Code, rec.Body)
	}
	n = calls()
	gone("the same challenge again", verify(ch), n)

	w.s.passkeyChallenges = boundedWindow{cap: 2}
	old := options()
	options()
	options() // evicts old
	n = calls()
	gone("an evicted challenge", verify(old), n)
}
