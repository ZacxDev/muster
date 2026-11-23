package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/coder/websocket"
)

// WHO TYPED EACH LINE OF A TEAM'S SHARED CHAT (chat_typers.go). The walk's team
// T (A manages, B operates, V views) owns a chief whose one room A, B and others
// have typed into; every chat renderer draws each typed line with its typer's
// name, the viewer's own as "You", and a personal chat draws none.

// typerOfBubble matches one stored bubble and its typer label, if any: group 1
// the message id, group 2 the label's (escaped) text, group 3 non-empty when a
// label is drawn at all.
var typerOfBubble = regexp.MustCompile(`data-msg-id="(\d+)" data-md-rendered="">(<div data-typed-by=""[^>]*>([^<]*)</div>)?`)

// typerLabels is every bubble of body by message id: its label's text, and
// whether it has one.
func typerLabels(t *testing.T, body string) map[int64]struct {
	label string
	drawn bool
} {
	t.Helper()
	out := map[int64]struct {
		label string
		drawn bool
	}{}
	for _, m := range typerOfBubble.FindAllStringSubmatch(body, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		out[id] = struct {
			label string
			drawn bool
		}{m[3], m[2] != ""}
	}
	return out
}

// typedLine stores a user line of the team chief's room typed by uid (0: nobody
// typed it).
func (tw *teamAgentWalk) typedLine(t *testing.T, uid int64, text string) int64 {
	t.Helper()
	m, err := agents.NewPG(tw.w.pool).AddChatMessage(tw.teamCtx, agents.ChatMessage{AgentID: tw.chief.ID, SessionID: tw.session.ID,
		Role: "user", Kind: "text", Content: text, TypedBy: uid})
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// person is one more account with display name name (never in the team).
func (tw *teamAgentWalk) person(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	if err := tw.w.pool.QueryRow(tw.w.sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = tw.w.pool.Exec(authz.System(context.Background()), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// TestATeamChatNamesWhoTypedEachLine: in the team chief's room, a line typed
// by A and a line typed by B draw A's and B's display names, to a viewer (V)
// as to a member; the viewer's own lines draw "You"; a typer with no person
// row (an unknown or deleted account) and one with no display name draw the
// fallback "user #<id>", never an email; HTML in a display name draws as
// text; a line nobody typed draws no label. Every renderer: the chat-log
// refetch (what sse:chat.reply drives) and the page.
//
// 🔴 RED at origin/main: no bubble carries a label (no renderer draws a name).
func TestATeamChatNamesWhoTypedEachLine(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	hostile := tw.person(t, `<img src=x onerror=alert(1)>&"Mallory`)
	unnamed := tw.person(t, "")
	if _, err := w.pool.Exec(w.sys, `UPDATE users SET email = $2 WHERE id = $1`, unnamed, "unnamed-"+w.suffix+"@example.com"); err != nil {
		t.Fatal(err)
	}
	unknown := int64(1) << 40 // no users row at all
	long := tw.person(t, strings.Repeat("Bartholomew ", 10))
	fromA := tw.typedLine(t, w.a.uid, "from A "+tw.canary)
	fromB := tw.typedLine(t, w.b.uid, "from B "+tw.canary)
	fromHostile := tw.typedLine(t, hostile, "from H "+tw.canary)
	fromUnnamed := tw.typedLine(t, unnamed, "from U "+tw.canary)
	fromUnknown := tw.typedLine(t, unknown, "from X "+tw.canary)
	fromLong := tw.typedLine(t, long, "from L "+tw.canary)
	untyped := tw.typedLine(t, 0, "nobody typed this "+tw.canary)
	fromV := tw.typedLine(t, tw.v.uid, "from V "+tw.canary)
	// A plugin message delivered to the chief, as internal/plugins' Deliverer
	// writes it: a user row with its plugin-message link and no typer.
	relay, err := agents.NewPG(w.pool).AddChatMessage(tw.teamCtx, agents.ChatMessage{AgentID: tw.chief.ID, SessionID: tw.session.ID,
		Role: "user", Kind: "text", Content: "a plugin says " + tw.canary, PluginMessageID: 987654})
	if err != nil {
		t.Fatal(err)
	}

	aName, bName, vName := w.a.name+w.suffix, w.b.name+w.suffix, tw.v.name+"-"+w.suffix
	type want struct {
		label string
		drawn bool
	}
	cases := []struct {
		viewer int64
		path   string
		want   map[int64]want
	}{
		{tw.v.uid, "/ui/agents/" + tw.chief.Name + "/chat-log", map[int64]want{
			fromA: {aName, true}, fromB: {bName, true}, fromV: {"You", true},
			fromHostile: {`&lt;img src=x onerror=alert(1)&gt;&amp;&#34;Mallory`, true},
			fromUnnamed: {"user #" + strconv.FormatInt(unnamed, 10), true},
			fromUnknown: {"user #" + strconv.FormatInt(unknown, 10), true},
			fromLong:    {strings.Repeat("Bartholomew ", 3) + "Bar…", true},
			untyped:     {"", false},
		}},
		{w.b.uid, "/ui/agents/" + tw.chief.Name + "/chat-log", map[int64]want{
			fromA: {aName, true}, fromB: {"You", true}, fromV: {vName, true}, untyped: {"", false},
		}},
		{tw.v.uid, "/agents/" + tw.chief.Name + "?session=" + strconv.FormatInt(tw.session.ID, 10), map[int64]want{
			fromA: {aName, true}, fromB: {bName, true}, fromV: {"You", true}, untyped: {"", false},
		}},
	}
	for _, c := range cases {
		rec := w.do(w.h, "GET", c.path, "", cred{sessionUser: c.viewer})
		if !is2xx(rec.Code) {
			t.Fatalf("GET %s as %d: %d %s", c.path, c.viewer, rec.Code, truncate(rec.Body.String(), 300))
		}
		body := rec.Body.String()
		got := typerLabels(t, body)
		for id, wl := range c.want {
			g, ok := got[id]
			if !ok {
				t.Errorf("GET %s as %d: message %d has no bubble", c.path, c.viewer, id)
				continue
			}
			if g.drawn != wl.drawn || g.label != wl.label {
				t.Errorf("GET %s as %d: message %d's typer label = %q (drawn %v); want %q (drawn %v)", c.path, c.viewer, id, g.label, g.drawn, wl.label, wl.drawn)
			}
		}
		// The plugin message draws as its relay row, never with a typer label;
		// and no label is drawn beyond the typed lines this case expects.
		relayRow := regexp.MustCompile(`<details data-relay-row="" data-msg-id="` + strconv.FormatInt(relay.ID, 10) + `"[^>]*>(.*?)</details>`).FindStringSubmatch(body)
		if relayRow == nil {
			t.Errorf("GET %s as %d: the plugin message %d is not drawn as its relay row (instrument check)", c.path, c.viewer, relay.ID)
		} else if strings.Contains(relayRow[1], "data-typed-by") {
			t.Errorf("GET %s as %d: the plugin message %d draws a typer label", c.path, c.viewer, relay.ID)
		}
		labelled := 0
		for _, wl := range c.want {
			if wl.drawn {
				labelled++
			}
		}
		if c.viewer == tw.v.uid && c.path == "/ui/agents/"+tw.chief.Name+"/chat-log" {
			if n := strings.Count(body, "data-typed-by"); n != labelled {
				t.Errorf("GET %s as %d: %d typer labels drawn; want exactly the %d typed lines'", c.path, c.viewer, n, labelled)
			}
		}
		if strings.Contains(body, "unnamed-"+w.suffix+"@example.com") {
			t.Errorf("GET %s as %d: a typer's EMAIL is drawn", c.path, c.viewer)
		}
		if strings.Contains(body, "<img src=x") {
			t.Errorf("GET %s as %d: a display name's HTML is drawn unescaped", c.path, c.viewer)
		}
	}
}

// TestAPersonalChatDrawsNoTyperLabel: A's own agent's chat, where A typed a
// line, draws it as it always has — no label, not even "You".
//
// The CONTROL is the team test above: the same renderer, a line typed by the
// viewer, draws "You" there. Mutation-checked: dropping chatTypers' team-only
// condition turns this test red with its own assertion.
func TestAPersonalChatDrawsNoTyperLabel(t *testing.T) {
	w := newIsoWorld(t)
	ua := authz.WithPrincipal(context.Background(), authz.Principal{UserID: w.a.uid, Role: authz.RoleMember, Via: authz.ViaSession})
	m, err := agents.NewPG(w.pool).AddChatMessage(ua, agents.ChatMessage{AgentID: w.a.agent.ID, SessionID: w.aChatSession.ID,
		Role: "user", Kind: "text", Content: "A typed this " + w.canary, TypedBy: w.a.uid})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/ui/agents/" + w.a.agent.Name + "/chat-log", "/agents/" + w.a.agent.Name + "?session=" + strconv.FormatInt(w.aChatSession.ID, 10)} {
		rec := w.do(w.h, "GET", path, "", cred{sessionUser: w.a.uid})
		if !is2xx(rec.Code) {
			t.Fatalf("GET %s as A: %d %s", path, rec.Code, truncate(rec.Body.String(), 300))
		}
		got, ok := typerLabels(t, rec.Body.String())[m.ID]
		if !ok {
			t.Fatalf("GET %s as A: A's typed line %d has no bubble (instrument check)", path, m.ID)
		}
		if got.drawn {
			t.Errorf("GET %s as A: A's own line in A's PERSONAL chat draws the typer label %q; want none", path, got.label)
		}
		if strings.Contains(rec.Body.String(), "data-typed-by") {
			t.Errorf("GET %s as A: a personal chat draws a typer label", path)
		}
	}
}

// sentEchoOf dials agent's chat socket as uid, sends text, and returns the
// socket's "sent" echo html.
func sentEchoOf(t *testing.T, w *isoWorld, agent string, session, uid int64, text string) string {
	t.Helper()
	ts := httptest.NewServer(w.h)
	t.Cleanup(ts.Close)
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(w.srv))
	h.Set(isoUserHeader, strconv.FormatInt(uid, 10))
	h.Set("Origin", ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/agents/" + agent + "/ws?session=" + strconv.FormatInt(session, 10)
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial %s's chat socket as %d: %v", agent, uid, err)
	}
	t.Cleanup(func() { c.CloseNow() })
	msg, _ := json.Marshal(map[string]string{"type": "message", "text": text})
	if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatal(err)
	}
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read %s's chat socket as %d: no \"sent\" echo: %v", agent, uid, err)
		}
		var ev struct {
			Type string `json:"type"`
			HTML string `json:"html"`
		}
		if json.Unmarshal(data, &ev) == nil && ev.Type == "sent" {
			return ev.HTML
		}
	}
}

// TestTheLiveChatPathCarriesTheTyper: the live halves of a team chat carry
// the typer too. B's message over the chat socket is echoed with B's label
// ("You", as the stored bubble draws it for B), and the refetch that
// sse:chat.reply drives draws B's NAME on that line for V. A's message in A's
// personal chat is echoed with no label.
//
// 🔴 RED at origin/main: the echo is the body alone and no refetch names B.
func TestTheLiveChatPathCarriesTheTyper(t *testing.T) {
	tw := newTeamAgentWalk(t)
	w := tw.w
	text := "live from B " + tw.canary
	echo := sentEchoOf(t, w, tw.chief.Name, tw.session.ID, w.b.uid, text)
	if want := `<div data-typed-by="" class="mb-1 text-xs font-medium text-fg2">You</div>`; !strings.HasPrefix(echo, want) || !strings.Contains(echo, text) {
		t.Errorf("B's echo in the team chat = %q; want it to start with the label %q and carry the text", echo, want)
	}
	var id int64
	eventuallyAPI(t, "B's line to be stored", func() bool {
		_ = w.pool.QueryRow(w.sys, `SELECT coalesce(max(id), 0) FROM chat_messages WHERE agent_id = $1 AND role = 'user' AND content = $2 AND typed_by = $3`,
			tw.chief.ID, text, w.b.uid).Scan(&id)
		return id > 0
	})
	rec := w.do(w.h, "GET", "/ui/agents/"+tw.chief.Name+"/chat-log", "", cred{sessionUser: tw.v.uid})
	if got := typerLabels(t, rec.Body.String())[id]; got.label != w.b.name+w.suffix {
		t.Errorf("V's refetch draws B's live line %d with label %q (drawn %v); want B's name %q", id, got.label, got.drawn, w.b.name+w.suffix)
	}

	pw := newIsoWorld(t)
	if echo := sentEchoOf(t, pw, pw.a.agent.Name, pw.aChatSession.ID, pw.a.uid, "personal "+pw.canary); strings.Contains(echo, "data-typed-by") {
		t.Errorf("A's echo in A's personal chat = %q; want no typer label", echo)
	}
}
