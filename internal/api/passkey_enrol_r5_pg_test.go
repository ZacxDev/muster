package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/users"
)

// ROUND 5 (admin_proof.go, passkey_enrol.go): the admin pages cannot launder a
// password into a factor, a GoTrue /settings blip does not open an account,
// a passkey sign-in marks its account, and a removal whose mark cannot be
// written is refused. Red at the previous head.

const adminProofRefusal = "This needs you to confirm" // the page escapes the apostrophe that follows

// adminSelf is the invite world's admin: its id and its session's claims.
func adminSelf(t *testing.T, w *invWorld) int64 {
	t.Helper()
	c, err := w.fake.Verifier().Verify(w.adminAT)
	if err != nil {
		t.Fatal(err)
	}
	u, err := w.store.BySub(context.Background(), c.Subject)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// TestASockPuppetAdminCannotWalkAroundTheStickyTier: 🔴 with only admin A's
// password — A marked as having had a factor — invite B, promote B to admin,
// have B mint a reset link for A (whose completion clears A's mark). The
// promotion is refused without A's proof; and even promoted (with the
// proof), B, an admin that never had a factor, cannot mint a link for A.
func TestASockPuppetAdminCannotWalkAroundTheStickyTier(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	if err := w.s.markStrongFactor(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b, _, bat, brt := w.joinMember("puppet@invite.test")
	promote := func() *http.Response {
		rec := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(b, 10)+"/role", url.Values{"role": {"admin"}}, w.asAdmin())
		return rec.Result()
	}
	if res := promote(); res.StatusCode != http.StatusForbidden {
		t.Fatalf("promoting B with A's password alone: %d, want 403", res.StatusCode)
	}
	if role := w.roleOf(t, b); role != "member" {
		t.Fatalf("B after a refused promotion: %s", role)
	}
	// With A's proof the promotion goes through (the control)…
	grantEnrolTicket(t, w.s, w.fake, a, w.adminAT)
	if res := promote(); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("control: promoting B with A's proof: %d", res.StatusCode)
	}
	// …and B, an admin that never had a factor, still cannot reset A.
	rec := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(a, 10)+"/reset-link", url.Values{}, withCookies(bat, brt))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Your own account has no passkey or phone") {
		t.Fatalf("an unmarked admin minting a reset link for a marked one: %d, want 403", rec.Code)
	}
}

// roleOf reads user id's role.
func (w *invWorld) roleOf(t *testing.T, id int64) string {
	t.Helper()
	var role string
	if err := w.pool.QueryRow(authz.System(context.Background()), `SELECT role FROM users WHERE id = $1`, id).Scan(&role); err != nil {
		t.Fatal(err)
	}
	return role
}

// TestAResetLinkForAMarkedAccountTakesTheAdminsProof: minting a reset link
// for an account that has had a factor takes the acting admin's proof; the
// account is told when the link is minted and when it is used.
func TestAResetLinkForAMarkedAccountTakesTheAdminsProof(t *testing.T) {
	w := newPasskeyWorld(t)
	a := adminSelf(t, w)
	_ = w.s.markStrongFactor(context.Background(), a)
	c, _, _, _ := w.joinMember("marked-target@invite.test")
	_ = w.s.markStrongFactor(context.Background(), c)
	mint := func() *http.Response {
		return do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(c, 10)+"/reset-link", url.Values{}, w.asAdmin()).Result()
	}
	rec := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(c, 10)+"/reset-link", url.Values{}, w.asAdmin())
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), adminProofRefusal) {
		t.Fatalf("a reset link for a marked account without the proof: %d, want 403", rec.Code)
	}
	_ = mint
	grantEnrolTicket(t, w.s, w.fake, a, w.adminAT)
	rec = do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(c, 10)+"/reset-link", url.Values{}, w.asAdmin())
	m := regexp.MustCompile(regexp.QuoteMeta(invitePublic+ResetPath+"#") + `([A-Za-z0-9_-]+)`).FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		t.Fatalf("control: the reset link with the proof: %d", rec.Code)
	}
	told := func(title string) bool {
		return strings.Contains(w.logs.String(), `told user `+strconv.FormatInt(c, 10)+` "`+title+`"`)
	}
	if !told("An admin created a password reset link for your account") {
		t.Fatal("the account was not told a reset link was minted for it")
	}
	const pw = "a-new-password-after-the-proven-link"
	if r := do(w.edge, "POST", ResetPath, url.Values{"token": {m[1]}, "password": {pw}, "password2": {pw}}); r.Code != http.StatusSeeOther {
		t.Fatalf("the reset: %d", r.Code)
	}
	if !told("Your account's password was reset") {
		t.Fatal("the account was not told its reset link was used")
	}
}

// TestASettingsBlipDoesNotOpenAPasskeyOnlyAccount: GoTrue's /settings failing
// reads as "passkeys off"; an account whose only factor is a passkey (never
// marked) must still be gated: its passkeys are listed through the admin API
// whatever /settings says.
func TestASettingsBlipDoesNotOpenAPasskeyOnlyAccount(t *testing.T) {
	r := newEnrolRig(t, false)
	r.fake.AddPasskey(r.subject(), "only-cred")
	r.s.gt.pk.mu.Lock()
	r.s.gt.pk.at = time.Time{}
	r.s.gt.pk.mu.Unlock()
	r.fake.FailSettings(50)
	code, allowed := r.mintCode(t)
	r.fake.FailSettings(0)
	if allowed {
		t.Fatal("a /settings failure opened a passkey-only account's pairing")
	}
	if res := r.redeemKeyed(t, code); res.code != http.StatusForbidden {
		t.Fatalf("a step-up phone paired on a passkey-only account during a /settings failure: %d %s", res.code, res.body)
	}
}

// TestAPasskeySignInMarksTheAccount: signing in with a passkey marks the
// account as having had a factor (the backfill SQL cannot do for passkeys).
func TestAPasskeySignInMarksTheAccount(t *testing.T) {
	w := newPasskeyWorld(t)
	uid, sub, _, _ := w.joinMember("signs-in@invite.test")
	w.fake.AddPasskey(sub, "signin-cred")
	if had, _ := w.s.hadStrongFactor(context.Background(), uid); had {
		t.Fatal("fixture: marked before signing in")
	}
	if rec := w.passkeySignIn("signin-cred", gotruetest.FlagUPUV); rec.Code != http.StatusOK {
		t.Fatalf("passkey sign-in: %d %s", rec.Code, rec.Body)
	}
	if had, err := w.s.hadStrongFactor(context.Background(), uid); err != nil || !had {
		t.Fatalf("a passkey sign-in did not mark the account (%v %v)", had, err)
	}
}

// failingMarkStore is the users store whose MarkStrongFactor fails.
type failingMarkStore struct {
	users.Store
	pg *users.PGStore
}

func (f failingMarkStore) HasStepUpPhone(ctx context.Context, id int64) (bool, error) {
	return f.pg.HasStepUpPhone(ctx, id)
}
func (f failingMarkStore) HadStrongFactor(ctx context.Context, id int64) (bool, error) {
	return f.pg.HadStrongFactor(ctx, id)
}
func (f failingMarkStore) MarkStrongFactor(context.Context, int64) error {
	return errors.New("injected mark failure")
}
func (f failingMarkStore) ClearStrongFactor(ctx context.Context, id int64) error {
	return f.pg.ClearStrongFactor(ctx, id)
}

// TestARemovalWhoseMarkCannotBeWrittenIsRefused: removing a passkey (or a
// step-up phone) first marks the account; when that write fails the removal is
// refused and nothing is removed (fail closed).
func TestARemovalWhoseMarkCannotBeWrittenIsRefused(t *testing.T) {
	r := newEnrolRig(t, false)
	r.fake.AddPasskey(r.subject(), "owner-cred")
	recordPasskey(t, r.s, r.fake, r.subject(), "owner-cred", true)
	if w := r.proveWith(t, r.at, r.rt, "owner-cred"); w.Code != http.StatusOK {
		t.Fatalf("the proof: %d %s", w.Code, w.Body)
	}
	pg := users.NewPG(r.pool)
	r.s.ext.Users = failingMarkStore{Store: r.s.ext.Users, pg: pg}
	pid := r.fake.Passkeys(r.subject())[0].ID
	if c := do(r.edge, http.MethodPost, accountPasskeysPath+"/"+pid+"/delete", nil, withCookies(r.at, r.rt)).Code; c != http.StatusServiceUnavailable {
		t.Fatalf("a passkey removal whose mark failed: %d, want 503", c)
	}
	if n := r.passkeys(); n != 1 {
		t.Fatal("a removal whose mark failed removed the passkey")
	}
}
