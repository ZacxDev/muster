package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// THE SERVER-APP FIXTURE (core PR 1h; docs/design/meetings.md "1h tests"):
// the real plugin door, the real plugins and notes stores under row-level
// security, the real users table, and a recording push service, with two
// people (A and B) and the installing ADMIN, each with a device of their own.
// Every name is invented.
//
// 🔴 FIXTURE VALUES ARE PAIRWISE DISTINCT AND DISTINCT FROM EVERY CONSTANT an
// assertion names: the tab label shares no word with the knock bodies or any
// default; each person's display name, email and subject differ; and the
// admin's user id is neither 1 nor the app's id (srvFix asserts both), so a
// mutant pushing to installed_by, to user 1 or to the app id is visible.
const (
	srvLabel = "Basalt Room"
	srvNS    = "meetings"
)

type srvFix struct {
	t      *testing.T
	pool   *pgxpool.Pool
	srv    *Server
	h      http.Handler
	store  *plugins.PGStore
	notes  *notes.PGStore
	push   *memPush
	pusher *knockPusher
	now    time.Time
	hdr    string
	pats   []plugins.OriginPattern
	suffix string
	n      int

	a, b, admin int64
	// tok and app are the installed server app (app + tasks).
	tok string
	app plugins.Plugin
	// signin is the sign-in time every default mint's session carries: real
	// time (a password session is checked against it), whole-second aligned
	// plus 400 ms so a whole-second comparison is never accidental.
	signin time.Time

	userIDs []int64
	appIDs  []int64
}

func newSrvFix(t *testing.T) *srvFix {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &srvFix{t: t, pool: pool, hdr: "X-Server-App-Test-User",
		// t0: not on an hour boundary, and far from any real clock.
		now:    time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		suffix: strconv.FormatInt(time.Now().UnixNano(), 36),
		signin: time.Now().Add(-10 * time.Minute).Truncate(time.Second).Add(400 * time.Millisecond),
		store:  plugins.NewPG(pool), notes: notes.NewPG(pool), push: &memPush{}, pusher: &knockPusher{}}
	t.Cleanup(f.cleanup)
	if f.pats, err = plugins.ParseOrigins(hoZone); err != nil {
		t.Fatal(err)
	}
	f.admin = f.person("Ingrid Vale", "admin", "")
	f.a = f.person("Ada Quill", "member", "")
	f.b = f.person("Bram Okafor", "member", "")
	if err := notes.SetPluginTaskNamespaces([]string{srvNS}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = notes.SetPluginTaskNamespaces(nil) })
	f.srv = New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "server-app-operator-pw"}, log.New(io.Discard, "", 0))
	f.srv.UseExtensions(Extensions{Users: users.NewPG(pool), Plugins: f.store, Notes: f.notes, PluginOrigins: f.pats,
		Push: push.NewService(f.push, f.pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))})
	f.srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(f.hdr), 10, 64)
		return v
	}
	f.srv.now = func() time.Time { return f.now }
	f.h = f.srv.Handler()
	f.tok, f.app = f.install("srv-meet", true)
	if f.admin == authz.OperatorUserID || f.admin == f.app.ID {
		t.Fatalf("fixture: the admin's id %d must be neither 1 nor the app's id %d", f.admin, f.app.ID)
	}
	for _, u := range []int64{f.a, f.b, f.admin} {
		f.device(u)
	}
	return f
}

func (f *srvFix) sys() context.Context { return authz.System(context.Background()) }

func (f *srvFix) cleanup() {
	sys := f.sys()
	for _, q := range []string{
		`DELETE FROM plugin_server_tasks WHERE owner_id = ANY($1)`,
		`DELETE FROM plugin_server_users WHERE owner_id = ANY($1)`,
		`DELETE FROM plugin_handoffs WHERE owner_id = ANY($1)`,
		`DELETE FROM note_comments WHERE owner_id = ANY($1)`,
		`DELETE FROM notes WHERE owner_id = ANY($1)`,
		`DELETE FROM plugins WHERE owner_id = ANY($1)`,
		`DELETE FROM audit_log WHERE user_id = ANY($1)`,
	} {
		_, _ = f.pool.Exec(sys, q, f.userIDs)
	}
	_, _ = f.pool.Exec(sys, `DELETE FROM plugin_handoffs WHERE server_plugin_id = ANY($1)`, f.appIDs)
	_, _ = f.pool.Exec(sys, `DELETE FROM audit_log WHERE object = ANY($1)`, func() []string {
		var o []string
		for _, id := range f.appIDs {
			o = append(o, plugins.Object(id))
		}
		return o
	}())
	_, _ = f.pool.Exec(sys, `DELETE FROM plugins WHERE id = ANY($1)`, f.appIDs)
	_, _ = f.pool.Exec(sys, `DELETE FROM users WHERE id = ANY($1)`, f.userIDs)
}

// person inserts a person (role) with display name, and an email unique to
// this fixture; kind "team" makes a team instead.
func (f *srvFix) person(name, role, kind string) int64 {
	f.t.Helper()
	f.n++
	var id int64
	var err error
	if kind == "team" {
		err = f.pool.QueryRow(f.sys(), `INSERT INTO users (role, display_name, kind) VALUES ('member', $1, 'team') RETURNING id`,
			name+" "+f.suffix).Scan(&id)
	} else {
		email := strings.ToLower(strings.ReplaceAll(name, " ", ".")) + "." + f.suffix + "@mail.example.test"
		err = f.pool.QueryRow(f.sys(), `INSERT INTO users (role, display_name, email) VALUES ($1, $2, $3) RETURNING id`,
			role, name, email).Scan(&id)
	}
	if err != nil {
		f.t.Fatalf("insert %s: %v", name, err)
	}
	f.userIDs = append(f.userIDs, id)
	return id
}

// install installs a server app named name (+ this fixture's suffix).
func (f *srvFix) install(name string, tasks bool) (string, plugins.Plugin) {
	f.t.Helper()
	tok, p, err := f.store.InstallServerApp(f.sys(), plugins.ServerAppIn{Name: name + "-" + f.suffix, AppLabel: srvLabel, AppIcon: "video",
		Origin: hoZone, EntryURL: hoEntry, Origins: f.pats, Tasks: tasks}, users.Actor{UserID: f.admin, Via: authz.ViaSession})
	if err != nil {
		f.t.Fatalf("install %s: %v", name, err)
	}
	f.appIDs = append(f.appIDs, p.ID)
	return tok, p
}

// device gives uid a browser device and returns its endpoint.
func (f *srvFix) device(uid int64) string {
	f.t.Helper()
	e := ep("srv-" + strconv.FormatInt(uid, 10) + "-" + f.suffix)
	claim(f.t, f.push, uid, e)
	return e
}

func (f *srvFix) endpoint(uid int64) string {
	return ep("srv-" + strconv.FormatInt(uid, 10) + "-" + f.suffix)
}

func (f *srvFix) do(method, path, body string, set func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "muster.test"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	set(r)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

// browser is uid's browser on muster's own origin, signed in at signin.
func (f *srvFix) browser(uid int64, signin time.Time) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieName+"="+auth.SignSession(f.srv.auth.uiSessionSecret(), signin, sessionTTL))
		r.Header.Set("Origin", "http://muster.test")
		r.Header.Set(f.hdr, strconv.FormatInt(uid, 10))
	}
}

func srvBearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

// mintAt mints an app code for plugin app as uid, signed in at signin.
func (f *srvFix) mintAt(uid, app int64, signin time.Time) (*httptest.ResponseRecorder, string) {
	f.t.Helper()
	rec := f.do("POST", pluginHandoffsPath, `{"plugin":`+strconv.FormatInt(app, 10)+`,"target":{"app":{}}}`, f.browser(uid, signin))
	if rec.Code != http.StatusOK {
		return rec, ""
	}
	var out struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, codeOf(f.t, out.URL)
}

type srvRedeem struct {
	Owner struct {
		Subject     string `json:"subject"`
		DisplayName string `json:"display_name"`
	} `json:"owner"`
	Scope    string    `json:"scope"`
	ActUntil time.Time `json:"act_until"`
}

func (f *srvFix) redeem(tok, code string) (*httptest.ResponseRecorder, srvRedeem) {
	rec := f.do("POST", pluginHandoffRedeemPath, `{"code":"`+code+`"}`, srvBearer(tok))
	var out srvRedeem
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// open is uid opening app's tab: a mint at f.signin and its redeem with tok.
// It returns the subject.
func (f *srvFix) open(uid int64, app int64, tok string) string {
	f.t.Helper()
	rec, code := f.mintAt(uid, app, f.signin)
	if code == "" {
		f.t.Fatalf("user %d mint for app %d: %d %s", uid, app, rec.Code, rec.Body.String())
	}
	got, out := f.redeem(tok, code)
	if got.Code != http.StatusOK || !plugins.ValidSubject(out.Owner.Subject) {
		f.t.Fatalf("user %d redeem for app %d: %d %s", uid, app, got.Code, got.Body.String())
	}
	return out.Owner.Subject
}

// actFor calls method path with tok and, unless subject is "", the act-for
// header naming it.
func (f *srvFix) actFor(tok, subject, method, path, body string) *httptest.ResponseRecorder {
	rec := f.do(method, path, body, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok)
		if subject != "" {
			r.Header.Set(MusterActForHeader, subject)
		}
	})
	f.srv.WaitForPushes()
	return rec
}

func (f *srvFix) knock(tok, subject, ref string) *httptest.ResponseRecorder {
	return f.actFor(tok, subject, "POST", pluginNotifyPath, knockBody(ref, 2))
}

func (f *srvFix) withdraw(tok, subject, ref string) *httptest.ResponseRecorder {
	return f.actFor(tok, subject, "DELETE", pluginNotifyPath+"/"+ref, "")
}

// check is POST /plugin/v1/owners/check with tok: the states it answered.
func (f *srvFix) check(tok string, subjects ...string) (*httptest.ResponseRecorder, map[string]string) {
	body, _ := json.Marshal(map[string]any{"subjects": subjects})
	rec := f.do("POST", pluginOwnersCheckPath, string(body), srvBearer(tok))
	var out struct {
		States map[string]string `json:"states"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out.States
}

// exec runs q under System, failing the test on an error.
func (f *srvFix) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.sys(), q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

// count is a count(*) under System.
func (f *srvFix) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.sys(), q, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return n
}

// tasksOf is how many tasks uid owns.
func (f *srvFix) tasksOf(uid int64) int {
	return f.count(`SELECT count(*) FROM notes WHERE owner_id = $1`, uid)
}

// pushesTo is how many bodies uid's device was sent.
func (f *srvFix) pushesTo(uid int64) int { return len(f.pusher.to(f.endpoint(uid))) }

const srvNotFound = `{"error":"` + ServerActForNotFound + `"}`
