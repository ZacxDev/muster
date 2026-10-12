package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/sse"
	"github.com/ZacxDev/muster/internal/users"
)

// Accounts PR 5: link invites, the admin pages and the password reset, driven
// in gotrue mode against the in-memory GoTrue and the REAL users store
// (Postgres: the invite row lock and the last-admin lock are the mechanisms
// under test). The e2e suite drives the same flow against the real GoTrue.

const invitePublic = "https://muster.example.com"

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type invWorld struct {
	t       *testing.T
	s       *Server
	edge    http.Handler
	fake    *gotruetest.Fake
	pool    *pgxpool.Pool
	store   *users.PGStore
	logs    *syncBuf
	adminAT string
	adminRT string
}

func newInviteWorld(t *testing.T) *invWorld {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reset := func() {
		sys := authz.System(context.Background())
		for _, q := range []string{
			`DELETE FROM audit_log`,
			`DELETE FROM invites`,
			`DELETE FROM users WHERE id <> 1 AND lower(email) LIKE '%@invite.test'`,
			`UPDATE users SET gotrue_sub = NULL, email = NULL, role = 'admin', disabled_at = NULL WHERE id = 1`,
		} {
			if _, err := pool.Exec(sys, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	reset()
	t.Cleanup(reset)
	w := &invWorld{t: t, pool: pool, store: users.NewPG(pool), logs: &syncBuf{}}
	w.fake = gotruetest.New(t, gtSecret)
	w.fake.SetAccessTTL(time.Hour)
	w.s = New(sse.New(16), AuthConfig{Mode: AuthModeGoTrue, HookToken: gtHook, PublicURL: invitePublic},
		log.New(w.logs, "", 0))
	w.s.UseExtensions(Extensions{Users: w.store})
	w.s.UseGoTrue(GoTrueAuth{Client: w.fake.Client(), Verifier: w.fake.Verifier(), Users: w.store, Accounts: w.store})
	w.edge = BrowserListener(w.s.Handler())
	// User 1, the admin, bound to a GoTrue login and signed in.
	sub := w.fake.AddUser("op@invite.test", "operator-password-long")
	if _, err := pool.Exec(authz.System(ctx), `UPDATE users SET gotrue_sub = $1::uuid, email = 'op@invite.test' WHERE id = 1`, sub); err != nil {
		t.Fatal(err)
	}
	w.adminAT, w.adminRT, _ = w.fake.Mint(sub)
	return w
}

func (w *invWorld) asAdmin() reqOpt { return withCookies(w.adminAT, w.adminRT) }

var inviteLinkRE = regexp.MustCompile(regexp.QuoteMeta(invitePublic+InvitePath+"#") + `([A-Za-z0-9_-]+)`)

// createInvite drives the admin page and returns the token from the link.
func (w *invWorld) createInvite(email, role string) string {
	w.t.Helper()
	form := url.Values{"email": {email}, "role": {role}, "expires_hours": {"72"}}
	rec := do(w.edge, "POST", "/admin/invites", form, w.asAdmin())
	m := inviteLinkRE.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		w.t.Fatalf("create invite: %d, no link in %s", rec.Code, truncate(rec.Body.String(), 400))
	}
	return m[1]
}

func redeemForm(token, email, name, pw, pw2 string) url.Values {
	return url.Values{"step": {"redeem"}, "token": {token}, "email": {email}, "display_name": {name},
		"password": {pw}, "password2": {pw2}}
}

const goodPW = "a-good-long-password"

func (w *invWorld) audit(action string) []users.AuditEntry {
	w.t.Helper()
	all, err := w.store.RecentAudit(context.Background(), 200)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []users.AuditEntry
	for _, e := range all {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestAnAdminInvitesAMemberWhoRedeemsTheLinkAndIsSignedIn(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")

	// Only the sha256 is stored, and the token appears in no audit detail.
	var stored []byte
	if err := w.pool.QueryRow(authz.System(context.Background()), `SELECT token_hash FROM invites`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256([]byte(tok)); !bytes.Equal(stored, sum[:]) {
		t.Fatal("the stored hash is not sha256(token)")
	}
	if raw, _ := base64RawURLDecode(tok); len(raw) != 32 {
		t.Fatalf("the token carries %d random bytes, want 32", len(raw))
	}
	var leaked bool
	_ = w.pool.QueryRow(authz.System(context.Background()), `SELECT EXISTS (SELECT 1 FROM audit_log WHERE detail::text LIKE '%' || $1 || '%')`, tok).Scan(&leaked)
	if leaked {
		t.Fatal("the invite token was written to audit_log")
	}
	// The page lists it as pending; it never shows the link again.
	list := do(w.edge, "GET", "/admin/invites", nil, w.asAdmin())
	if !strings.Contains(list.Body.String(), `data-invite-state="pending"`) || strings.Contains(list.Body.String(), tok) {
		t.Fatalf("the invite list: pending missing or link shown again\n%s", truncate(list.Body.String(), 400))
	}

	// The invitee: the page (no token in it), the open step, the form.
	if rec := do(w.edge, "GET", InvitePath, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "location.hash") {
		t.Fatalf("invite page: %d", rec.Code)
	}
	open := do(w.edge, "POST", InvitePath, url.Values{"step": {"open"}, "token": {tok}})
	if open.Code != 200 || !strings.Contains(open.Body.String(), "Create your muster account") || strings.Contains(open.Body.String(), "readonly") {
		t.Fatalf("open: %d %s", open.Code, truncate(open.Body.String(), 300))
	}
	rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "ann@invite.test", "Ann", goodPW, goodPW))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("redeem: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	ck := setCookies(rec)
	if ck[accessCookieName] == nil || ck[refreshCookieName] == nil {
		t.Fatal("a redemption did not sign the new user in")
	}
	// The new user: a member, bound to a CONFIRMED GoTrue user.
	var uid int64
	var role, name, sub string
	if err := w.pool.QueryRow(authz.System(context.Background()),
		`SELECT id, role, display_name, gotrue_sub::text FROM users WHERE email = 'ann@invite.test'`).Scan(&uid, &role, &name, &sub); err != nil {
		t.Fatal(err)
	}
	if role != "member" || name != "Ann" {
		t.Fatalf("new user role %q name %q", role, name)
	}
	if gu, ok := w.fake.User(sub); !ok || !gu.Confirmed {
		t.Fatalf("GoTrue user %+v (ok %v) — must exist and be confirmed (email_confirm:true)", gu, ok)
	}
	// The new session acts as the new member.
	var seen authz.Principal
	w.s.principalProbe = func(r *http.Request) { seen, _ = authz.From(r.Context()) }
	if r := do(w.edge, "GET", "/account", nil, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)); r.Code != 200 {
		t.Fatalf("the new session: %d", r.Code)
	}
	if seen.UserID != uid || seen.Role != authz.RoleMember {
		t.Fatalf("principal %v, want user %d member", seen, uid)
	}
	// Audit: who created it, and who redeemed it.
	if c := w.audit("invite.create"); len(c) != 1 || c[0].UserID != 1 {
		t.Fatalf("invite.create audit rows %+v", c)
	}
	if r := w.audit("invite.redeem"); len(r) != 1 || r[0].UserID != uid {
		t.Fatalf("invite.redeem audit rows %+v", r)
	}
	// Single use.
	if again := do(w.edge, "POST", InvitePath, redeemForm(tok, "bob@invite.test", "Bob", goodPW, goodPW)); again.Code != http.StatusNotFound {
		t.Fatalf("second redemption: %d", again.Code)
	}
	if list := do(w.edge, "GET", "/admin/invites", nil, w.asAdmin()); !strings.Contains(list.Body.String(), `data-invite-state="redeemed"`) {
		t.Fatal("the redeemed invite is not listed as redeemed")
	}
}

func base64RawURLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// TestAnInviteLinkIsSingleUseUnderConcurrency races N redemptions of ONE
// link: exactly one wins, GoTrue gets exactly one user.
func TestAnInviteLinkIsSingleUseUnderConcurrency(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")
	// Hold each GoTrue create open, so without the row lock every racer would
	// pass the live check before any commits.
	w.fake.SetCreateDelay(100 * time.Millisecond)
	before := w.fake.UserCount()
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "racer"+strconv.Itoa(i)+"@invite.test", "", goodPW, goodPW),
				fromIP("198.51.100."+strconv.Itoa(10+i)))
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()
	won, lost := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusSeeOther:
			won++
		case http.StatusNotFound:
			lost++
		}
	}
	if won != 1 || lost != n-1 {
		t.Fatalf("codes %v: %d won and %d got the invalid page, want 1 and %d", codes, won, lost, n-1)
	}
	if c := w.fake.CountCalls("POST", "/admin/users"); c != 1 || w.fake.UserCount() != before+1 {
		t.Fatalf("GoTrue got %d create call(s) and %d new user(s), want 1 and 1", c, w.fake.UserCount()-before)
	}
	var rows int
	_ = w.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM users WHERE email LIKE 'racer%@invite.test'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("%d users rows from one link", rows)
	}
}

// TestEveryBadLinkGetsTheSameInvalidPage: unknown, expired, revoked and
// already-redeemed tokens are indistinguishable, and none reaches GoTrue.
func TestEveryBadLinkGetsTheSameInvalidPage(t *testing.T) {
	w := newInviteWorld(t)
	sys := authz.System(context.Background())
	expired := w.createInvite("", "member")
	if _, err := w.pool.Exec(sys, `UPDATE invites SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, claimHash(expired)); err != nil {
		t.Fatal(err)
	}
	revoked := w.createInvite("", "member")
	var rid int64
	_ = w.pool.QueryRow(sys, `SELECT id FROM invites WHERE token_hash = $1`, claimHash(revoked)).Scan(&rid)
	if rec := do(w.edge, "POST", "/admin/invites/"+strconv.FormatInt(rid, 10)+"/revoke", url.Values{}, w.asAdmin()); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", rec.Code)
	}
	redeemed := w.createInvite("", "member")
	if rec := do(w.edge, "POST", InvitePath, redeemForm(redeemed, "first@invite.test", "", goodPW, goodPW)); rec.Code != http.StatusSeeOther {
		t.Fatalf("control redemption: %d", rec.Code)
	}
	calls := len(w.fake.Calls())
	var bodies []string
	for name, tok := range map[string]string{"unknown": "no-such-token-at-all", "expired": expired, "revoked": revoked, "redeemed": redeemed} {
		for _, step := range []string{"open", "redeem"} {
			form := redeemForm(tok, "x"+name+"@invite.test", "", goodPW, goodPW)
			form.Set("step", step)
			rec := do(w.edge, "POST", InvitePath, form, fromIP("203.0.113."+strconv.Itoa(len(bodies)+1)))
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "invalid or expired") {
				t.Errorf("%s/%s: %d", name, step, rec.Code)
			}
			bodies = append(bodies, rec.Body.String())
		}
	}
	for i := range bodies {
		if bodies[i] != bodies[0] {
			t.Fatalf("the invalid page differs between cases:\n%s\n---\n%s", bodies[0], bodies[i])
		}
	}
	if got := len(w.fake.Calls()); got != calls {
		t.Fatalf("bad links reached GoTrue %d time(s)", got-calls)
	}
	// Positive control on the expiry: the same invite one minute in the future works.
	if _, err := w.pool.Exec(sys, `UPDATE invites SET expires_at = now() + interval '1 minute' WHERE token_hash = $1`, claimHash(expired)); err != nil {
		t.Fatal(err)
	}
	if rec := do(w.edge, "POST", InvitePath, redeemForm(expired, "late@invite.test", "", goodPW, goodPW)); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the unexpired invite: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
}

// TestABoundInviteRefusesAnyOtherEmail: the form locks the bound email, a
// tampered POST is the uniform page and never reaches GoTrue, and the link
// stops answering after inviteTokenFailures such failures.
func TestABoundInviteRefusesAnyOtherEmail(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("Bound@Invite.test", "member")
	open := do(w.edge, "POST", InvitePath, url.Values{"step": {"open"}, "token": {tok}})
	if !strings.Contains(open.Body.String(), `value="Bound@Invite.test"`) || !strings.Contains(open.Body.String(), "readonly") {
		t.Fatalf("the form does not lock the bound email:\n%s", truncate(open.Body.String(), 600))
	}
	creates := w.fake.CountCalls("POST", "/admin/users")
	rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "intruder@invite.test", "", goodPW, goodPW))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "invalid or expired") {
		t.Fatalf("mismatched email: %d", rec.Code)
	}
	if w.fake.CountCalls("POST", "/admin/users") != creates {
		t.Fatal("a mismatched email reached GoTrue")
	}
	// Case does not matter, and the stored email is the invite's.
	rec = do(w.edge, "POST", InvitePath, redeemForm(tok, "bound@invite.TEST", "", goodPW, goodPW))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the bound email in another case: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	var email string
	_ = w.pool.QueryRow(authz.System(context.Background()), `SELECT email FROM users WHERE lower(email) = 'bound@invite.test'`).Scan(&email)
	if email != "Bound@Invite.test" {
		t.Fatalf("stored email %q", email)
	}
}

func TestALinkStopsAnsweringAfterRepeatedFailures(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("held@invite.test", "member")
	for i := 0; i < inviteTokenFailures; i++ {
		rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "guess"+strconv.Itoa(i)+"@invite.test", "", goodPW, goodPW),
			fromIP("192.0.2."+strconv.Itoa(i+1)))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("guess %d: %d", i, rec.Code)
		}
	}
	// The right email, from a fresh address, is now refused too: the LINK is held.
	if rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "held@invite.test", "", goodPW, goodPW), fromIP("192.0.2.200")); rec.Code != http.StatusNotFound {
		t.Fatalf("after %d failures the right email: %d, want the invalid page", inviteTokenFailures, rec.Code)
	}
	// Control: one failure fewer leaves a link usable.
	tok2 := w.createInvite("held2@invite.test", "member")
	for i := 0; i < inviteTokenFailures-1; i++ {
		do(w.edge, "POST", InvitePath, redeemForm(tok2, "guess@invite.test", "", goodPW, goodPW), fromIP("192.0.2."+strconv.Itoa(50+i)))
	}
	if rec := do(w.edge, "POST", InvitePath, redeemForm(tok2, "held2@invite.test", "", goodPW, goodPW), fromIP("192.0.2.201")); rec.Code != http.StatusSeeOther {
		t.Fatalf("control (%d failures): %d", inviteTokenFailures-1, rec.Code)
	}
}

func TestRedemptionIsRateLimitedPerAddress(t *testing.T) {
	w := newInviteWorld(t)
	for i := 0; i < inviteIPLimit; i++ {
		if rec := do(w.edge, "POST", InvitePath, url.Values{"step": {"open"}, "token": {"nope" + strconv.Itoa(i)}}, fromIP("198.51.100.77")); rec.Code != http.StatusNotFound {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	if rec := do(w.edge, "POST", InvitePath, url.Values{"step": {"open"}, "token": {"nope"}}, fromIP("198.51.100.77")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d from one address: %d, want 429", inviteIPLimit+1, rec.Code)
	}
	if rec := do(w.edge, "POST", InvitePath, url.Values{"step": {"open"}, "token": {"nope"}}, fromIP("198.51.100.78")); rec.Code != http.StatusNotFound {
		t.Fatalf("another address: %d, want the ordinary invalid page", rec.Code)
	}
}

func TestAWeakOrMismatchedPasswordIsRefusedBeforeGoTrue(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")
	calls := len(w.fake.Calls())
	for name, form := range map[string]url.Values{
		"11 characters": redeemForm(tok, "weak@invite.test", "", "elevenchars", "elevenchars"),
		"mismatch":      redeemForm(tok, "weak@invite.test", "", goodPW, goodPW+"x"),
		"bad email":     redeemForm(tok, "not an email", "", goodPW, goodPW),
	} {
		rec := do(w.edge, "POST", InvitePath, form)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Create your muster account") {
			t.Errorf("%s: %d, want 400 with the form", name, rec.Code)
		}
	}
	if got := len(w.fake.Calls()); got != calls {
		t.Fatalf("refused forms reached GoTrue %d time(s)", got-calls)
	}
	// Exactly 12 is enough (the floor, not one past it).
	if rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "weak@invite.test", "", "twelve-chars", "twelve-chars")); rec.Code != http.StatusSeeOther {
		t.Fatalf("a 12-character password: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
}

// TestARedemptionWhoseRowFailsUndoesTheGoTrueUser: the GoTrue user is made,
// then the users INSERT fails (another account took the email in between), so
// the GoTrue user is deleted again and the invite stays pending.
func TestARedemptionWhoseRowFailsUndoesTheGoTrueUser(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")
	w.s.gt.Accounts = racingStore{PGStore: w.store, pool: w.pool}
	before := w.fake.UserCount()
	rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "taken@invite.test", "", goodPW, goodPW))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failed row: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	if c := w.fake.CountCalls("POST", "/admin/users"); c != 1 {
		t.Fatalf("create calls %d, want 1 (the undo must follow a real create)", c)
	}
	if w.fake.UserCount() != before {
		t.Fatalf("the GoTrue user made for the failed row was left behind (%d → %d)", before, w.fake.UserCount())
	}
	if _, err := w.store.OpenInvite(context.Background(), claimHash(tok)); err != nil {
		t.Fatalf("the invite is no longer pending after a rolled-back redemption: %v", err)
	}
}

// racingStore inserts a users row with the redeemer's email between GoTrue's
// create and the store's INSERT — the race the unique index settles.
type racingStore struct {
	*users.PGStore
	pool *pgxpool.Pool
}

func (r racingStore) RedeemInvite(ctx context.Context, h []byte, email, name string,
	create func(context.Context) (string, error), undo func(string)) (users.User, error) {
	return r.PGStore.RedeemInvite(ctx, h, email, name, func(ctx context.Context) (string, error) {
		sub, err := create(ctx)
		if err == nil {
			_, err2 := r.pool.Exec(authz.System(context.Background()),
				`INSERT INTO users (email, role, display_name) VALUES ($1, 'member', 'racer')`, email)
			if err2 != nil {
				return sub, errors.New("test setup: " + err2.Error())
			}
		}
		return sub, err
	}, undo)
}

func TestTheLastAdminCannotBeDemotedOrDisabled(t *testing.T) {
	w := newInviteWorld(t)
	for path, form := range map[string]url.Values{
		"/admin/users/1/role":    {"role": {"member"}},
		"/admin/users/1/disable": {},
	} {
		rec := do(w.edge, "POST", path, form, w.asAdmin())
		if rec.Code != http.StatusConflict {
			t.Errorf("%s on the only admin: %d, want 409", path, rec.Code)
		}
	}
	u, _ := w.store.ByID(context.Background(), 1)
	if r, _ := w.store.Role(authz.System(context.Background()), 1); r != authz.RoleAdmin || u.Disabled() {
		t.Fatalf("user 1 changed: %+v role %q", u, r)
	}
	// With a second admin, user 1 can be demoted (by itself); the other is then last.
	tok := w.createInvite("", "admin")
	if rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "second@invite.test", "", goodPW, goodPW)); rec.Code != http.StatusSeeOther {
		t.Fatalf("second admin: %d", rec.Code)
	}
	if rec := do(w.edge, "POST", "/admin/users/1/role", url.Values{"role": {"member"}}, w.asAdmin()); rec.Code != http.StatusSeeOther {
		t.Fatalf("demoting one of two admins: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	if a := w.audit("user.role"); len(a) != 1 || a[0].UserID != 1 || a[0].Detail["from"] != "admin" || a[0].Detail["to"] != "member" {
		t.Fatalf("user.role audit %+v", a)
	}
	// The demotion applies on the NEXT request (the session cache is dropped).
	if rec := do(w.edge, "GET", "/admin/users", nil, w.asAdmin(), asXHR); rec.Code != http.StatusForbidden {
		t.Fatalf("the demoted admin's next admin request: %d, want 403", rec.Code)
	}
}

// TestTwoAdminsCannotDemoteEachOtherAtOnce: the last-admin check is under a
// lock, so racing demotions of the only two admins leave one admin.
func TestTwoAdminsCannotDemoteEachOtherAtOnce(t *testing.T) {
	w := newInviteWorld(t)
	ctx := context.Background()
	var second int64
	if err := w.pool.QueryRow(authz.System(ctx), `INSERT INTO users (email, role) VALUES ('second@invite.test', 'admin') RETURNING id`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, id := range []int64{1, second} {
			wg.Add(1)
			go func(i int, id int64) {
				defer wg.Done()
				_, errs[i] = w.store.SetRole(ctx, users.Actor{UserID: 1, Via: authz.ViaSession}, id, authz.RoleMember)
			}(i, id)
		}
		wg.Wait()
		var admins int
		_ = w.pool.QueryRow(authz.System(ctx), `SELECT count(*) FROM users WHERE role = 'admin' AND disabled_at IS NULL`).Scan(&admins)
		if admins != 1 || (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("round %d: %d admins left, errors %v", round, admins, errs)
		}
		if _, err := w.pool.Exec(authz.System(ctx), `UPDATE users SET role = 'admin' WHERE id = ANY($1)`, []int64{1, second}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDisablingThroughThePageRefusesTheUserAndIsAudited(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")
	rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "dis@invite.test", "", goodPW, goodPW))
	ck := setCookies(rec)
	var uid int64
	var sub string
	_ = w.pool.QueryRow(authz.System(context.Background()), `SELECT id, gotrue_sub::text FROM users WHERE email = 'dis@invite.test'`).Scan(&uid, &sub)
	member := withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)
	if r := do(w.edge, "GET", "/account", nil, member); r.Code != 200 {
		t.Fatalf("before: %d", r.Code)
	}
	id := strconv.FormatInt(uid, 10)
	if r := do(w.edge, "POST", "/admin/users/"+id+"/disable", url.Values{}, w.asAdmin()); r.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d %s", r.Code, truncate(r.Body.String(), 300))
	}
	if r := do(w.edge, "GET", "/account", nil, member, asXHR); r.Code != http.StatusUnauthorized {
		t.Fatalf("the disabled member's next request: %d, want 401", r.Code)
	}
	if gu, _ := w.fake.User(sub); !gu.Banned {
		t.Fatal("not banned in GoTrue")
	}
	if a := w.audit("user.disable"); len(a) != 1 || a[0].UserID != 1 || a[0].Object != "user:"+id {
		t.Fatalf("user.disable audit %+v", a)
	}
	// Enable: audited, and the user may sign in again.
	if r := do(w.edge, "POST", "/admin/users/"+id+"/enable", url.Values{}, w.asAdmin()); r.Code != http.StatusSeeOther {
		t.Fatalf("enable: %d", r.Code)
	}
	if a := w.audit("user.enable"); len(a) != 1 {
		t.Fatalf("user.enable audit %+v", a)
	}
	if r := do(w.edge, "POST", "/login", url.Values{"email": {"dis@invite.test"}, "password": {goodPW}}); r.Code != http.StatusSeeOther {
		t.Fatalf("sign-in after enable: %d", r.Code)
	}
	// An admin cannot disable itself.
	if r := do(w.edge, "POST", "/admin/users/1/disable", url.Values{}, w.asAdmin()); r.Code != http.StatusConflict {
		t.Fatalf("self-disable: %d", r.Code)
	}
}

func TestTheResetLinkSetsANewPasswordOnceAndSignsIn(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")
	do(w.edge, "POST", InvitePath, redeemForm(tok, "forgot@invite.test", "", goodPW, goodPW))
	var uid int64
	var sub string
	_ = w.pool.QueryRow(authz.System(context.Background()), `SELECT id, gotrue_sub::text FROM users WHERE email = 'forgot@invite.test'`).Scan(&uid, &sub)
	rec := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(uid, 10)+"/reset-link", url.Values{}, w.asAdmin())
	m := regexp.MustCompile(regexp.QuoteMeta(invitePublic+ResetPath+"#") + `([A-Za-z0-9_-]+)`).FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil {
		t.Fatalf("reset link: %d %s", rec.Code, truncate(rec.Body.String(), 400))
	}
	if a := w.audit("user.reset_link"); len(a) != 1 || a[0].UserID != 1 {
		t.Fatalf("user.reset_link audit %+v", a)
	}
	const newPW = "a-brand-new-password"
	// A weak password comes back with the form, token still unspent.
	if r := do(w.edge, "POST", ResetPath, url.Values{"token": {m[1]}, "password": {"short"}, "password2": {"short"}}); r.Code != http.StatusBadRequest ||
		!strings.Contains(r.Body.String(), m[1]) || w.fake.CountCalls("POST", "/verify") != 0 {
		t.Fatalf("weak reset: %d (verify calls %d)", r.Code, w.fake.CountCalls("POST", "/verify"))
	}
	r := do(w.edge, "POST", ResetPath, url.Values{"token": {m[1]}, "password": {newPW}, "password2": {newPW}})
	if r.Code != http.StatusSeeOther || setCookies(r)[accessCookieName] == nil {
		t.Fatalf("reset: %d %s", r.Code, truncate(r.Body.String(), 300))
	}
	if gu, _ := w.fake.User(sub); gu.Password != newPW {
		t.Fatal("the GoTrue password did not change")
	}
	if a := w.audit("user.password_reset"); len(a) != 1 || a[0].UserID != uid {
		t.Fatalf("user.password_reset audit %+v", a)
	}
	// Single use.
	if again := do(w.edge, "POST", ResetPath, url.Values{"token": {m[1]}, "password": {"another-long-password"}, "password2": {"another-long-password"}}); again.Code != http.StatusNotFound {
		t.Fatalf("second use: %d", again.Code)
	}
}

// TestInviteAndResetTokensNeverReachTheLog: every request in both flows, plus
// hand-made path forms, with the log recording the requests (positive control).
func TestInviteAndResetTokensNeverReachTheLog(t *testing.T) {
	w := newInviteWorld(t)
	h := w.s.Handler() // through the real middleware (accessLog)
	edge := BrowserListener(h)
	rec := do(edge, "POST", "/admin/invites", url.Values{"role": {"member"}}, w.asAdmin())
	tok := inviteLinkRE.FindStringSubmatch(rec.Body.String())[1]
	do(edge, "GET", InvitePath, nil)
	do(edge, "POST", InvitePath, url.Values{"step": {"open"}, "token": {tok}})
	do(edge, "GET", InvitePath+"/"+tok, nil)
	do(edge, "GET", ResetPath+"/"+tok, nil)
	do(edge, "POST", InvitePath, redeemForm(tok, "logged@invite.test", "", goodPW, goodPW))
	logs := w.logs.String()
	if strings.Contains(logs, tok) {
		t.Fatalf("the token reached the log:\n%s", logs)
	}
	for _, want := range []string{"GET /invite 200", "POST /invite 200", "POST /invite 303", "/invite/[redacted]", "/auth/reset/[redacted]"} {
		if !strings.Contains(logs, want) {
			t.Errorf("CONTROL: the log does not show %q:\n%s", want, logs)
		}
	}
}

func TestInvitesAreServedOnlyOnTheBrowserListenerInGoTrueMode(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")
	machine := w.s.Handler()
	if rec := do(machine, "POST", InvitePath, redeemForm(tok, "m@invite.test", "", goodPW, goodPW)); rec.Code != http.StatusForbidden {
		t.Fatalf("redeem on the machine port: %d, want 403", rec.Code)
	}
	if rec := do(machine, "GET", ResetPath, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("reset page on the machine port: %d", rec.Code)
	}
	// A cross-site POST is refused.
	if rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "m@invite.test", "", goodPW, goodPW),
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site redeem: %d", rec.Code)
	}
	// Control: the same form on the browser listener works.
	if rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "m@invite.test", "", goodPW, goodPW)); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: %d", rec.Code)
	}
}

func TestTheSidebarLinksAdminOnlyForAnAdmin(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")
	ck := setCookies(do(w.edge, "POST", InvitePath, redeemForm(tok, "nav@invite.test", "", goodPW, goodPW)))
	admin := do(w.edge, "GET", "/tasks", nil, w.asAdmin()).Body.String()
	member := do(w.edge, "GET", "/tasks", nil, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value)).Body.String()
	if !strings.Contains(admin, `id="sidebar-admin"`) || !strings.Contains(admin, `id="sidebar-account"`) {
		t.Fatal("the admin's sidebar has no Admin/Account link")
	}
	if strings.Contains(member, `id="sidebar-admin"`) || !strings.Contains(member, `id="sidebar-account"`) {
		t.Fatal("the member's sidebar: Admin shown or Account missing")
	}
	if rec := do(w.edge, "GET", "/admin/users", nil, withCookies(ck[accessCookieName].Value, ck[refreshCookieName].Value), asNavigation); rec.Code != http.StatusForbidden {
		t.Fatalf("a member opening /admin/users: %d, want 403", rec.Code)
	}
}

// TestAdminWritesRefuseACrossSitePost: requireAdmin's same-origin check, with
// the same request from muster's own origin as the control.
func TestAdminWritesRefuseACrossSitePost(t *testing.T) {
	w := newInviteWorld(t)
	evil := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }
	if rec := do(w.edge, "POST", "/admin/invites", url.Values{"role": {"member"}}, w.asAdmin(), evil); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site create: %d", rec.Code)
	}
	var n int
	_ = w.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM invites`).Scan(&n)
	if n != 0 {
		t.Fatalf("a cross-site POST created %d invite(s)", n)
	}
	w.createInvite("", "member") // control: same-origin works
}

// TestAnAdminCannotDisableItself — with a SECOND admin present, so the
// last-admin guard is not what refuses it.
func TestAnAdminCannotDisableItself(t *testing.T) {
	w := newInviteWorld(t)
	if _, err := w.pool.Exec(authz.System(context.Background()), `INSERT INTO users (email, role) VALUES ('other-admin@invite.test', 'admin')`); err != nil {
		t.Fatal(err)
	}
	rec := do(w.edge, "POST", "/admin/users/1/disable", url.Values{}, w.asAdmin())
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "your own account") {
		t.Fatalf("self-disable with another admin present: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
}

// TestARedemptionWithATakenEmailIsTheUniformPage: an unbound invite redeemed
// with an email a muster account already has never reaches GoTrue.
func TestARedemptionWithATakenEmailIsTheUniformPage(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")
	creates := w.fake.CountCalls("POST", "/admin/users")
	rec := do(w.edge, "POST", InvitePath, redeemForm(tok, "OP@invite.test", "", goodPW, goodPW))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "invalid or expired") {
		t.Fatalf("taken email: %d", rec.Code)
	}
	if w.fake.CountCalls("POST", "/admin/users") != creates {
		t.Fatal("a taken email reached GoTrue")
	}
}

// TestAResetLinkForADisabledUserSetsNothing.
func TestAResetLinkForADisabledUserSetsNothing(t *testing.T) {
	w := newInviteWorld(t)
	tok := w.createInvite("", "member")
	do(w.edge, "POST", InvitePath, redeemForm(tok, "gone@invite.test", "", goodPW, goodPW))
	var uid int64
	var sub string
	_ = w.pool.QueryRow(authz.System(context.Background()), `SELECT id, gotrue_sub::text FROM users WHERE email = 'gone@invite.test'`).Scan(&uid, &sub)
	hash := w.fake.RecoveryHash(sub) // minted before the disable, as a link handed out earlier would be
	if rec := do(w.edge, "POST", "/admin/users/"+strconv.FormatInt(uid, 10)+"/disable", url.Values{}, w.asAdmin()); rec.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d", rec.Code)
	}
	// The fake (like GoTrue) refuses a banned user's token; un-ban in GoTrue
	// only, so muster's own disabled_at check is the one under test.
	if err := w.fake.Client().AdminSetBanned(context.Background(), "127.0.0.1", sub, false); err != nil {
		t.Fatal(err)
	}
	rec := do(w.edge, "POST", ResetPath, url.Values{"token": {hash}, "password": {"a-new-long-password"}, "password2": {"a-new-long-password"}})
	if rec.Code != http.StatusNotFound || len(setCookies(rec)) != 0 {
		t.Fatalf("reset for a disabled user: %d cookies %v", rec.Code, setCookies(rec))
	}
	if gu, _ := w.fake.User(sub); gu.Password != goodPW {
		t.Fatal("a disabled user's password was changed")
	}
}
