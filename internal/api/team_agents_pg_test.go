package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/users"
)

// THE TEAM-AGENT WALK (team sharing phase 4, slice 4.4; plan §6 and §7). Team
// T: A manages it, B operates in it, V views it, N is in no team. T owns a
// running chief (chief-<T>) whose chat carries a canary, whose credentials are
// in agent_secrets only (0131), and whose turns go to a fake ccd through the
// REAL gateway (agentgateway.Gateway reading agents.Store.Secrets), so the
// bearer a turn arrives with says which principal read the credential.

type teamAgentWalk struct {
	w        *isoWorld
	team     int64
	v, n     isoUser
	chief    agents.Agent
	session  agents.ChatSession
	canary   string
	secret   string
	ccd      *authCCD
	teamCtx  context.Context
	userRows string
}

// authCCD is busyCCD recording each request's Authorization header.
type authCCD struct {
	*busyCCD
	mu    sync.Mutex
	auths []string
}

func (f *authCCD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()
	f.busyCCD.ServeHTTP(w, r)
}

func (f *authCCD) authorizations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...)
}

func newTeamAgentWalk(t *testing.T) *teamAgentWalk {
	t.Helper()
	w := newIsoWorld(t)
	w.srv.ext.Users = users.NewPG(w.pool)
	tw := &teamAgentWalk{w: w, canary: "tacnry" + w.suffix, secret: "team-turn-credential-" + w.suffix}
	store := users.NewPG(w.pool)
	by := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	person := func(name string) isoUser {
		var id int64
		if err := w.pool.QueryRow(w.sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+"-"+w.suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return isoUser{name: name, uid: id}
	}
	tw.v, tw.n = person("ta-viewer"), person("ta-outsider")
	team, err := store.CreateTeam(w.sys, by, "agents team "+w.suffix, w.a.uid)
	if err != nil {
		t.Fatal(err)
	}
	tw.team = team.ID
	for uid, role := range map[int64]authz.TeamRole{w.b.uid: authz.TeamOperator, tw.v.uid: authz.TeamViewer} {
		if err := store.SetTeamMember(w.sys, by, tw.team, uid, role); err != nil {
			t.Fatal(err)
		}
	}
	tw.teamCtx = authz.WithPrincipal(context.Background(), teamActingPrincipal(tw.team, authz.ViaAgent))
	as := agents.NewPG(w.pool)
	tw.chief, err = as.Create(tw.teamCtx, agents.Agent{Name: agents.ChiefNameFor(tw.team), DisplayName: ChiefDisplayName,
		Namespace: "ns-team-chief-" + w.suffix, Status: agents.StatusRunning, Kind: agents.KindClaudeCode, CCAccount: "acct",
		HooksToken: "team-hooks-" + w.suffix})
	if err != nil {
		t.Fatal(err)
	}
	if err := as.SetGatewaySecret(tw.teamCtx, tw.chief.ID, tw.secret); err != nil {
		t.Fatal(err)
	}
	tw.session, err = as.LatestOrCreateSession(tw.teamCtx, tw.chief.ID, tw.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.AddChatMessage(tw.teamCtx, agents.ChatMessage{AgentID: tw.chief.ID, SessionID: tw.session.ID, Role: "assistant",
		Kind: "text", Content: "team chief says " + tw.canary}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM chat_messages WHERE owner_id = $1`, tw.team)
		_, _ = w.pool.Exec(bg, `DELETE FROM chat_sessions WHERE owner_id = $1`, tw.team)
		_, _ = w.pool.Exec(bg, `DELETE FROM chief_turn_usage WHERE owner_id = $1`, tw.team)
		_, _ = w.pool.Exec(bg, `DELETE FROM agents WHERE owner_id = $1`, tw.team)
		_, _ = w.pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, tw.team)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE object = $1 OR team_id = $1::bigint`, users.TeamObject(tw.team))
		_, _ = w.pool.Exec(bg, `DELETE FROM users WHERE id = ANY($1)`, []int64{tw.team, tw.v.uid, tw.n.uid})
	})

	// The real gateway, reading the credential through the store as the turn's
	// principal, against a fake ccd that starts FREE.
	tw.ccd = &authCCD{busyCCD: newBusyCCD()}
	tw.ccd.free()
	up := httptest.NewServer(tw.ccd)
	t.Cleanup(up.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Client: up.Client(), Secrets: as})
	if err != nil {
		t.Fatal(err)
	}
	w.srv.ext.Gateway = gw
	// Live state "not observed": the stored status (running) is the chief's,
	// as the isolation walk's provisioner lists no instance.
	w.srv.ext.Provisioner = unobservedProv{w.prov}
	w.srv.chatBusyWait, w.srv.chatBusyPoll = 30*time.Second, 10*time.Millisecond
	tw.userRows = `SELECT COALESCE((SELECT json_agg(x ORDER BY x.id)::text FROM agents x WHERE owner_id = $1), '') ||
		COALESCE((SELECT json_agg(x ORDER BY x.id)::text FROM chat_sessions x WHERE owner_id = $1), '') ||
		COALESCE((SELECT json_agg(x ORDER BY x.id)::text FROM chat_messages x WHERE owner_id = $1), '')`
	return tw
}

// unobservedProv is the walk's provisioner with its live instances unread.
type unobservedProv struct{ *isoProvisioner }

func (unobservedProv) Instances(context.Context) ([]provision.Instance, error) {
	return nil, errors.New("live state is not observed in this test")
}

func (tw *teamAgentWalk) rows(t *testing.T) string {
	t.Helper()
	var s string
	if err := tw.w.pool.QueryRow(tw.w.sys, tw.userRows, tw.team).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// path is r's path with the team chief's ids filled in.
func (tw *teamAgentWalk) path(r string) (method, path string) {
	method, path, _ = strings.Cut(r, " ")
	path = strings.NewReplacer("{name}", tw.chief.Name, "{id}", strconv.FormatInt(tw.chief.ID, 10),
		"{sid}", strconv.FormatInt(tw.session.ID, 10)).Replace(path)
	return method, path
}

// teamChatRoutes and teamRefusedRoutes are the plan's role table for a team
// agent's routes (§6), written out LITERALLY — not read from teamAgentRoutes,
// so a rule loosened there is caught here rather than walked past.
var (
	teamChatRoutes    = []string{"GET /agents/{name}/ws", "POST /agents/{name}/send", "POST /agents/sessions/{sid}/read"}
	teamRefusedRoutes = []string{
		"GET /agents/{name}/terminal/ws",
		"POST /agents/{id}/start", "POST /agents/{id}/stop", "DELETE /agents/{id}",
		"POST /agents/{id}/model", "POST /agents/{id}/name",
		"POST /agents/{id}/sessions", "POST /agents/{id}/sessions/side", "POST /agents/{id}/sessions/fresh-primary",
		"POST /agents/{id}/sessions/{sid}/default", "POST /agents/{id}/sessions/{sid}/archive",
		"POST /agents/{id}/sessions/{sid}/unarchive",
	}
)

// sessionAgentRoutes is every agent route a browser session reaches (not the
// machine /api/ doors), with the rule the plan's table gives it: chat,
// refused, or read for every other.
func sessionAgentRoutes() map[string]teamAgentRule {
	out := map[string]teamAgentRule{}
	for r := range teamAgentRoutes {
		if _, p, _ := strings.Cut(r, " "); !strings.HasPrefix(p, "/api/") {
			out[r] = teamAgentRead
		}
	}
	for _, r := range teamChatRoutes {
		out[r] = teamAgentChat
	}
	for _, r := range teamRefusedRoutes {
		out[r] = teamAgentRefused
	}
	return out
}

func (tw *teamAgentWalk) send(t *testing.T, uid int64, text string) *httptest.ResponseRecorder {
	t.Helper()
	return tw.w.do(tw.w.h, "POST", "/agents/"+tw.chief.Name+"/send",
		url.Values{"text": {text}, "session": {strconv.FormatInt(tw.session.ID, 10)}}.Encode(), cred{sessionUser: uid})
}

// TestATeamAgentIsWalkedByEveryRole is the walk's role legs (plan §6, chat
// role tests):
//
//   - N (not a member): no route naming the team chief carries its canary,
//     acts on its instance, or changes anything of the team's (a lifecycle
//     route answers a ghost's 200 the same way: isolation_test.go);
//   - V (viewer): reads the chat (the canary, on the chat log); every chat route
//     and every refused route answers 403 from the team-agent gate; nothing of
//     the team's changes;
//   - B (operator): every refused route still 403 (lifecycle, settings, new
//     threads, the terminal), and the send is accepted;
//   - A (manager) is refused the same routes: in this slice no member's
//     principal acts on a team agent's lifecycle (slice 4.5).
//
// 🔴 RED at origin/main 006bbe5c: no member sees the team chief at all (agents
// is personal there), so V's read and B's send fail.
func TestATeamAgentIsWalkedByEveryRole(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	routes := sessionAgentRoutes()
	if len(routes) < 20 {
		t.Fatalf("instrument check FAILED: %d session agent routes", len(routes))
	}
	before := tw.rows(t)
	if !strings.Contains(before, tw.canary) {
		t.Fatalf("fixture: the team's rows carry no canary")
	}
	for r := range routes {
		m, p := tw.path(r)
		rec := w.do(w.h, m, p, "", cred{sessionUser: tw.n.uid})
		if strings.Contains(rec.Body.String(), tw.canary) {
			t.Errorf("LEAK: N (not a member) %s carries the team chief's canary", r)
		}
	}
	if after := tw.rows(t); after != before {
		t.Errorf("MUTATION: N's walk changed the team's rows")
	}
	if n := tw.w.prov.actedOn(tw.chief.ID, tw.n.uid); n != 0 {
		t.Errorf("N's walk acted on the team chief's instance %d time(s)", n)
	}

	for r, rule := range routes {
		m, p := tw.path(r)
		body := ""
		if strings.HasSuffix(p, "/send") && m == "POST" {
			body = url.Values{"text": {"viewer says " + tw.canary}}.Encode()
		}
		rec := w.do(w.h, m, p, body, cred{sessionUser: tw.v.uid})
		if rule != teamAgentRead && rec.Code != http.StatusForbidden {
			t.Errorf("V (viewer) %s → %d; want 403 from the team-agent gate", r, rec.Code)
		}
	}
	if log := w.do(w.h, "GET", "/ui/agents/"+tw.chief.Name+"/chat-log", "", cred{sessionUser: tw.v.uid}); !is2xx(log.Code) ||
		!strings.Contains(log.Body.String(), "team chief says "+tw.canary) {
		t.Errorf("V (viewer) cannot read the team chief's chat: %d", log.Code)
	}
	if after := tw.rows(t); after != before {
		t.Errorf("MUTATION: V's (a viewer's) walk changed the team's rows:\n before %s\n after  %s", before, after)
	}

	for _, who := range []isoUser{w.b, w.a} {
		for r, rule := range routes {
			if rule != teamAgentRefused {
				continue
			}
			m, p := tw.path(r)
			if rec := w.do(w.h, m, p, "", cred{sessionUser: who.uid}); rec.Code != http.StatusForbidden {
				t.Errorf("%s (member) %s → %d; want 403: a member does not act on a team agent through it in this slice", who.name, r, rec.Code)
			}
		}
	}
	if after := tw.rows(t); after != before {
		t.Errorf("MUTATION: a refused route changed the team's rows")
	}
	if rec := tw.send(t, w.b.uid, "operator says hi"); rec.Code != http.StatusNoContent {
		t.Errorf("B (operator) sending to the team chief: %d %s; want 204", rec.Code, truncate(rec.Body.String(), 200))
	}
}

// TestAMembersTurnOnTheTeamChiefRunsAsTheTeam: B's message to the team chief
// reaches the runtime with the TEAM'S turn credential — read from
// agent_secrets, which only the team acting principal may read (B's own
// principal fails closed: agents.Store.Secrets) — behind a muster line naming
// B and B's role; the reply is stored; the stored user row is what B typed
// (no header), typed by B and owned by the team.
//
// 🔴 RED at origin/main 006bbe5c: B cannot see the team chief (404).
func TestAMembersTurnOnTheTeamChiefRunsAsTheTeam(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	typed := "please summarise " + tw.canary
	if rec := tw.send(t, w.b.uid, typed); rec.Code != http.StatusNoContent {
		t.Fatalf("B (operator) sending: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	eventuallyAPI(t, "the team chief to answer B", func() bool {
		_, answered := tw.ccd.counts()
		return len(answered) == 1
	})
	_, answered := tw.ccd.counts()
	header := "[muster] typed by " + w.b.name + w.suffix + " (operator) in team agents team " + w.suffix
	if !strings.HasPrefix(answered[0], header) || !strings.HasSuffix(answered[0], typed) {
		t.Errorf("the runtime was handed %q; want the muster line %q then what B typed", answered[0], header)
	}
	if got := tw.ccd.authorizations(); len(got) != 1 || got[0] != "Bearer "+tw.secret {
		t.Errorf("the turn reached the runtime with Authorization %v; want the TEAM's turn credential (read as the team acting principal)", got)
	}
	var content string
	var typedBy, owner int64
	if err := w.pool.QueryRow(w.sys, `SELECT content, coalesce(typed_by, 0), owner_id FROM chat_messages
		WHERE agent_id = $1 AND role = 'user' ORDER BY id DESC LIMIT 1`, tw.chief.ID).Scan(&content, &typedBy, &owner); err != nil {
		t.Fatal(err)
	}
	if content != typed || typedBy != w.b.uid || owner != tw.team {
		t.Errorf("stored user row: %q typed_by %d owner %d; want exactly what B typed, typed by B %d, owned by the team %d",
			content, typedBy, owner, w.b.uid, tw.team)
	}
	eventuallyAPI(t, "the reply to be stored", func() bool {
		var n int
		_ = w.pool.QueryRow(w.sys, `SELECT count(*) FROM chat_messages WHERE agent_id = $1 AND role = 'assistant' AND content LIKE 'answer to:%'`, tw.chief.ID).Scan(&n)
		return n == 1
	})
}

// TestARemovedMembersQueuedMessageIsDropped (plan §3.1 "Turns and
// concurrency", §6): B's message waits behind a turn holding the team chief's
// session; B is removed from the team while it waits; A's message queues
// behind it. When the session frees, B's message is NOT sent (the role is
// re-read before each send attempt) and A's is. CONTROL: A's — a member's
// message through the same queue — is answered, so the drop is about B.
//
// 🔴 RED at origin/main 006bbe5c: B cannot see the team chief (404).
func TestARemovedMembersQueuedMessageIsDropped(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	tw.ccd.mu.Lock()
	tw.ccd.busy = true
	tw.ccd.mu.Unlock()
	fromB, fromA := "from B "+tw.canary, "from A "+tw.canary
	if rec := tw.send(t, w.b.uid, fromB); rec.Code != http.StatusNoContent {
		t.Fatalf("B (operator) sending: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	waitRefusals(t, tw.ccd.busyCCD, 1) // B's message is waiting on the busy session
	if err := users.NewPG(w.pool).RemoveTeamMember(w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, tw.team, w.b.uid); err != nil {
		t.Fatal(err)
	}
	if rec := tw.send(t, w.a.uid, fromA); rec.Code != http.StatusNoContent {
		t.Fatalf("A (manager) sending: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	tw.ccd.free()
	eventuallyAPI(t, "A's message to be answered", func() bool {
		_, answered := tw.ccd.counts()
		for _, a := range answered {
			if strings.HasSuffix(a, fromA) {
				return true
			}
		}
		return false
	})
	_, answered := tw.ccd.counts()
	for _, a := range answered {
		if strings.HasSuffix(a, fromB) {
			t.Errorf("the removed member's queued message was sent to the team chief: %q", a)
		}
	}
}

// TestAMemberWritesOnlyInTheTeamRoom ("one shared room", operator decision 4):
// a team chief has a second thread (a side chat). B (operator) naming it —
// the composer's session field, the chat socket's ?session= — is refused 403,
// and nothing is stored in it. CONTROL: B's send and socket on the room (the
// chief's primary) are not refused.
//
// 🔴 RED at 30a995ac: resolveSession took any session of the agent's, so B's
// send to the side chat was accepted (204) and its socket opened on it.
func TestAMemberWritesOnlyInTheTeamRoom(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	side, err := agents.NewPG(w.pool).CreateSession(tw.teamCtx, tw.chief.ID, tw.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if side.ID == tw.session.ID {
		t.Fatalf("fixture: the side chat is the room")
	}
	sendIn := func(sid int64, text string) *httptest.ResponseRecorder {
		return w.do(w.h, "POST", "/agents/"+tw.chief.Name+"/send",
			url.Values{"text": {text}, "session": {strconv.FormatInt(sid, 10)}}.Encode(), cred{sessionUser: w.b.uid})
	}
	if rec := sendIn(side.ID, "into the side chat "+tw.canary); rec.Code != http.StatusForbidden {
		t.Errorf("B sending into the team chief's side chat: %d %s; want 403", rec.Code, truncate(rec.Body.String(), 200))
	}
	ws := func(sid int64) int {
		return w.do(w.h, "GET", "/agents/"+tw.chief.Name+"/ws?session="+strconv.FormatInt(sid, 10), "", cred{sessionUser: w.b.uid}).Code
	}
	if code := ws(side.ID); code != http.StatusForbidden {
		t.Errorf("B opening the chat socket on the side chat: %d; want 403", code)
	}
	if code := ws(tw.session.ID); code == http.StatusForbidden {
		t.Errorf("control: B opening the chat socket on the room is refused 403")
	}
	if rec := sendIn(tw.session.ID, "into the room"); rec.Code != http.StatusNoContent {
		t.Errorf("control: B sending into the room: %d %s; want 204", rec.Code, truncate(rec.Body.String(), 200))
	}
	var n int
	if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM chat_messages WHERE session_id = $1`, side.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d message(s) were stored in the side chat; want none", n)
	}
}

// TestAMarkReadOnATeamSessionIsTheRoomsWriters: mark-read writes the team
// session's one read state. B (operator) marks it (204, read_at moves); V
// (viewer) is refused 403 by the team-agent gate and read_at does not move.
// (The walk's role legs, through teamChatRoutes, pin V's 403 too.)
//
// 🔴 RED at 30a995ac: the route was outside teamAgentRoutes, so V's call
// passed to the handler (204, a silent no-op under 0127's operator write).
func TestAMarkReadOnATeamSessionIsTheRoomsWriters(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	readAt := func() time.Time {
		var at time.Time
		if err := w.pool.QueryRow(w.sys, `SELECT read_at FROM chat_sessions WHERE id = $1`, tw.session.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	path := "/agents/sessions/" + strconv.FormatInt(tw.session.ID, 10) + "/read"
	before := readAt()
	if rec := w.do(w.h, "POST", path, "", cred{sessionUser: tw.v.uid}); rec.Code != http.StatusForbidden {
		t.Errorf("V (viewer) marking the team session read: %d %s; want 403", rec.Code, truncate(rec.Body.String(), 200))
	}
	if !readAt().Equal(before) {
		t.Errorf("V's mark-read moved the team session's read state")
	}
	if rec := w.do(w.h, "POST", path, "", cred{sessionUser: w.b.uid}); rec.Code != http.StatusNoContent {
		t.Errorf("control: B (operator) marking the team session read: %d %s; want 204", rec.Code, truncate(rec.Body.String(), 200))
	}
	if readAt().Equal(before) {
		t.Errorf("control: B's mark-read did not move read_at")
	}
}

// teamChiefAsk is an open ask the team chief raised, owned by the team.
func (tw *teamAgentWalk) teamChiefAsk(t *testing.T) attention.Ask {
	t.Helper()
	ask, err := attention.NewPG(tw.w.pool).Create(tw.teamCtx, attention.New{Asker: attention.Asker{AgentID: tw.chief.ID}, Question: "ship it? " + tw.canary})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = tw.w.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE owner_id = $1`, tw.team)
	})
	return ask
}

func (tw *teamAgentWalk) askStatus(t *testing.T, id int64) string {
	t.Helper()
	var st string
	if err := tw.w.pool.QueryRow(tw.w.sys, `SELECT status FROM attention_asks WHERE id = $1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestARemovedMembersDroppedMessageClosesNoAsks: the team chief has an open
// ask. B's message waits behind a busy session; B is removed while it waits;
// the message is dropped (TestARemovedMembersQueuedMessageIsDropped) — and
// the ask is still OPEN: a member's message answers the agent's asks only
// after the fresh role check passes and the runtime takes it.
// CONTROL: the same message from a B who is NOT removed is answered and
// answers the ask (answered_in_session) — the deferred answer does fire.
//
// 🔴 RED at 30a995ac: askerAddressed ran at the door, before the turn, so the
// dropped message had already answered the ask.
func TestARemovedMembersDroppedMessageClosesNoAsks(t *testing.T) {
	for _, removed := range []bool{true, false} {
		tw := newTeamAgentWalk(t)
		w := tw.w
		ask := tw.teamChiefAsk(t)
		tw.ccd.mu.Lock()
		tw.ccd.busy = true
		tw.ccd.mu.Unlock()
		fromB := "from B " + tw.canary
		if rec := tw.send(t, w.b.uid, fromB); rec.Code != http.StatusNoContent {
			t.Fatalf("B (operator) sending: %d %s", rec.Code, truncate(rec.Body.String(), 300))
		}
		waitRefusals(t, tw.ccd.busyCCD, 1)
		if removed {
			if err := users.NewPG(w.pool).RemoveTeamMember(w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, tw.team, w.b.uid); err != nil {
				t.Fatal(err)
			}
		}
		tw.ccd.free()
		eventuallyAPI(t, "B's message to be settled", func() bool { return !w.srv.sessionTurns.inFlight(tw.session.SessionKey) })
		_, answered := tw.ccd.counts()
		got := tw.askStatus(t, ask.ID)
		if removed {
			if len(answered) != 0 {
				t.Fatalf("instrument: the removed member's message was sent: %v", answered)
			}
			if got != "open" {
				t.Errorf("the team chief's ask is %q after the removed member's DROPPED message; want open", got)
			}
			continue
		}
		if len(answered) != 1 {
			t.Fatalf("control: B's message was not answered: %v", answered)
		}
		if got != "answered_in_session" {
			t.Errorf("control: the team chief's ask is %q after B's answered message; want answered_in_session", got)
		}
	}
}
