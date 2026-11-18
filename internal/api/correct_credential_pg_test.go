package api

import (
	"context"
	"net/http"
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

// TestPasskeySignInOptionsAreLimitedPerAddress: passkeyOptionsPerAddr unused
// sign-in challenges from one address, then the next is 429 without asking
// GoTrue; another address is unaffected; and a passkey sign-in from the address
// clears its count (failure-only: a signed-in browser is never held).
func TestPasskeySignInOptionsAreLimitedPerAddress(t *testing.T) {
	w := newPasskeyWorld(t)
	_, sub, _, _ := w.joinMember("pk-opts@invite.test")
	w.fake.AddPasskey(sub, "cred-opts")
	for i := 0; i < passkeyOptionsPerAddr; i++ {
		if rec := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{}, arriveFrom(sharedAddress)); rec.Code != http.StatusOK {
			t.Fatalf("options %d: %d", i, rec.Code)
		}
	}
	calls := w.fake.CountCalls(http.MethodPost, "/passkeys/authentication/options")
	if rec := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{}, arriveFrom(sharedAddress)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("options past the limit: %d, want 429", rec.Code)
	}
	if n := w.fake.CountCalls(http.MethodPost, "/passkeys/authentication/options"); n != calls {
		t.Fatalf("options past the limit still asked GoTrue (%d calls, was %d)", n, calls)
	}
	if rec := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{}, arriveFrom("198.51.100.150")); rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: another address: %d", rec.Code)
	}
	// Clearing: a sign-in from a fresh address that had used challenges.
	const other = "198.51.100.151"
	for i := 0; i < passkeyOptionsPerAddr-1; i++ {
		postJSON(w.edge, passkeySignInOptionsPath, map[string]any{}, arriveFrom(other))
	}
	if rec := w.passkeySignIn("cred-opts", gotruetest.FlagUPUV, arriveFrom(other)); rec.Code != http.StatusOK {
		t.Fatalf("a passkey sign-in on the last challenge: %d", rec.Code)
	}
	if rec := postJSON(w.edge, passkeySignInOptionsPath, map[string]any{}, arriveFrom(other)); rec.Code != http.StatusOK {
		t.Fatalf("options after a passkey sign-in from that address: %d, want 200 (the sign-in cleared the count)", rec.Code)
	}
}
