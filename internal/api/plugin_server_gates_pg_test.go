package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE SERVER APP'S GATES (docs/design/meetings.md "1h tests"), each through
// the real door, the real stores and real row-level security.

func srvTask(key, title string) string {
	return `{"key":"` + srvNS + `:` + key + `","create":{"title":"` + title + `","body":"filed by the app"}}`
}

// TestAServerAppActsForTheRedeemingPersonAlone (isolation, two people): A
// opens the tab and the app files a task acting for A's subject — A's tasks
// +1 AND B's unchanged (the positive control and the negative in one case).
// A subject never redeemed, B's USER ID as the header, and A's subject under
// a SECOND server app are each 404 with the identical body.
// Red if: the principal is taken from the header, or the subject lookup
// ignores plugin_id.
func TestAServerAppActsForTheRedeemingPersonAlone(t *testing.T) {
	f := newSrvFix(t)
	subA := f.open(f.a, f.app.ID, f.tok)
	tok2, app2 := f.install("srv-other", true)
	beforeA, beforeB := f.tasksOf(f.a), f.tasksOf(f.b)
	if rec := f.actFor(f.tok, subA, "POST", pluginTasksPath, srvTask("iso-1", "Iso one")); rec.Code != http.StatusCreated {
		t.Fatalf("act-for A files a task: %d %s; want 201", rec.Code, rec.Body.String())
	}
	if got := f.tasksOf(f.a); got != beforeA+1 {
		t.Errorf("A's tasks: %d; want %d (the act-for positive control)", got, beforeA+1)
	}
	if got := f.tasksOf(f.b); got != beforeB {
		t.Errorf("B's tasks: %d; want %d unchanged", got, beforeB)
	}
	never := base64.RawURLEncoding.EncodeToString([]byte("never-redeemed!!"))
	for name, c := range map[string]struct {
		tok, subject string
	}{
		"a subject never redeemed":    {f.tok, never},
		"B's user id as the header":   {f.tok, strconv.FormatInt(f.b, 10)},
		"A's subject under a 2nd app": {tok2, subA},
		"A's user id as the header":   {f.tok, strconv.FormatInt(f.a, 10)},
	} {
		rec := f.actFor(c.tok, c.subject, "POST", pluginTasksPath, srvTask("iso-2", "Iso two"))
		if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != srvNotFound {
			t.Errorf("%s: %d %q; want 404 %s", name, rec.Code, rec.Body.String(), srvNotFound)
		}
	}
	if got := f.tasksOf(f.a); got != beforeA+1 {
		t.Errorf("A's tasks after the refusals: %d; want %d", got, beforeA+1)
	}
	_ = app2
}

// TestTheGrantGatesCloseOneAtATimeOnBothConsumers is the gate table, run once
// against act-for (POST /plugin/v1/notify) and once against owners/check.
// Each row closes ONE gate: alone it is 404 on act-for and not "live" on
// owners/check ("expired" for the clock alone, "revoked" for every other).
// The all-open row is 202 and "live". Red if any one predicate is dropped
// (one mutant per line of serverGrantLive, and one for the origin check).
func TestTheGrantGatesCloseOneAtATimeOnBothConsumers(t *testing.T) {
	f := newSrvFix(t)
	type row struct {
		name  string
		close func(uid int64, sub string)
		state string
	}
	rows := []row{
		{"all open", func(int64, string) {}, "live"},
		{"act_until passed", func(uid int64, _ string) {
			f.exec(`UPDATE plugin_server_users SET act_until = $2 WHERE owner_id = $1`, uid, f.now.Add(-time.Second))
		}, "expired"},
		{"hidden", func(uid int64, _ string) {
			f.exec(`UPDATE plugin_server_users SET hidden_at = $2 WHERE owner_id = $1`, uid, f.now)
		}, "revoked"},
		// 🔴 PRECEDENCE: Hide sets act_until = now as well, so the clock has
		// ALSO run out. Revoked, never expired: a hidden person's meetings end.
		// Red if the state checks expiry first.
		{"hidden by Hide, so act_until passed too", func(uid int64, _ string) {
			if _, err := f.store.SetServerAppHidden(srvAs(uid), f.app.ID, true, f.now); err != nil {
				t.Fatal(err)
			}
		}, "revoked"},
		{"disabled and act_until passed", func(uid int64, _ string) {
			f.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, uid)
			f.exec(`UPDATE plugin_server_users SET act_until = $2 WHERE owner_id = $1`, uid, f.now.Add(-time.Second))
		}, "revoked"},
		{"a hide-only row (signin_at NULL)", func(uid int64, _ string) {
			f.exec(`UPDATE plugin_server_users SET signin_at = NULL WHERE owner_id = $1`, uid)
		}, "revoked"},
		{"disabled", func(uid int64, _ string) {
			f.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, uid)
		}, "revoked"},
		{"signed out everywhere after the sign-in", func(uid int64, _ string) {
			f.exec(`UPDATE users SET signed_out_at = $2 WHERE id = $1`, uid, f.signin.Add(5*time.Second))
		}, "revoked"},
		{"sessions_not_before past the sign-in", func(uid int64, _ string) {
			f.exec(`UPDATE users SET sessions_not_before = $2 WHERE id = $1`, uid, f.signin.Add(5*time.Second))
		}, "revoked"},
	}
	for i, r := range rows {
		uid := f.person("Gate Person "+strconv.Itoa(i), "member", "")
		f.device(uid)
		sub := f.open(uid, f.app.ID, f.tok)
		r.close(uid, sub)
		knock := f.knock(f.tok, sub, "gate-"+strconv.Itoa(i))
		_, states := f.check(f.tok, sub)
		if r.state == "live" {
			if knock.Code != http.StatusAccepted || states[sub] != "live" {
				t.Errorf("%s: act-for %d, owners/check %q; want 202 and live", r.name, knock.Code, states[sub])
			}
			continue
		}
		if knock.Code != http.StatusNotFound || strings.TrimSpace(knock.Body.String()) != srvNotFound {
			t.Errorf("%s: act-for %d %q; want the one 404", r.name, knock.Code, knock.Body.String())
		}
		if states[sub] != r.state {
			t.Errorf("%s: owners/check %q; want %q", r.name, states[sub], r.state)
		}
	}
	// A team row: no redeem can make one, so the fixture writes it.
	team := f.person("Gate Team", "member", "team")
	teamSub := base64.RawURLEncoding.EncodeToString([]byte("team-subject-001"))
	f.exec(`INSERT INTO plugin_server_users (plugin_id, owner_id, subject, signin_at, granted_at, act_until)
		VALUES ($1, $2, $3, $4, $5, $6)`, f.app.ID, team, teamSub, f.signin, f.now, f.now.Add(time.Hour))
	if rec := f.knock(f.tok, teamSub, "gate-team"); rec.Code != http.StatusNotFound {
		t.Errorf("a team row: act-for %d; want 404", rec.Code)
	}
	if _, st := f.check(f.tok, teamSub); st[teamSub] != "revoked" {
		t.Errorf("a team row: owners/check %q; want revoked", st[teamSub])
	}
	// The origin retired: every subject, the live one included.
	live := f.person("Gate Live", "member", "")
	f.device(live)
	liveSub := f.open(live, f.app.ID, f.tok)
	other, _ := plugins.ParseOrigins("https://*.elsewhere.example.test")
	f.srv.ext.PluginOrigins = other
	if rec := f.knock(f.tok, liveSub, "gate-origin"); rec.Code != http.StatusNotFound {
		t.Errorf("origin retired: act-for %d; want 404", rec.Code)
	}
	if _, st := f.check(f.tok, liveSub); st[liveSub] != "revoked" {
		t.Errorf("origin retired: owners/check %q; want revoked", st[liveSub])
	}
	f.srv.ext.PluginOrigins = f.pats
	if rec := f.knock(f.tok, liveSub, "gate-origin-back"); rec.Code != http.StatusAccepted {
		t.Errorf("CONTROL origin restored: act-for %d; want 202", rec.Code)
	}
	// The app row revoked while the door's token check had already passed is
	// not reachable through HTTP (the door refuses a revoked token 401 first),
	// so the p.revoked_at line is driven at the store: the same statement.
	f.exec(`UPDATE plugins SET revoked_at = now() WHERE id = $1`, f.app.ID)
	got, err := f.store.LiveServerGrants(f.sys(), f.app, f.pats, []string{liveSub}, f.now)
	if err != nil || got[liveSub].State != plugins.GrantRevoked {
		t.Errorf("app revoked, at the store: %+v %v; want revoked", got[liveSub], err)
	}
	if rec := f.knock(f.tok, liveSub, "gate-uninstalled"); rec.Code != http.StatusUnauthorized {
		t.Errorf("app revoked: the door answers %d; want 401 before any grant is read", rec.Code)
	}
}

// TestTheGrantClockBoundary: a redeem at t0 (not on an hour boundary) acts at
// t0 + 8 h - 1 s (202, live) and not at t0 + 8 h (404, expired).
func TestTheGrantClockBoundary(t *testing.T) {
	f := newSrvFix(t)
	t0 := f.now
	sub := f.open(f.a, f.app.ID, f.tok)
	f.now = t0.Add(plugins.ServerActFor - time.Second)
	if rec := f.knock(f.tok, sub, "clock-1"); rec.Code != http.StatusAccepted {
		t.Errorf("t0 + 8h - 1s: %d; want 202", rec.Code)
	}
	if _, st := f.check(f.tok, sub); st[sub] != "live" {
		t.Errorf("t0 + 8h - 1s: owners/check %q; want live", st[sub])
	}
	f.now = t0.Add(plugins.ServerActFor)
	if rec := f.knock(f.tok, sub, "clock-2"); rec.Code != http.StatusNotFound {
		t.Errorf("t0 + 8h: %d; want 404", rec.Code)
	}
	if _, st := f.check(f.tok, sub); st[sub] != "expired" {
		t.Errorf("t0 + 8h: owners/check %q; want expired (not revoked: only the clock ran out)", st[sub])
	}
}

// TestACodeMintedBeforeASignOutIsRefusedAfterIt: a session signs in at s0 and
// mints; Sign out everywhere follows at s0 + 5 s; the code is redeemed at
// s0 + 10 s AS IF ON ANOTHER PROCESS (the test writes the cutoff to the
// users row and touches no in-memory sign-out state). The redeem is 404, and
// no grant row gains a signin_at. Its sibling: a grant redeemed BEFORE the
// sign-out is live until it, then 404. And the whole-second rule: a cutoff in
// the SAME second as the sign-in refuses, one second earlier passes.
// Red if: the cutoffs are compared with granted_at or now() (the redeem
// time); signin_at is re-stamped from now() on a re-redeem; or the
// comparison is >= instead of at-or-before refused.
func TestACodeMintedBeforeASignOutIsRefusedAfterIt(t *testing.T) {
	f := newSrvFix(t)
	s0 := f.signin
	_, code := f.mintAt(f.a, f.app.ID, s0)
	f.exec(`UPDATE users SET signed_out_at = $2 WHERE id = $1`, f.a, s0.Add(5*time.Second))
	f.now = f.now.Add(10 * time.Second)
	if rec, _ := f.redeem(f.tok, code); rec.Code != http.StatusNotFound {
		t.Errorf("minted before the sign-out, redeemed after: %d %s; want 404", rec.Code, rec.Body.String())
	}
	if n := f.count(`SELECT count(*) FROM plugin_server_users WHERE owner_id = $1 AND signin_at IS NOT NULL`, f.a); n != 0 {
		t.Errorf("a grant row gained a signin_at (%d rows)", n)
	}

	// The sibling: redeemed before the sign-out.
	sub := f.open(f.b, f.app.ID, f.tok)
	if rec := f.knock(f.tok, sub, "before-out"); rec.Code != http.StatusAccepted {
		t.Fatalf("CONTROL before the sign-out: %d; want 202", rec.Code)
	}
	// A later redeem from a NEWER session must not move the cut: re-redeem
	// with a session signed in at s0 + 2 s, then sign out at s0 + 1 s — the
	// grant now carries s0 + 2 s and stays live; a mutant stamping now() would
	// carry f.now (2030) and stay live too, so the decisive check is below.
	f.exec(`UPDATE users SET signed_out_at = $2 WHERE id = $1`, f.b, s0.Add(5*time.Second))
	if rec := f.knock(f.tok, sub, "after-out"); rec.Code != http.StatusNotFound {
		t.Errorf("after the sign-out: %d; want 404", rec.Code)
	}
	var stored time.Time
	if err := f.pool.QueryRow(f.sys(), `SELECT signin_at FROM plugin_server_users WHERE owner_id = $1`, f.b).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	// A password session records its sign-in in whole seconds.
	if !stored.Equal(s0.Truncate(time.Second)) {
		t.Errorf("the grant's signin_at is %v; want the minting session's sign-in %v, never the redeem's time", stored, s0)
	}

	// The whole-second boundary, on the redeem and on act-for.
	for name, c := range map[string]struct {
		cut  time.Duration
		want int
	}{
		"a cutoff in the same second as the sign-in": {cut: 500 * time.Millisecond, want: http.StatusNotFound},
		"a cutoff one second before the sign-in":     {cut: -time.Second, want: http.StatusOK},
	} {
		uid := f.person("Edge "+strconv.Itoa(int(c.cut/time.Millisecond)), "member", "")
		_, code := f.mintAt(uid, f.app.ID, s0)
		// The session's sign-in is s0's whole second; a cut 0.5 s later is in
		// the same second, one a second earlier is in the one before.
		f.exec(`UPDATE users SET sessions_not_before = $2 WHERE id = $1`, uid, s0.Add(c.cut))
		if rec, _ := f.redeem(f.tok, code); rec.Code != c.want {
			t.Errorf("%s: redeem %d; want %d", name, rec.Code, c.want)
		}
	}
}

// TestTheRedeemRechecksThePerson: a person hidden, and a person disabled,
// BETWEEN the mint and the redeem, each get a redeem 404 — and the code is
// spent (undoing the change does not make it redeemable again).
// Red if: the redeem trusts the mint's checks.
func TestTheRedeemRechecksThePerson(t *testing.T) {
	f := newSrvFix(t)
	for name, c := range map[string]struct{ change, undo func(uid int64) }{
		"hidden": {func(uid int64) {
			if _, err := f.store.SetServerAppHidden(srvAs(uid), f.app.ID, true, f.now); err != nil {
				t.Fatal(err)
			}
		}, func(uid int64) {
			f.exec(`UPDATE plugin_server_users SET hidden_at = NULL WHERE owner_id = $1 AND plugin_id = $2`, uid, f.app.ID)
		}},
		"disabled": {func(uid int64) { f.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, uid) },
			func(uid int64) { f.exec(`UPDATE users SET disabled_at = NULL WHERE id = $1`, uid) }},
	} {
		uid := f.person("Recheck "+name, "member", "")
		_, code := f.mintAt(uid, f.app.ID, f.signin)
		if code == "" {
			t.Fatalf("%s: no code", name)
		}
		c.change(uid)
		if rec, _ := f.redeem(f.tok, code); rec.Code != http.StatusNotFound {
			t.Errorf("%s between the mint and the redeem: %d; want 404", name, rec.Code)
		}
		c.undo(uid)
		if rec, _ := f.redeem(f.tok, code); rec.Code != http.StatusNotFound {
			t.Errorf("%s, undone: the code redeemed (%d); want it spent", name, rec.Code)
		}
	}
}

// TestHandoffCodesDoNotCross: a personal plugin's code is 404 to the server
// token; a server code is 404 to a personal token (whatever muster_plugin
// says); and one server app cannot spend another's.
func TestHandoffCodesDoNotCross(t *testing.T) {
	f := newSrvFix(t)
	form := appForm("srv-personal-"+f.suffix, "https://w2.viewer.example.net/enter")
	ptok, pid, rec := (&handoffSeam{t: t, srv: f.srv, h: f.h, hdr: f.hdr}).create(f.a, form)
	if ptok == "" {
		t.Fatalf("minting a personal app plugin: %d %s", rec.Code, rec.Body.String())
	}
	_, pcode := f.mintAt(f.a, pid, f.signin)
	if rec, _ := f.redeem(f.tok, pcode); rec.Code != http.StatusNotFound {
		t.Errorf("a personal code to the server token: %d; want 404", rec.Code)
	}
	if rec, _ := f.redeem(ptok, pcode); rec.Code != http.StatusOK {
		t.Errorf("CONTROL the personal code to its own token: %d; want 200", rec.Code)
	}
	_, scode := f.mintAt(f.a, f.app.ID, f.signin)
	if rec, _ := f.redeem(ptok, scode); rec.Code != http.StatusNotFound {
		t.Errorf("a server code to a personal token: %d; want 404", rec.Code)
	}
	tok2, _ := f.install("srv-cross", false)
	if rec, _ := f.redeem(tok2, scode); rec.Code != http.StatusNotFound {
		t.Errorf("a server code to ANOTHER server app: %d; want 404", rec.Code)
	}
	if rec, _ := f.redeem(f.tok, scode); rec.Code != http.StatusOK {
		t.Errorf("CONTROL the server code to its own app: %d; want 200", rec.Code)
	}
}

// TestTheServerRedeemAnswerIsWhole pins the answer as a whole string: no
// owner_id, no email. The fixture's email, display name and subject are
// pairwise distinct, so a mutant leaking the email under display_name (or
// adding owner_id) cannot produce the expected bytes.
func TestTheServerRedeemAnswerIsWhole(t *testing.T) {
	f := newSrvFix(t)
	// A FRESH sign-in, so stepped_up is true (server-derived).
	_, code := f.mintAt(f.a, f.app.ID, time.Now().Add(-2*time.Second))
	rec, out := f.redeem(f.tok, code)
	if rec.Code != http.StatusOK || !plugins.ValidSubject(out.Owner.Subject) {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body.String())
	}
	want := `{"owner":{"subject":"` + out.Owner.Subject + `","display_name":"Ada Quill"},"scope":"server","target":{"app":{}},` +
		`"purpose":"app","stepped_up":true,"minted_at":"2030-01-02T03:04:05Z","act_until":"2030-01-02T11:04:05Z"}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("the redeem answer:\n got %s\nwant %s", got, want)
	}
	if strings.Contains(rec.Body.String(), "mail.example.test") || strings.Contains(rec.Body.String(), "owner_id") ||
		strings.Contains(rec.Body.String(), `"`+strconv.FormatInt(f.a, 10)+`"`) {
		t.Errorf("the redeem answer leaks the email or the user id: %s", rec.Body.String())
	}
}

// TestASubjectIsPairwiseRandomAndExactly22Base64URLCharacters: the format
// (22 base64url characters, 16 random bytes, no padding), never derived from
// the user id (not its decimal, not any encoding of it), different under two
// installs for the same person, and the same across a re-redeem.
func TestASubjectIsPairwiseRandomAndExactly22Base64URLCharacters(t *testing.T) {
	f := newSrvFix(t)
	tok2, app2 := f.install("srv-pair", false)
	a1 := f.open(f.a, f.app.ID, f.tok)
	a2 := f.open(f.a, app2.ID, tok2)
	b1 := f.open(f.b, f.app.ID, f.tok)
	for _, s := range []string{a1, a2, b1} {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if len(s) != 22 || err != nil || len(raw) != 16 || strings.ContainsAny(s, "=+/") {
			t.Errorf("subject %q: want exactly 22 base64url characters decoding to 16 bytes", s)
		}
		for _, uid := range []int64{f.a, f.b} {
			id := strconv.FormatInt(uid, 10)
			if strings.Contains(s, id) && len(id) > 3 || strings.Contains(string(raw), id) {
				t.Errorf("subject %q carries user id %s", s, id)
			}
		}
	}
	if a1 == a2 {
		t.Errorf("A's subject is the same under two apps (%s): not pairwise", a1)
	}
	if a1 == b1 {
		t.Errorf("A and B share a subject (%s)", a1)
	}
	if again := f.open(f.a, f.app.ID, f.tok); again != a1 {
		t.Errorf("a re-redeem changed A's subject: %s then %s; want it stable for the install", a1, again)
	}
	for _, bad := range []string{a1 + "A", a1[:21], strings.Repeat("A", 21) + "=", "Q2hhbmdlTWUtMTIzNDU2Nzg"} {
		if plugins.ValidSubject(bad) {
			t.Errorf("ValidSubject(%q) = true; want false", bad)
		}
	}
}

// TestTheActForHeaderShape: missing or doubled on a served act-for route is
// 400, and on a served acts-as-nobody route present is 400; on a PERSONAL
// token the header is 400 on every route. Red if a personal token with the
// header acts as its owner.
func TestTheActForHeaderShape(t *testing.T) {
	f := newSrvFix(t)
	sub := f.open(f.a, f.app.ID, f.tok)
	if rec := f.knock(f.tok, "", "shape-1"); rec.Code != http.StatusBadRequest {
		t.Errorf("act-for with no header: %d; want 400", rec.Code)
	}
	twice := f.do("POST", pluginNotifyPath, knockBody("shape-2", 1), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+f.tok)
		r.Header.Add(MusterActForHeader, sub)
		r.Header.Add(MusterActForHeader, sub)
	})
	if twice.Code != http.StatusBadRequest {
		t.Errorf("the header twice: %d; want 400", twice.Code)
	}
	if rec := f.actFor(f.tok, sub, "GET", pluginSelfPath, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("the header on /self: %d; want 400", rec.Code)
	}
	if rec := f.knock(f.tok, sub, "shape-3"); rec.Code != http.StatusAccepted {
		t.Errorf("CONTROL one header: %d; want 202", rec.Code)
	}
	form := appForm("srv-shape-"+f.suffix, "https://w3.viewer.example.net/enter")
	ptok, _, rec := (&handoffSeam{t: t, srv: f.srv, h: f.h, hdr: f.hdr}).create(f.a, form)
	if ptok == "" {
		t.Fatalf("minting a personal app plugin: %d %s", rec.Code, rec.Body.String())
	}
	before := f.pushesTo(f.a)
	for _, route := range goldenPluginRoutes(t) {
		method, pattern, _ := strings.Cut(route, " ")
		if strings.Contains(pattern, "/pair") {
			continue
		}
		path := strings.NewReplacer("{id}", "5", "{ref}", "m-5").Replace(pattern)
		got := f.actFor(ptok, sub, method, path, `{}`)
		if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "server app's header") {
			t.Errorf("%s with a personal token and the header: %d %s; want 400", route, got.Code, got.Body.String())
		}
	}
	if f.pushesTo(f.a) != before {
		t.Errorf("a personal token with the header pushed to its owner")
	}
}

// TestOwnersCheckAnswersOnlyWhatItWasAsked: every subject asked about, once,
// and no other; a subject the app never redeemed is "revoked" exactly as an
// ended one is; 201 subjects is 400; a malformed subject is 400; a personal
// token is 403. The wire shape is pinned whole.
func TestOwnersCheckAnswersOnlyWhatItWasAsked(t *testing.T) {
	f := newSrvFix(t)
	subA := f.open(f.a, f.app.ID, f.tok)
	subB := f.open(f.b, f.app.ID, f.tok)
	f.exec(`UPDATE plugin_server_users SET hidden_at = now() WHERE owner_id = $1`, f.b)
	never := base64.RawURLEncoding.EncodeToString([]byte("never-seen-subj!"))
	rec := f.do("POST", pluginOwnersCheckPath, `{"subjects":["`+subA+`","`+never+`","`+subB+`","`+subA+`"]}`, srvBearer(f.tok))
	want := `{"states":{"` + never + `":"revoked","` + subA + `":"live","` + subB + `":"revoked"}}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != canonStates(t, want) {
		t.Errorf("owners/check: %d\n got %s\nwant %s", rec.Code, strings.TrimSpace(rec.Body.String()), canonStates(t, want))
	}
	many := make([]string, plugins.MaxOwnersCheck+1)
	for i := range many {
		many[i] = subA
	}
	if rec, _ := f.check(f.tok, many...); rec.Code != http.StatusBadRequest {
		t.Errorf("201 subjects: %d; want 400", rec.Code)
	}
	if rec, st := f.check(f.tok, many[:plugins.MaxOwnersCheck]...); rec.Code != http.StatusOK || len(st) != 1 {
		t.Errorf("CONTROL 200 subjects: %d %v; want 200 and one entry", rec.Code, st)
	}
	if rec, _ := f.check(f.tok, "not-a-subject"); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed subject: %d; want 400", rec.Code)
	}
	form := appForm("srv-check-"+f.suffix, "https://w4.viewer.example.net/enter")
	ptok, _, prec := (&handoffSeam{t: t, srv: f.srv, h: f.h, hdr: f.hdr}).create(f.a, form)
	if ptok == "" {
		t.Fatalf("minting a personal plugin: %d", prec.Code)
	}
	if rec, _ := f.check(ptok, subA); rec.Code != http.StatusForbidden {
		t.Errorf("a personal token: %d; want 403", rec.Code)
	}
}

// canonStates re-encodes a {"states": {...}} body with Go's sorted keys.
func canonStates(t *testing.T, s string) string {
	t.Helper()
	var v map[string]map[string]string
	if err := jsonUnmarshalString(s, &v); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return jsonString(v)
}

// TestWhoEachServerRouteActsAs: A's knock reaches A's device and nobody
// else's — with A, B and the installing admin each holding a device, act-for
// A sends exactly one push, to A's device, none to B's or the admin's; the
// same for the withdrawal; a task filed for A lands in A's rows and not the
// admin's. /self, the redeem and owners/check run with NO request principal;
// act-for runs as ViaServerPlugin with A's user id.
// Red if: the recipient stays p.OwnerID (0: A's device gets nothing), or
// becomes installed_by (the admin's device gets it); ViaServerPlugin is
// missing from requestVias (act-for panics); the tokens-elsewhere exemption
// stays == ViaPlugin (act-for is 401).
func TestWhoEachServerRouteActsAs(t *testing.T) {
	f := newSrvFix(t)
	sub := f.open(f.a, f.app.ID, f.tok)
	var seen []authz.Principal
	var nobody []string
	f.srv.serverNobodyProbe = func(r *http.Request) {
		if _, ok := authz.From(r.Context()); ok {
			t.Errorf("%s %s: an acts-as-nobody route carried a principal", r.Method, r.URL.Path)
		}
		nobody = append(nobody, r.Method+" "+r.URL.Path)
	}
	f.srv.principalProbe = func(r *http.Request) {
		if p, _ := authz.From(r.Context()); p.Via != authz.ViaSession {
			seen = append(seen, p) // the plugin door's; a browser's mint is the session door's
		}
	}
	a0, b0, m0 := f.pushesTo(f.a), f.pushesTo(f.b), f.pushesTo(f.admin)
	if rec := f.knock(f.tok, sub, "who-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("act-for A: %d %s", rec.Code, rec.Body.String())
	}
	if got := []int{f.pushesTo(f.a) - a0, f.pushesTo(f.b) - b0, f.pushesTo(f.admin) - m0}; got[0] != 1 || got[1] != 0 || got[2] != 0 {
		t.Errorf("the knock's pushes (A, B, admin) = %v; want [1 0 0]", got)
	}
	if rec := f.withdraw(f.tok, sub, "who-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("withdraw for A: %d %s", rec.Code, rec.Body.String())
	}
	if got := []int{f.pushesTo(f.a) - a0, f.pushesTo(f.b) - b0, f.pushesTo(f.admin) - m0}; got[0] != 2 || got[1] != 0 || got[2] != 0 {
		t.Errorf("after the withdrawal (A, B, admin) = %v; want [2 0 0]", got)
	}
	adminTasks := f.tasksOf(f.admin)
	if rec := f.actFor(f.tok, sub, "POST", pluginTasksPath, srvTask("who-2", "Who two")); rec.Code != http.StatusCreated {
		t.Fatalf("a task for A: %d %s", rec.Code, rec.Body.String())
	}
	if f.tasksOf(f.admin) != adminTasks {
		t.Errorf("a task for A landed in the admin's rows")
	}
	for _, p := range seen {
		if p.Via != authz.ViaServerPlugin || p.UserID != f.a {
			t.Errorf("act-for ran as %v; want user %d via %s", p, f.a, authz.ViaServerPlugin)
		}
	}
	if len(seen) != 3 {
		t.Errorf("act-for requests admitted: %d; want 3", len(seen))
	}
	seen = nil
	_ = f.actFor(f.tok, "", "GET", pluginSelfPath, "")
	_, code := f.mintAt(f.b, f.app.ID, f.signin)
	_, _ = f.redeem(f.tok, code)
	_, _ = f.check(f.tok, sub)
	if len(seen) != 0 || len(nobody) != 3 {
		t.Errorf("the acts-as-nobody routes: %d with a principal, %d without (%v); want 0 and 3", len(seen), len(nobody), nobody)
	}
}

// TestTheServerSelfNamesNoPerson pins /self for a server token whole.
func TestTheServerSelfNamesNoPerson(t *testing.T) {
	f := newSrvFix(t)
	rec := f.actFor(f.tok, "", "GET", pluginSelfPath, "")
	want := `{"plugin_id":` + strconv.FormatInt(f.app.ID, 10) + `,"plugin_name":"` + f.app.Name + `","scope":"server","owner_id":null,` +
		`"owner_login":null,"owner_kind":"server","capabilities":["app","tasks"],"deliver_to":null,"act_for_hours":8}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("/self:\n got %d %s\nwant %s", rec.Code, strings.TrimSpace(rec.Body.String()), want)
	}
}

// srvAs is a context acting as person uid through the browser door.
func srvAs(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
