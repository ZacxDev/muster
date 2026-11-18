package api

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
)

// TestTheClaimAnswers429PastItsAddressFailureCount: loginFailureLimit junk
// claim tokens from one address, then the next junk one is 429, and a VALID
// claim link from that same address still binds and signs in.
func TestTheClaimAnswers429PastItsAddressFailureCount(t *testing.T) {
	f := newGoTrueFixture(t)
	tok := mintClaim(t, f.store)
	for i := 0; i < loginFailureLimit; i++ {
		if w := do(f.edge, http.MethodPost, ClaimPath, claimForm("junk"+strconv.Itoa(i), "op@example.com", "a-strong-password", "a-strong-password"),
			arriveFrom(sharedAddress)); w.Code != http.StatusNotFound {
			t.Fatalf("junk claim %d: %d, want the invalid page (404)", i, w.Code)
		}
	}
	if w := do(f.edge, http.MethodPost, ClaimPath, claimForm("junk-more", "op@example.com", "a-strong-password", "a-strong-password"),
		arriveFrom(sharedAddress)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a junk claim past the address's failure count: %d, want 429", w.Code)
	}
	if w := do(f.edge, http.MethodPost, ClaimPath, claimForm(tok, "op@example.com", "a-strong-password", "a-strong-password"),
		arriveFrom(sharedAddress)); w.Code != http.StatusSeeOther {
		t.Fatalf("the VALID claim from the address past its count: %d %s, want 303", w.Code, w.Body)
	}
}

// TestAWrongStepUpPastTheAddressCountIs429: gotrue mode. The address is put
// past its failure count by junk sign-ins; then a WRONG step-up confirm from
// it — by password, and by a passkey GoTrue refuses — is answered 429 with the
// throttled message, not 401.
func TestAWrongStepUpPastTheAddressCountIs429(t *testing.T) {
	r := newPasskeyStepUpRig(t)
	at, rt, _ := r.fake.Mint(r.owner)
	// CONTROL: before the flood, a wrong password confirm is 401.
	if w := r.confirm(at, rt, url.Values{"password": {"not the password"}}, arriveFrom("198.51.100.200")); w.Code != http.StatusUnauthorized {
		t.Fatalf("CONTROL: a wrong password confirm from a fresh address: %d %s, want 401", w.Code, w.Body)
	}
	for i := 0; i < loginFailureLimit; i++ {
		do(r.edge, http.MethodPost, "/login", url.Values{"email": {"nobody" + strconv.Itoa(i) + "@example.com"}, "password": {"x"}},
			arriveFrom(sharedAddress))
	}
	if w := r.confirm(at, rt, url.Values{"password": {"not the password"}}, arriveFrom(sharedAddress)); w.Code != http.StatusTooManyRequests || errorOf(w) != stepUpThrottled {
		t.Errorf("a wrong password confirm past the address count: %d %s, want 429 %q", w.Code, w.Body, stepUpThrottled)
	}
	if w := r.passkeyConfirm(t, at, rt, "a-credential-gotrue-does-not-know", gotruetest.FlagUPUV, arriveFrom(sharedAddress)); w.Code != http.StatusTooManyRequests || errorOf(w) != stepUpThrottled {
		t.Errorf("a refused passkey confirm past the address count: %d %s, want 429 %q", w.Code, w.Body, stepUpThrottled)
	}
}
