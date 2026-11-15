package api

import (
	"context"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/users"
)

// TEAM HOSTS FOR VIEWERS, end to end (team sharing 2b.3): a REAL mirror
// plugin, two REAL muster-mirror clients — A's own build-01 and the team's
// build-01, one host name, two owners — and a real Postgres. Who sees which
// host in the sidebar, who opens which page, which routes refuse a team
// session, and that a member's open streams follow their membership.
//
// Synthetic fixtures: team "view-<n>" with A (manager), B (operator), cal
// (viewer) and dee (not a member). Each host's one session is named with its
// own canary, so a body says which host it came from.

// viewBackend plays tmux for one session whose screen prints its name.
type viewBackend struct{ session string }

func (b *viewBackend) List(context.Context) ([]string, error) { return []string{b.session}, nil }
func (b *viewBackend) Attach(context.Context, string, bool, uint16, uint16) (mirrorhost.Terminal, error) {
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("screen of " + b.session + "\r\n")) }()
	return &sentinelTerm{r: pr, w: pw}, nil
}

// teamView is the fixture: the world, its server, its people and its hosts.
type teamView struct {
	w            *isoWorld
	ts           *httptest.Server
	svc          *mirror.Service
	team         int64
	cal, dee     int64
	own, shared  string // the sessions of A's own build-01 and of the team's
	store        *users.PGStore
	admin        users.Actor
	teamPath     string // the team host's terminal page
	teamQuery    url.Values
	ownTermQuery url.Values
}

func newTeamView(t *testing.T) *teamView {
	t.Helper()
	w := newIsoWorld(t)
	v := &teamView{w: w, store: users.NewPG(w.pool), admin: users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}}
	w.srv.ext.Users = v.store
	setTeamLiveEvery(w.srv, 100*time.Millisecond)
	mk := func(name string) int64 {
		var id int64
		if err := w.pool.QueryRow(w.sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+"-"+w.suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	v.cal, v.dee = mk("view-cal"), mk("view-dee")
	tm, err := v.store.CreateTeam(w.sys, v.admin, "view-"+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	v.team = tm.ID
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM mirror_tokens WHERE owner_id = $1`, `DELETE FROM team_members WHERE team_id = $1`,
			`DELETE FROM audit_log WHERE team_id = $1 OR object = 'team:' || $1::text`, `DELETE FROM users WHERE id = $1`} {
			_, _ = w.pool.Exec(bg, q, v.team)
		}
		_, _ = w.pool.Exec(bg, `DELETE FROM mirror_tokens WHERE owner_id = $1`, w.a.uid)
		for _, id := range []int64{v.cal, v.dee} {
			_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE user_id = $1`, id)
			_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
		}
	})
	for id, role := range map[int64]authz.TeamRole{w.b.uid: authz.TeamOperator, v.cal: authz.TeamViewer} {
		if err := v.store.SetTeamMember(w.sys, v.admin, v.team, id, role); err != nil {
			t.Fatal(err)
		}
	}
	tokens := mirror.NewPG(w.pool)
	v.svc = mirror.NewService(tokens, log.New(io.Discard, "", 0))
	w.srv.ext.Mirror, w.srv.ext.MirrorTokens = v.svc, tokens
	v.ts = httptest.NewServer(w.h)
	t.Cleanup(v.ts.Close)

	v.own, v.shared = "own"+w.suffix, "shared"+w.suffix
	ownTok, _, err := tokens.CreateToken(authz.WithPrincipal(context.Background(), authz.Principal{UserID: w.a.uid, Role: authz.RoleMember, Via: authz.ViaSession}), "build-01")
	if err != nil {
		t.Fatal(err)
	}
	teamTok, _, err := tokens.CreateTeamToken(w.sys, v.admin, v.team, "build-01")
	if err != nil {
		t.Fatal(err)
	}
	v.connect(t, ownTok, v.own, w.a.uid)
	v.connect(t, teamTok, v.shared, v.team)
	v.teamQuery = url.Values{"host": {"build-01"}, "session": {v.shared}, "owner": {strconv.FormatInt(v.team, 10)}}
	v.ownTermQuery = url.Values{"host": {"build-01"}, "session": {v.own}}
	v.teamPath = "/mirror/terminal?" + v.teamQuery.Encode()
	return v
}

// connect runs a real muster-mirror for owner's build-01 listing session.
func (v *teamView) connect(t *testing.T, token, session string, owner int64) {
	t.Helper()
	c, err := mirrorhost.New(mirrorhost.Config{URL: v.ts.URL, Token: token, Host: "build-01", Backend: &viewBackend{session: session},
		AllowHTTP: true, Refresh: 30 * time.Millisecond, ViewPoll: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !v.svc.Hub().Online(owner, "build-01", session) {
		if time.Now().After(deadline) {
			t.Fatalf("build-01 never came online for owner %d", owner)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (v *teamView) get(path string, uid int64) *httptest.ResponseRecorder {
	return v.w.do(v.w.h, "GET", path, "", cred{sessionUser: uid})
}

// TestATeamHostIsWatchedByItsMembersAndNoOneElse: with A's own build-01 and
// the team's build-01 both online,
//
//   - the sidebar lists the team's session to each member (cal, B, A) and to
//     no one else (dee), and A's own session to A alone; a team row links
//     owner=;
//   - the team host's terminal page opens for each member, at owner=, and is
//     404 for dee — and for a member who leaves owner= out (no own build-01
//     lists it); A's own page still opens without owner=, and never shows the
//     team's session;
//   - the model-backed extras answer 403 on a team session at every role
//     (viewer, operator, manager), 404 to dee;
//   - typing — a send, live keys — answers 403 to a member, 404 to dee.
//
// 🔴 RED at the base (fcd4bbfd): Hosts was the only listing and owner= was
// read nowhere, so no member saw the team's session and its page was 404.
func TestATeamHostIsWatchedByItsMembersAndNoOneElse(t *testing.T) {
	v := newTeamView(t)
	w := v.w
	for _, c := range []struct {
		who            string
		uid            int64
		shared, ownSaw bool
	}{{"cal (viewer)", v.cal, true, false}, {"B (operator)", w.b.uid, true, false}, {"A (manager, owns build-01)", w.a.uid, true, true}, {"dee (no member)", v.dee, false, false}} {
		body := v.get("/ui/nav/agents", c.uid).Body.String()
		if got := strings.Contains(body, v.shared); got != c.shared {
			t.Errorf("%s: the sidebar lists the team's session: %v; want %v\n%s", c.who, got, c.shared, truncate(body, 600))
		}
		if got := strings.Contains(body, v.own); got != c.ownSaw {
			t.Errorf("%s: the sidebar lists A's own session: %v; want %v", c.who, got, c.ownSaw)
		}
		if c.shared && !strings.Contains(body, "owner="+strconv.FormatInt(v.team, 10)) {
			t.Errorf("%s: the team's row does not link owner=:\n%s", c.who, truncate(body, 600))
		}
		rec := v.get(v.teamPath, c.uid)
		if want := map[bool]int{true: http.StatusOK, false: http.StatusNotFound}[c.shared]; rec.Code != want {
			t.Errorf("%s: GET the team host's terminal page: %d; want %d", c.who, rec.Code, want)
		}
		// (A's own session is in her switcher at the page's foot, by design.)
		if c.shared && !c.ownSaw && strings.Contains(rec.Body.String(), v.own) {
			t.Errorf("%s: the team host's page carries A's own session", c.who)
		}
	}
	noOwner := url.Values{"host": {"build-01"}, "session": {v.shared}}
	if rec := v.get("/mirror/terminal?"+noOwner.Encode(), v.cal); rec.Code != http.StatusNotFound {
		t.Errorf("cal without owner=: %d; want 404 (her own build-01 does not exist)", rec.Code)
	}
	// A's own page is her own host's: its socket names her session and no
	// owner (the switcher at its foot lists the team's session too, by design).
	ownWS := html.EscapeString("/mirror/terminal/ws?" + url.Values{"host": {"build-01"}, "session": {v.own}}.Encode())
	if rec := v.get("/mirror/terminal?"+v.ownTermQuery.Encode(), w.a.uid); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ownWS) {
		t.Errorf("CONTROL: A's own terminal page: %d; want 200 with its socket at %s", rec.Code, ownWS)
	}
	teamWS := html.EscapeString("/mirror/terminal/ws?" + v.teamQuery.Encode())
	// 🔴 A, who has a build-01 of her OWN, opening the TEAM's build-01 gets the
	// team's entry — its socket names the team and its session — never her
	// own host of that name (#690 round 1, 🟡5).
	for who, uid := range map[string]int64{"cal": v.cal, "A (her own build-01 too)": w.a.uid} {
		if body := v.get(v.teamPath, uid).Body.String(); !strings.Contains(body, teamWS) {
			t.Errorf("%s: the team host's page does not open its socket at %s", who, teamWS)
		}
	}

	extra := []string{"/mirror/view/chief", "/mirror/view/chief/log", "/mirror/view/proposal", "/mirror/view/successor", "/mirror/view/suggest"}
	for _, p := range extra {
		for who, uid := range map[string]int64{"cal": v.cal, "B": w.b.uid, "A": w.a.uid} {
			if rec := v.get(p+"?"+v.teamQuery.Encode(), uid); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), MirrorTeamExtraRefusal) {
				t.Errorf("%s GET %s on the team session: %d %q; want 403 %q", who, p, rec.Code, truncate(rec.Body.String(), 120), MirrorTeamExtraRefusal)
			}
		}
		if rec := v.get(p+"?"+v.teamQuery.Encode(), v.dee); rec.Code != http.StatusNotFound {
			t.Errorf("dee GET %s on the team session: %d; want 404", p, rec.Code)
		}
	}
	// A send naming the team in its URL is refused at the gate to a VIEWER
	// (403; team sharing 2b.4 lets an operator through to the handler's own
	// gates) and to anyone else (404); one naming it only in its body is
	// refused by the handler's own entry lookup (404) — and never reaches B's
	// own host of the same name either way: B has none.
	sendPath := "/mirror/terminal/send?owner=" + strconv.FormatInt(v.team, 10)
	body := url.Values{"host": {"build-01"}, "session": {v.shared}, "text": {"hello"}}
	if rec := w.do(w.h, "POST", sendPath, body.Encode(), cred{sessionUser: v.cal}); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), MirrorTeamViewerRefusal) {
		t.Errorf("cal's (viewer) send into the team session: %d %q; want 403 %q", rec.Code, truncate(rec.Body.String(), 120), MirrorTeamViewerRefusal)
	}
	if rec := w.do(w.h, "POST", sendPath, body.Encode(), cred{sessionUser: w.b.uid}); rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound {
		t.Errorf("B's (operator) send into the team session was refused (%d %q); want it past the gate, to the send's own gates",
			rec.Code, truncate(rec.Body.String(), 120))
	} else {
		t.Logf("B's (operator) send into the team session: %d %q", rec.Code, truncate(rec.Body.String(), 120))
	}
	if rec := w.do(w.h, "POST", sendPath, body.Encode(), cred{sessionUser: v.dee}); rec.Code != http.StatusNotFound {
		t.Errorf("dee's send into the team session: %d; want 404", rec.Code)
	}
	body.Set("owner", strconv.FormatInt(v.team, 10))
	if rec := w.do(w.h, "POST", "/mirror/terminal/send", body.Encode(), cred{sessionUser: w.b.uid}); rec.Code != http.StatusNotFound {
		t.Errorf("B's send naming the team in its body: %d; want 404", rec.Code)
	}
	// 🔴 The case that needs the entry lookup's own owner check: A HAS a
	// build-01 of her own, listing her own session, so an owner= the gate did
	// not aim must not fall back to it.
	own := url.Values{"host": {"build-01"}, "session": {v.own}, "owner": {strconv.FormatInt(v.team, 10)}, "text": {"hello"}}
	if rec := w.do(w.h, "POST", "/mirror/terminal/send", own.Encode(), cred{sessionUser: w.a.uid}); rec.Code != http.StatusNotFound {
		t.Errorf("A's send naming the team in its body, at her own session's address: %d; want 404", rec.Code)
	}
	if rec := v.get("/mirror/terminal/ws?mode=rw&"+v.teamQuery.Encode(), v.cal); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), MirrorTeamViewerRefusal) {
		t.Errorf("cal's (viewer) live keys on the team session: %d %q; want 403 %q", rec.Code, truncate(rec.Body.String(), 120), MirrorTeamViewerRefusal)
	}
}

// openWS dials the terminal socket at query as uid and returns a reader of
// its frames and a channel closed when the socket ends.
func (v *teamView) openWS(t *testing.T, query url.Values, uid int64) (screen func() string, ended <-chan struct{}) {
	t.Helper()
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(v.w.srv))
	h.Set(isoUserHeader, strconv.FormatInt(uid, 10))
	h.Set("Origin", v.ts.URL)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(v.ts.URL, "http")+"/mirror/terminal/ws?"+query.Encode(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial the terminal as %d: %v (%d)", uid, err, code)
	}
	t.Cleanup(func() { c.CloseNow() })
	var mu sync.Mutex
	var buf strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			buf.Write(b)
			mu.Unlock()
		}
	}()
	return func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }, done
}

// openEvents opens /events as uid and returns its text so far and a channel
// closed when the stream ends.
func (v *teamView) openEvents(t *testing.T, uid int64) (*isoStream, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", v.ts.URL+"/events", nil)
	req.Header.Set("Cookie", sessionCookieFor(v.w.srv))
	req.Header.Set(isoUserHeader, strconv.FormatInt(uid, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("open /events as %d: %v %v", uid, err, resp)
	}
	st := &isoStream{}
	done := make(chan struct{})
	go func() { defer close(done); st.read(resp.Body) }()
	t.Cleanup(func() { _ = resp.Body.Close() })
	return st, done
}

// TestATeamsEventsReachItsMembersStreams: an event the TEAM owns (its
// Attention queue moving) reaches the open /events stream of each of its
// members — cal, a viewer, too — and not dee's. CONTROL: dee's own event
// reaches dee, so its silence on the team's is a fact about membership.
//
// 🔴 RED at the base (fcd4bbfd): a stream subscribed to its person alone, so
// a team's event reached no one (2b.1's per-operator copy of
// attention.changed, deleted here, reached operators only).
func TestATeamsEventsReachItsMembersStreams(t *testing.T) {
	v := newTeamView(t)
	cal, _ := v.openEvents(t, v.cal)
	dee, _ := v.openEvents(t, v.dee)
	deadline := time.Now().Add(2 * time.Second)
	for v.w.bus.Count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	teamKey, deeKey := "ask-team"+v.w.suffix, "ask-dee"+v.w.suffix
	v.w.srv.attentionChanged(v.team, teamKey)
	v.w.srv.attentionChanged(v.dee, deeKey)
	deadline = time.Now().Add(2 * time.Second)
	for !(strings.Contains(cal.text(), teamKey) && strings.Contains(dee.text(), deeKey)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(cal.text(), teamKey) {
		t.Errorf("cal's stream never carried the team's event:\n%s", cal.text())
	}
	if !strings.Contains(dee.text(), deeKey) {
		t.Fatalf("CONTROL: dee's own event never reached dee's stream:\n%s", dee.text())
	}
	if strings.Contains(dee.text(), teamKey) {
		t.Errorf("LEAK: dee's stream carried the team's event:\n%s", dee.text())
	}
}

// TestARemovedOperatorsTerminalClosesWithinTheSweep: B (operator) and cal
// (viewer) each hold the team host's terminal (read-only) and /events open.
// B is removed from the team THROUGH THE STORE — another replica's or the
// CLI's removal: nothing in this process is told — and within the membership
// sweep (teamLiveEvery, 100 ms here; DisableSweepInterval, 10 s, in
// production) B's terminal and B's event stream end. CONTROL: cal's stay
// open, so the end is B's membership, not the host or the server.
//
// 🔴 RED at the base (fcd4bbfd): a member had no team host terminal to open.
// Against this tree with the sweep removed, B's streams stay open.
func TestARemovedOperatorsTerminalClosesWithinTheSweep(t *testing.T) {
	v := newTeamView(t)
	bScreen, bTerm := v.openWS(t, v.teamQuery, v.w.b.uid)
	cScreen, cTerm := v.openWS(t, v.teamQuery, v.cal)
	_, bEvents := v.openEvents(t, v.w.b.uid)
	_, cEvents := v.openEvents(t, v.cal)
	deadline := time.Now().Add(5 * time.Second)
	for !(strings.Contains(bScreen(), v.shared) && strings.Contains(cScreen(), v.shared)) {
		if time.Now().After(deadline) {
			t.Fatalf("the team's screen never reached its members: B %q, cal %q", bScreen(), cScreen())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := v.store.RemoveTeamMember(v.w.sys, v.admin, v.team, v.w.b.uid); err != nil {
		t.Fatal(err)
	}
	removed := time.Now()
	for name, ch := range map[string]<-chan struct{}{"terminal": bTerm, "event stream": bEvents} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Errorf("B's %s is still open %v after B left the team", name, time.Since(removed))
		}
	}
	select {
	case <-cTerm:
		t.Error("CONTROL: cal's terminal ended too")
	case <-cEvents:
		t.Error("CONTROL: cal's event stream ended too")
	case <-time.After(500 * time.Millisecond):
	}
}

// staleMember is uid's browser-session principal as it was read BEFORE a
// change the caller then makes in the database.
func (v *teamView) staleMember(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), v.w.srv.principalFor(v.w.sys, uid, authz.ViaSession))
}

// TestARemovalJustBeforeAViewIsNotMissed (#690 round 1, 🟢1): cal's request
// was admitted as a member; she is removed before her view registers (through
// the store: nothing in this process closes anything). The gate reads her
// role again after registering and answers 404; her event stream, opened on
// the same stale principal, ends at once. CONTROL: B, still a member, is let
// through and his stream stays open.
//
// 🔴 RED at 1181b8aa: nothing re-read the role, so both lived until the next
// sweep.
func TestARemovalJustBeforeAViewIsNotMissed(t *testing.T) {
	v := newTeamView(t)
	cal, ben := v.staleMember(v.cal), v.staleMember(v.w.b.uid)
	if err := v.store.RemoveTeamMember(v.w.sys, v.admin, v.team, v.cal); err != nil {
		t.Fatal(err)
	}
	gate := func(ctx context.Context) (int, bool) {
		r := httptest.NewRequest("GET", v.teamPath, nil).WithContext(ctx)
		r.Pattern = "GET /mirror/terminal"
		rec := httptest.NewRecorder()
		_, done, ok := v.w.srv.mirrorTeamGate(rec, r)
		if done != nil {
			done()
		}
		return rec.Code, ok
	}
	if code, ok := gate(cal); ok || code != http.StatusNotFound {
		t.Errorf("cal, removed after her principal was read: gate ok=%v %d; want refused 404", ok, code)
	}
	if _, ok := gate(ben); !ok {
		t.Error("CONTROL: B, still a member, was refused")
	}
	cctx, _, cdone := v.w.srv.teamEventsOpen(cal)
	defer cdone()
	bctx, _, bdone := v.w.srv.teamEventsOpen(ben)
	defer bdone()
	if cctx.Err() == nil {
		t.Error("cal's event stream, opened on the stale principal, is still open")
	}
	if bctx.Err() != nil {
		t.Error("CONTROL: B's event stream ended")
	}
}

// TestAnOversizedScrollIsRefusedBeforeItIsRead (#690 round 1, 🟢2): a scroll
// whose body passes the handler's 4 KiB bound is answered 413, whether or not
// it names a team — the gate reads the body first, under the same bound.
//
// 🔴 RED at 1181b8aa: the gate parsed the body under net/http's 10 MiB
// default, so the handler's own bound never applied.
func TestAnOversizedScrollIsRefusedBeforeItIsRead(t *testing.T) {
	v := newTeamView(t)
	body := url.Values{"host": {"build-01"}, "session": {v.own}, "lines": {"3"}, "pad": {strings.Repeat("x", 5<<10)}}
	if rec := v.w.do(v.w.h, "POST", "/mirror/terminal/scroll", body.Encode(), cred{sessionUser: v.w.a.uid}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("A's oversized scroll: %d %q; want 413", rec.Code, truncate(rec.Body.String(), 120))
	}
	small := url.Values{"host": {"build-01"}, "session": {v.own}, "lines": {"3"}}
	if rec := v.w.do(v.w.h, "POST", "/mirror/terminal/scroll", small.Encode(), cred{sessionUser: v.w.a.uid}); rec.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("CONTROL: A's ordinary scroll was answered 413")
	}
}
