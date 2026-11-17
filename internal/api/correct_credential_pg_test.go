package api

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
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
