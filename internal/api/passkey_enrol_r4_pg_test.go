package api

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// THE STICKY TIER (migration 0188, passkey_enrol.go): an account that has
// ever had a passkey or a phone with a step-up key stays gated however its
// live rows are emptied — Sign out everywhere, a removal, an admin's strip —
// and only a completed password reset reopens its first-factor tier. Red at
// the previous head, where the tier was read from the live rows alone.

// TestSigningOutEverywhereDoesNotReopenPairing: 🔴 Sign out everywhere
// unpairs every phone. A password-only session then signed back in may NOT
// pair its own phone with a step-up key as if it were the account's first —
// and the phone being unpaired was told first.
func TestSigningOutEverywhereDoesNotReopenPairing(t *testing.T) {
	r := newEnrolRig(t, true) // a step-up phone, registered for push
	if w := do(r.edge, "POST", "/logout", url.Values{"scope": {"global"}}, withCookies(r.at, r.rt)); w.Code != http.StatusSeeOther {
		t.Fatalf("sign out everywhere: %d", w.Code)
	}
	if n := r.stepUpPhones(t); n != 0 {
		t.Fatalf("fixture: sign out everywhere left %d phone(s)", n)
	}
	if n := r.pushedTitle("You signed out everywhere: your phones are being unpaired"); n != 1 {
		t.Fatalf("the phone was told %d time(s) before it was unpaired, want 1", n)
	}
	r.at, r.rt = r.login() // the password, again
	code, allowed := r.mintCode(t)
	if allowed {
		t.Fatal("after sign out everywhere, a password-only code says it may pair a step-up key")
	}
	if res := r.redeemKeyed(t, code); res.code != http.StatusForbidden {
		t.Fatalf("a password-only pairing after sign out everywhere: %d %s, want 403", res.code, res.body)
	}
}

// TestTheStickyTierSurvivesRemovingTheFactor: the account's only passkey is
// removed (with its proof): the account has no passkey and no step-up phone
// left, and still adds no first factor on the password. Cleared, it does.
func TestTheStickyTierSurvivesRemovingTheFactor(t *testing.T) {
	r := newEnrolRig(t, false)
	r.fake.AddPasskey(r.subject(), "owner-cred")
	recordPasskey(t, r.s, r.fake, r.subject(), "owner-cred", true)
	if w := r.proveWith(t, r.at, r.rt, "owner-cred"); w.Code != http.StatusOK {
		t.Fatalf("the proof: %d %s", w.Code, w.Body)
	}
	pid := r.fake.Passkeys(r.subject())[0].ID
	if c := do(r.edge, http.MethodPost, accountPasskeysPath+"/"+pid+"/delete", nil, withCookies(r.at, r.rt)).Code; c != http.StatusSeeOther {
		t.Fatalf("removing the passkey: %d", c)
	}
	if _, allowed := r.mintCode(t); allowed {
		t.Fatal("an account that had a passkey, removed, pairs a step-up phone on the password")
	}
	proofOf(t, r.options())
	// The one way back (a completed reset clears the mark; the reset route
	// itself is TestAPasswordResetClearsTheStickyTier).
	r.s.clearStrongFactor(context.Background(), r.uid)
	if _, allowed := r.mintCode(t); !allowed {
		t.Fatal("control: a cleared account may not pair its first step-up phone")
	}
}

// TestAPasswordResetClearsTheStickyTier: the reset link (minted by an admin
// for the account, as the email flow's) clears the mark when the reset
// completes; nothing before that does.
func TestAPasswordResetClearsTheStickyTier(t *testing.T) {
	w := newPasskeyWorld(t)
	uid, _, _, _ := w.joinMember("sticky-reset@invite.test")
	w.s.markStrongFactor(context.Background(), uid)
	had := func() bool {
		t.Helper()
		h, err := w.s.hadStrongFactor(context.Background(), uid)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if !had() {
		t.Fatal("fixture: the account is not marked")
	}
	// A link for a marked account takes the acting admin's proof, from an
	// account that has had a factor (admin_proof.go).
	a := adminSelf(t, w)
	_ = w.s.markStrongFactor(context.Background(), a)
	grantEnrolTicket(t, w.s, w.fake, a, w.adminAT)
	rec := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(uid, 10)+"/reset-link", url.Values{}, w.asAdmin())
	m := regexp.MustCompile(regexp.QuoteMeta(invitePublic+ResetPath+"#") + `([A-Za-z0-9_-]+)`).FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		t.Fatalf("reset link: %d", rec.Code)
	}
	if !had() {
		t.Fatal("minting a reset link cleared the mark")
	}
	const newPW = "a-brand-new-password-for-the-sticky-tier"
	if r := do(w.edge, "POST", ResetPath, url.Values{"token": {m[1]}, "password": {newPW}, "password2": {newPW}}); r.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d %s", r.Code, truncate(r.Body.String(), 300))
	}
	if had() {
		t.Fatal("a completed reset left the mark")
	}
}

// TestAnAdminStrippingPasskeysTellsTheUser: an admin removing another
// account's passkeys tells that account, and marks it (it stays gated).
func TestAnAdminStrippingPasskeysTellsTheUser(t *testing.T) {
	w := newPasskeyWorld(t)
	uid, sub, _, _ := w.joinMember("stripped@invite.test")
	w.fake.AddPasskey(sub, "s1")
	rec := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(uid, 10)+"/passkeys/delete", url.Values{}, w.asAdmin())
	if rec.Code != 200 {
		t.Fatalf("strip: %d", rec.Code)
	}
	if !strings.Contains(w.logs.String(), `told user `+strconv.FormatInt(uid, 10)+` "An admin removed every passkey from your account"`) {
		t.Fatalf("the stripped account was not told:\n%s", truncate(w.logs.String(), 2000))
	}
	if had, err := w.s.hadStrongFactor(context.Background(), uid); err != nil || !had {
		t.Fatalf("a stripped account is not marked as having had a factor (%v %v)", had, err)
	}
	_ = gotruetest.FlagUPUV
}
