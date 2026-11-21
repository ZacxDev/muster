package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/users"
)

// A SERVER APP'S TASKS, KNOCK CAPS, HIDE, ROTATE, FORK 9, THE MINT ARM AND
// UNINSTALL (docs/design/meetings.md "1h tests").

func srvComment(key, body string) string {
	return `{"key":"` + srvNS + `:` + key + `","comment":"` + body + `"}`
}

// commentsOn is how many comments task id has.
func (f *srvFix) commentsOn(id int64) int {
	return f.count(`SELECT count(*) FROM note_comments WHERE note_id = $1`, id)
}

// ledgered is the task the app's ledger names for (uid, key), or 0.
func (f *srvFix) ledgered(app, uid int64, key string) int64 {
	f.t.Helper()
	var id int64
	_ = f.pool.QueryRow(f.sys(), `SELECT task_id FROM plugin_server_tasks WHERE plugin_id = $1 AND owner_id = $2 AND key = $3`,
		app, uid, srvNS+":"+key).Scan(&id)
	return id
}

// personTask files a task as uid itself, tagged srvNS:key, and returns its id.
func (f *srvFix) personTask(uid int64, key string) int64 {
	f.t.Helper()
	n, err := f.notes.Create(srvAs(uid), notes.Note{Title: "mine", Body: "the person's own", Tags: []string{srvNS + ":" + key}})
	if err != nil {
		f.t.Fatal(err)
	}
	return n.ID
}

// TestAServerAppFindsOnlyTheTasksItFiled: a task the PERSON filed with key
// meetings:x, and one ANOTHER PLUGIN filed with it, are each invisible to the
// server app. A bare comment on meetings:x answers byte for byte what a key
// no task carries answers, and appends nothing to either task. A create files
// a NEW task and ledgers it, and the app's own task then takes its comment
// (the positive control).
// Red if: the server path calls FindLiveByTag across the person's tasks, or
// answers the not-ledgered case differently from the unknown-key case.
func TestAServerAppFindsOnlyTheTasksItFiled(t *testing.T) {
	f := newSrvFix(t)
	sub := f.open(f.a, f.app.ID, f.tok)
	own := f.personTask(f.a, "x")
	form := url.Values{"name": {"srv-filer-" + f.suffix}, "label": {"Example filer"}, "capability": {"tasks"}}
	ptok, _, prec := (&handoffSeam{t: t, srv: f.srv, h: f.h, hdr: f.hdr}).create(f.a, form)
	if ptok == "" {
		t.Fatalf("minting A's tasks plugin: %d %s", prec.Code, prec.Body.String())
	}
	other := f.do("POST", pluginTasksPath, srvTask("y", "Other plugin's"), srvBearer(ptok))
	if other.Code != http.StatusCreated {
		t.Fatalf("A's other plugin files meetings:y: %d %s", other.Code, other.Body.String())
	}
	otherID := f.count(`SELECT id::int FROM notes WHERE owner_id = $1 AND tags @> ARRAY[$2]::text[] ORDER BY id DESC LIMIT 1`, f.a, srvNS+":y")
	unknown := f.actFor(f.tok, sub, "POST", pluginTasksPath, srvComment("never-filed", "hello"))
	for _, key := range []string{"x", "y"} {
		rec := f.actFor(f.tok, sub, "POST", pluginTasksPath, srvComment(key, "hello"))
		if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
			t.Errorf("a bare comment on %s (filed by someone else): %d %q; want exactly the unknown key's %d %q",
				key, rec.Code, rec.Body.String(), unknown.Code, unknown.Body.String())
		}
	}
	if want := `{"commented":false,"created":false,"id":null}`; strings.TrimSpace(unknown.Body.String()) != want {
		t.Errorf("the unknown key's answer: %s; want %s", unknown.Body.String(), want)
	}
	if f.commentsOn(own) != 0 || f.commentsOn(int64(otherID)) != 0 {
		t.Errorf("a comment reached a task the app did not file (person's %d, plugin's %d)", f.commentsOn(own), f.commentsOn(int64(otherID)))
	}
	rec := f.actFor(f.tok, sub, "POST", pluginTasksPath, srvTask("x", "The app's own"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create meetings:x: %d %s; want 201 (a NEW task)", rec.Code, rec.Body.String())
	}
	mine := f.ledgered(f.app.ID, f.a, "x")
	if mine == 0 || mine == own {
		t.Fatalf("the ledger names %d for meetings:x; want the new task, never the person's %d", mine, own)
	}
	if rec := f.actFor(f.tok, sub, "POST", pluginTasksPath, srvComment("x", "on the app's own")); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"commented":true`) {
		t.Errorf("CONTROL a comment on the app's own task: %d %s", rec.Code, rec.Body.String())
	}
	if f.commentsOn(mine) != 1 || f.commentsOn(own) != 0 {
		t.Errorf("comments: the app's task %d, the person's %d; want 1 and 0", f.commentsOn(mine), f.commentsOn(own))
	}
}

// TestReFilingRepointsTheLedger: the app's ledgered task for meetings:x is
// completed; then dismissed; then merged away (superseded-by:). After each, a
// create files a new task and the ledger row names it, and a comment lands on
// the new task, never the old one.
// Red if: the ledger uses "live" in place of openOnly (the completed case
// comments on a closed task), or inserts instead of upserting.
func TestReFilingRepointsTheLedger(t *testing.T) {
	f := newSrvFix(t)
	sub := f.open(f.a, f.app.ID, f.tok)
	if rec := f.actFor(f.tok, sub, "POST", pluginTasksPath, srvTask("x", "First")); rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	for i, close := range []string{
		`UPDATE notes SET status = 'complete' WHERE id = $1`,
		`UPDATE notes SET deleted_at = now() WHERE id = $1`,
		`UPDATE notes SET tags = array_append(tags, 'superseded-by:1') WHERE id = $1`,
	} {
		old := f.ledgered(f.app.ID, f.a, "x")
		f.exec(close, old)
		rec := f.actFor(f.tok, sub, "POST", pluginTasksPath, srvTask("x", "Again "+strconv.Itoa(i)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("step %d: create after closing %d: %d %s; want 201", i, old, rec.Code, rec.Body.String())
		}
		now := f.ledgered(f.app.ID, f.a, "x")
		if now == old || now == 0 {
			t.Fatalf("step %d: the ledger still names %d", i, now)
		}
		before := f.commentsOn(old)
		_ = f.actFor(f.tok, sub, "POST", pluginTasksPath, srvComment("x", "after "+strconv.Itoa(i)))
		if f.commentsOn(now) != 1 || f.commentsOn(old) != before {
			t.Errorf("step %d: the comment went to %d comments on the new, %d on the old (was %d)", i, f.commentsOn(now), f.commentsOn(old), before)
		}
	}
}

// TestServerTaskWritesAreCapped: the 61st write in an hour for one (app,
// person) is 429 while another person's 1st still passes; the app's 601st
// write in an hour, spread over 11 people, is 429; a repeat answered from the
// stored row spends nothing.
func TestServerTaskWritesAreCapped(t *testing.T) {
	f := newSrvFix(t)
	subA := f.open(f.a, f.app.ID, f.tok)
	subB := f.open(f.b, f.app.ID, f.tok)
	if rec := f.actFor(f.tok, subA, "POST", pluginTasksPath, srvTask("cap", "Capped")); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	// A repeat of the create spends nothing: the 60 comments below still fit.
	for i := 0; i < 5; i++ {
		if rec := f.actFor(f.tok, subA, "POST", pluginTasksPath, srvTask("cap", "Capped")); rec.Code != http.StatusOK {
			t.Fatalf("a repeat create: %d %s; want 200", rec.Code, rec.Body.String())
		}
	}
	for i := 1; i < 60; i++ {
		if rec := f.actFor(f.tok, subA, "POST", pluginTasksPath, srvComment("cap", "c"+strconv.Itoa(i))); rec.Code != http.StatusOK {
			t.Fatalf("write %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if rec := f.actFor(f.tok, subA, "POST", pluginTasksPath, srvComment("cap", "c60")); rec.Code != http.StatusTooManyRequests ||
		rec.Header().Get("Retry-After") == "" {
		t.Errorf("A's 61st write: %d (Retry-After %q); want 429 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := f.actFor(f.tok, subB, "POST", pluginTasksPath, srvTask("cap", "B's")); rec.Code != http.StatusCreated {
		t.Errorf("B's 1st write: %d; want 201", rec.Code)
	}
	// The per-app ceiling: 600 across everyone. A has 60 and B 1; nine more
	// people make 59 each (531), and one makes the 8 that reach 600.
	total := 61
	var last string
	for p := 0; total < 600; p++ {
		uid := f.person("Cap Person "+strconv.Itoa(p), "member", "")
		sub := f.open(uid, f.app.ID, f.tok)
		last = sub
		if rec := f.actFor(f.tok, sub, "POST", pluginTasksPath, srvTask("cap", "Theirs")); rec.Code != http.StatusCreated {
			t.Fatalf("person %d create: %d %s", p, rec.Code, rec.Body.String())
		}
		total++
		for i := 1; i < 59 && total < 600; i++ {
			if rec := f.actFor(f.tok, sub, "POST", pluginTasksPath, srvComment("cap", "p"+strconv.Itoa(i))); rec.Code != http.StatusOK {
				t.Fatalf("person %d write %d (app total %d): %d %s", p, i, total+1, rec.Code, rec.Body.String())
			}
			total++
		}
	}
	extra := f.person("Cap Person last", "member", "")
	subX := f.open(extra, f.app.ID, f.tok)
	if rec := f.actFor(f.tok, subX, "POST", pluginTasksPath, srvTask("cap", "601st")); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the app's 601st write (a fresh person's 1st): %d; want 429", rec.Code)
	}
	_ = last
}

// TestKnocksAreKeyedPerPerson: A's 30 knocks in an hour do not make B's knock
// 429 (A's 31st is); B cannot withdraw A's ref; and A's withdrawals of refs A
// never knocked about spend A's allowance only.
// Red if: appNotifyKey loses the owner.
func TestKnocksAreKeyedPerPerson(t *testing.T) {
	f := newSrvFix(t)
	subA := f.open(f.a, f.app.ID, f.tok)
	subB := f.open(f.b, f.app.ID, f.tok)
	for i := 0; i < appNotifyPluginBurst; i++ {
		if rec := f.knock(f.tok, subA, "a-"+strconv.Itoa(i)); rec.Code != http.StatusAccepted {
			t.Fatalf("A's knock %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if rec := f.knock(f.tok, subA, "a-over"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("A's 31st knock: %d; want 429", rec.Code)
	}
	if rec := f.knock(f.tok, subB, "b-0"); rec.Code != http.StatusAccepted {
		t.Errorf("B's knock after A's 30: %d; want 202", rec.Code)
	}
	if rec := f.knock(f.tok, subB, "a-0"); rec.Code != http.StatusAccepted {
		t.Errorf("B's knock on A's ref name: %d; want 202 (a different person's key)", rec.Code)
	}
	a0 := f.pushesTo(f.a)
	if rec := f.withdraw(f.tok, subB, "a-1"); rec.Code != http.StatusNoContent || f.pushesTo(f.a) != a0 {
		t.Errorf("B withdrawing A's ref: %d, A's pushes %d -> %d; want 204 and nothing to A", rec.Code, a0, f.pushesTo(f.a))
	}
	if rec := f.withdraw(f.tok, subA, "a-1"); rec.Code != http.StatusAccepted {
		t.Errorf("CONTROL A withdrawing A's ref: %d; want 202 (it was still out)", rec.Code)
	}
	for i := 0; i < appWithdrawGhostBurst; i++ {
		if rec := f.withdraw(f.tok, subA, "ghost-"+strconv.Itoa(i)); rec.Code != http.StatusNoContent {
			t.Fatalf("A's ghost withdrawal %d: %d", i+1, rec.Code)
		}
	}
	if rec := f.withdraw(f.tok, subA, "ghost-over"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("A's 61st ghost withdrawal: %d; want 429", rec.Code)
	}
	if rec := f.withdraw(f.tok, subB, "ghost-b"); rec.Code != http.StatusNoContent {
		t.Errorf("B's 1st ghost withdrawal after A's 60: %d; want 204", rec.Code)
	}
}

// TestTheServerAppKnockCeiling: with 21 people each under their own 30, the
// app's 600th knock in an hour passes and its 601st is 429 with Retry-After.
// Red if: the per-app ceiling is removed. The grants are written directly
// (each as a redeem would), so the case runs in seconds.
func TestTheServerAppKnockCeiling(t *testing.T) {
	f := newSrvFix(t)
	n := 0
	var subs []string
	for p := 0; p < 21; p++ {
		uid := f.person("Ceiling "+strconv.Itoa(p), "member", "")
		f.device(uid)
		sub := base64.RawURLEncoding.EncodeToString([]byte("ceiling-person" + strconv.Itoa(10+p)))
		f.exec(`INSERT INTO plugin_server_users (plugin_id, owner_id, subject, signin_at, granted_at, act_until)
			VALUES ($1, $2, $3, $4, $5, $6)`, f.app.ID, uid, sub, f.signin, f.now, f.now.Add(time.Hour))
		subs = append(subs, sub)
	}
	for _, sub := range subs[:20] {
		for i := 0; i < appNotifyPluginBurst; i++ {
			n++
			if rec := f.knock(f.tok, sub, "c"+strconv.Itoa(i)); rec.Code != http.StatusAccepted {
				t.Fatalf("knock %d: %d %s", n, rec.Code, rec.Body.String())
			}
		}
	}
	if n != appNotifyServerCeiling {
		t.Fatalf("drove %d knocks; want %d", n, appNotifyServerCeiling)
	}
	rec := f.knock(f.tok, subs[20], "c0")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("the 601st knock (person 21's first): %d (Retry-After %q); want 429 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// TestHideEndsTheGrantAndShowDoesNotRevive: Hide on a live grant makes
// act-for 404; Show then act-for with no redeem between is still 404; Show,
// a redeem, then act-for is 202 with the SAME subject; Hide by a person who
// never opened the app makes a row that can never act.
// Red if: Hide sets only hidden_at (Show revives the old grant).
func TestHideEndsTheGrantAndShowDoesNotRevive(t *testing.T) {
	f := newSrvFix(t)
	sub := f.open(f.a, f.app.ID, f.tok)
	if changed, err := f.store.SetServerAppHidden(srvAs(f.a), f.app.ID, true, f.now); err != nil || !changed {
		t.Fatalf("hide: %v %v", changed, err)
	}
	if rec := f.knock(f.tok, sub, "hide-1"); rec.Code != http.StatusNotFound {
		t.Errorf("after Hide: %d; want 404", rec.Code)
	}
	if rec, _ := f.mintAt(f.a, f.app.ID, f.signin); rec.Code != http.StatusNotFound {
		t.Errorf("a mint while hidden: %d; want 404", rec.Code)
	}
	if changed, err := f.store.SetServerAppHidden(srvAs(f.a), f.app.ID, false, f.now); err != nil || !changed {
		t.Fatalf("show: %v %v", changed, err)
	}
	if rec := f.knock(f.tok, sub, "hide-2"); rec.Code != http.StatusNotFound {
		t.Errorf("Show with no redeem: %d; want 404 (Show does not revive the grant)", rec.Code)
	}
	if again := f.open(f.a, f.app.ID, f.tok); again != sub {
		t.Errorf("the subject after Show and a redeem: %s; want the same %s", again, sub)
	}
	if rec := f.knock(f.tok, sub, "hide-3"); rec.Code != http.StatusAccepted {
		t.Errorf("Show then a redeem: %d; want 202", rec.Code)
	}
	if _, err := f.store.SetServerAppHidden(srvAs(f.b), f.app.ID, true, f.now); err != nil {
		t.Fatal(err)
	}
	var hsub string
	var signin *time.Time
	if err := f.pool.QueryRow(f.sys(), `SELECT subject, signin_at FROM plugin_server_users WHERE plugin_id = $1 AND owner_id = $2`,
		f.app.ID, f.b).Scan(&hsub, &signin); err != nil || signin != nil || !plugins.ValidSubject(hsub) {
		t.Fatalf("Hide by a person who never opened it: %q %v %v; want a row with a subject and no signin_at", hsub, signin, err)
	}
	if rec := f.knock(f.tok, hsub, "hide-4"); rec.Code != http.StatusNotFound {
		t.Errorf("the hide-only row's subject: %d; want 404", rec.Code)
	}
	f.exec(`UPDATE plugin_server_users SET hidden_at = NULL WHERE plugin_id = $1 AND owner_id = $2`, f.app.ID, f.b)
	if rec := f.knock(f.tok, hsub, "hide-5"); rec.Code != http.StatusNotFound {
		t.Errorf("the hide-only row, shown: %d; want 404 (no sign-in ever)", rec.Code)
	}
}

// TestRotateKeepsSubjects: after Rotate the old token is 401 on every route;
// a redeem with the new token returns the SAME subject, and the existing grant
// still passes act-for. Red if Rotate recreates the row.
func TestRotateKeepsSubjects(t *testing.T) {
	f := newSrvFix(t)
	sub := f.open(f.a, f.app.ID, f.tok)
	newTok, p, err := f.store.RotateServerApp(f.sys(), f.app.ID, users.Actor{UserID: f.admin, Via: authz.ViaSession})
	if err != nil || p.ID != f.app.ID {
		t.Fatalf("rotate: %v (id %d, want %d)", err, p.ID, f.app.ID)
	}
	for _, route := range goldenPluginRoutes(t) {
		method, pattern, _ := strings.Cut(route, " ")
		if strings.Contains(pattern, "/pair") {
			continue
		}
		path := strings.NewReplacer("{id}", "5", "{ref}", "m-5").Replace(pattern)
		if rec := f.actFor(f.tok, sub, method, path, `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("the old token on %s: %d; want 401", route, rec.Code)
		}
	}
	if rec := f.knock(newTok, sub, "rot-1"); rec.Code != http.StatusAccepted {
		t.Errorf("the existing grant with the new token: %d; want 202", rec.Code)
	}
	if again := f.open(f.a, f.app.ID, newTok); again != sub {
		t.Errorf("a redeem with the new token: subject %s; want the same %s", again, sub)
	}
}

// TestAPersonalAppPluginIntoAServerAppsEntryIsRefused (fork 9): with a live
// server app at entry URL X, a person's mint of a personal app plugin at X is
// refused and at Y it is allowed. Red if the check runs under the person's
// own principal (it then sees no server row, and X mints).
func TestAPersonalAppPluginIntoAServerAppsEntryIsRefused(t *testing.T) {
	f := newSrvFix(t)
	seam := &handoffSeam{t: t, srv: f.srv, h: f.h, hdr: f.hdr}
	if tok, _, rec := seam.create(f.a, appForm("srv-fork9-x-"+f.suffix, hoEntry)); tok != "" || !strings.Contains(rec.Body.String(), "already offers "+srvLabel) {
		t.Errorf("a personal app plugin at the server app's entry URL: minted=%v %d; want refused naming %q", tok != "", rec.Code, srvLabel)
	}
	if tok, _, rec := seam.create(f.a, appForm("srv-fork9-y-"+f.suffix, "https://w9.viewer.example.net/enter")); tok == "" {
		t.Errorf("CONTROL at another entry URL: %d; want it minted", rec.Code)
	}
}

// TestTheServerMintArm: a team's acting principal, a hidden app, a retired
// origin and an uninstalled app each refuse the mint 404; a person mints; a
// session with no recorded sign-in time is refused (ErrSignInUnknown, the
// handler's 401); a server code row always stores the session's own sign-in.
// Red if: the System read drops scope = 'server' (ANOTHER OWNER's personal
// app plugin becomes mintable: the case names one).
func TestTheServerMintArm(t *testing.T) {
	f := newSrvFix(t)
	team := f.person("Mint Team", "member", "team")
	in := plugins.HandoffIn{Target: plugins.HandoffTarget{App: &plugins.AppTarget{}}, Origins: f.pats, SignedInAt: f.signin}
	if _, _, err := f.store.MintHandoff(teamAs(team), f.app.ID, in, f.now); err == nil {
		t.Errorf("a team's acting principal minted a server code")
	}
	if _, _, err := f.store.MintHandoff(srvAs(f.a), f.app.ID, plugins.HandoffIn{Target: in.Target, Origins: f.pats}, f.now); err != plugins.ErrSignInUnknown {
		t.Errorf("a session with no sign-in time: %v; want ErrSignInUnknown", err)
	}
	// Another owner's PERSONAL app plugin: never the server arm's.
	seam := &handoffSeam{t: t, srv: f.srv, h: f.h, hdr: f.hdr}
	_, bid, rec := seam.create(f.b, appForm("srv-mintarm-"+f.suffix, "https://w8.viewer.example.net/enter"))
	if bid == 0 {
		t.Fatalf("minting B's app plugin: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := f.mintAt(f.a, bid, f.signin); rec.Code != http.StatusNotFound {
		t.Errorf("A minting for B's personal app plugin: %d; want 404", rec.Code)
	}
	rec, code := f.mintAt(f.a, f.app.ID, f.signin)
	if code == "" {
		t.Fatalf("CONTROL a person mints: %d %s", rec.Code, rec.Body.String())
	}
	var stored time.Time
	if err := f.pool.QueryRow(f.sys(), `SELECT signed_in_at FROM plugin_handoffs WHERE code_hash = $1`, plugins.HashToken(code)).Scan(&stored); err != nil ||
		!stored.Equal(f.signin.Truncate(time.Second)) {
		t.Errorf("the server code's signed_in_at: %v %v; want the session's %v", stored, err, f.signin.Truncate(time.Second))
	}
	other, _ := plugins.ParseOrigins("https://*.elsewhere.example.test")
	f.srv.ext.PluginOrigins = other
	if rec, _ := f.mintAt(f.a, f.app.ID, f.signin); rec.Code != http.StatusNotFound {
		t.Errorf("a retired origin: %d; want 404", rec.Code)
	}
	f.srv.ext.PluginOrigins = f.pats
	if _, err := f.store.SetServerAppHidden(srvAs(f.b), f.app.ID, true, f.now); err != nil {
		t.Fatal(err)
	}
	if rec, _ := f.mintAt(f.b, f.app.ID, f.signin); rec.Code != http.StatusNotFound {
		t.Errorf("a hidden app: %d; want 404", rec.Code)
	}
	if _, err := f.srv.uninstallServerApp(httptestRequest(), f.app.ID, users.Actor{UserID: f.admin, Via: authz.ViaSession}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := f.mintAt(f.a, f.app.ID, f.signin); rec.Code != http.StatusNotFound {
		t.Errorf("an uninstalled app: %d; want 404", rec.Code)
	}
}

// TestTheMintCapIsPerPerson: two people mint for one server app. A's 61st
// mint in a minute is 429, B's 1st still mints; A's rows older than a day are
// pruned by A's next mint and B's are not; each code row carries its minter's
// owner_id, never 0. Red if the cap or the prune stays keyed on the plugin
// row's owner (B's 1st is refused).
func TestTheMintCapIsPerPerson(t *testing.T) {
	f := newSrvFix(t)
	f.exec(`INSERT INTO plugin_handoffs (owner_id, server_plugin_id, code_hash, target, purpose, stepped_up, minted_at, expires_at, signed_in_at)
		VALUES ($1, $3, md5('old-a') || md5('old-a'), '{"app":{}}', 'app', false, $2::timestamptz, $2::timestamptz + interval '1 minute', $2::timestamptz),
		       ($4, $3, md5('old-b') || md5('old-b'), '{"app":{}}', 'app', false, $2::timestamptz, $2::timestamptz + interval '1 minute', $2::timestamptz)`,
		f.a, f.now.Add(-25*time.Hour), f.app.ID, f.b)
	for i := 0; i < plugins.HandoffMintsPerMinute; i++ {
		if rec, code := f.mintAt(f.a, f.app.ID, f.signin); code == "" {
			t.Fatalf("A's mint %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if rec, _ := f.mintAt(f.a, f.app.ID, f.signin); rec.Code != http.StatusTooManyRequests {
		t.Errorf("A's 61st mint: %d; want 429", rec.Code)
	}
	if n := f.count(`SELECT count(*) FROM plugin_handoffs WHERE owner_id = $1 AND minted_at < $2`, f.a, f.now.Add(-24*time.Hour)); n != 0 {
		t.Errorf("A's day-old row survived A's mints (%d)", n)
	}
	if n := f.count(`SELECT count(*) FROM plugin_handoffs WHERE owner_id = $1 AND minted_at < $2`, f.b, f.now.Add(-24*time.Hour)); n != 1 {
		t.Errorf("B's day-old row: %d; want 1 (A's mints prune A's rows only)", n)
	}
	if rec, code := f.mintAt(f.b, f.app.ID, f.signin); code == "" {
		t.Errorf("B's 1st mint after A's 60: %d %s; want it minted", rec.Code, rec.Body.String())
	}
	if n := f.count(`SELECT count(*) FROM plugin_handoffs WHERE server_plugin_id = $1 AND (owner_id IS NULL OR owner_id = 0 OR owner_id NOT IN ($2, $3))`, f.app.ID, f.a, f.b); n != 0 {
		t.Errorf("%d server code rows name an owner that is not their minter", n)
	}
}

// TestUninstallEndsEverything: every grant, ledger row and unspent code is
// gone, the token is 401, and each knock still out is withdrawn (the push
// sender records the resolved on its owner's device).
func TestUninstallEndsEverything(t *testing.T) {
	f := newSrvFix(t)
	subA := f.open(f.a, f.app.ID, f.tok)
	subB := f.open(f.b, f.app.ID, f.tok)
	_, _ = f.mintAt(f.a, f.app.ID, f.signin) // an unspent code
	_ = f.actFor(f.tok, subA, "POST", pluginTasksPath, srvTask("un", "Before the uninstall"))
	if rec := f.knock(f.tok, subA, "out-a"); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code)
	}
	if rec := f.knock(f.tok, subB, "out-b"); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code)
	}
	a0, b0 := f.pushesTo(f.a), f.pushesTo(f.b)
	if _, err := f.srv.uninstallServerApp(httptestRequest(), f.app.ID, users.Actor{UserID: f.admin, Via: authz.ViaSession}); err != nil {
		t.Fatal(err)
	}
	f.srv.WaitForPushes()
	if got := f.pusher.to(f.endpoint(f.a)); f.pushesTo(f.a) != a0+1 || !strings.Contains(got[len(got)-1], `"resolved"`) {
		t.Errorf("A's knock was not withdrawn: %d -> %d %v", a0, f.pushesTo(f.a), got[len(got)-1:])
	}
	if f.pushesTo(f.b) != b0+1 {
		t.Errorf("B's knock was not withdrawn: %d -> %d", b0, f.pushesTo(f.b))
	}
	for name, q := range map[string]string{
		"grants":        `SELECT count(*) FROM plugin_server_users WHERE plugin_id = $1`,
		"task ledger":   `SELECT count(*) FROM plugin_server_tasks WHERE plugin_id = $1`,
		"unspent codes": `SELECT count(*) FROM plugin_handoffs WHERE server_plugin_id = $1 AND redeemed_at IS NULL`,
		"the live row":  `SELECT count(*) FROM plugins WHERE id = $1 AND revoked_at IS NULL`,
	} {
		if n := f.count(q, f.app.ID); n != 0 {
			t.Errorf("after the uninstall, %s: %d; want 0", name, n)
		}
	}
	if n := f.tasksOf(f.a); n == 0 {
		t.Errorf("the uninstall deleted A's task; the tasks stay in each person's list")
	}
	if rec := f.actFor(f.tok, "", "GET", pluginSelfPath, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("the token after the uninstall: %d; want 401", rec.Code)
	}
}

// TestAnExpiredGrantIsNeitherLiveNorRevoked: at the store, a grant whose clock
// alone ran out is GrantExpired with no owner (so no caller can act on it),
// and act-for refuses it. A mutant reporting expired as live is killed here
// and in the gate table.
func TestAnExpiredGrantIsNeitherLiveNorRevoked(t *testing.T) {
	f := newSrvFix(t)
	sub := f.open(f.a, f.app.ID, f.tok)
	f.now = f.now.Add(plugins.ServerActFor + time.Minute)
	got, err := f.store.LiveServerGrants(f.sys(), f.app, f.pats, []string{sub}, f.now)
	if err != nil || got[sub].State != plugins.GrantExpired || got[sub].Owner != 0 {
		t.Errorf("an expired grant at the store: %+v %v; want expired with no owner", got[sub], err)
	}
	if rec := f.knock(f.tok, sub, "exp"); rec.Code != http.StatusNotFound {
		t.Errorf("act-for an expired grant: %d; want 404", rec.Code)
	}
}

// teamAs is a team's acting principal (the team viewer plugin door).
func teamAs(team int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaPlugin})
}

// httptestRequest is a request for a helper that takes one only for its
// context (an admin's uninstall).
func httptestRequest() *http.Request { return httptest.NewRequest("POST", "/admin/apps", nil) }
