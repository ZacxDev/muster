package api

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// cardsMirror is a hub as two owners see it: owner 1 has "studio-mac"
// (heron:1, conversation cs-heron-0001, busy) and — as a team member —
// a TEAM's host "team-box" (lynx:1, cs-team-0001), which the real hub lists
// to members; owner 2 has "work-laptop" (lynx:1, cs-lynx-0001).
type cardsMirror struct{ *fakeMirror }

func (cardsMirror) Hosts(ctx context.Context) []mirror.HostView {
	win := func(session, sid, status string) []mirror.Window {
		return []mirror.Window{{Session: session, ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: sid, Status: status}}
	}
	switch requestOwner(ctx) {
	case 1:
		return []mirror.HostView{
			{Owner: 1, Host: "studio-mac", Sessions: []string{"heron"}, Online: true, StatusReport: true,
				Windows: win("heron", "cs-heron-0001", mirror.StatusBusy)},
			{Owner: 900, Host: "team-box", Sessions: []string{"lynx"}, Online: true, StatusReport: true,
				Windows: win("lynx", "cs-team-0001", mirror.StatusIdle)},
		}
	case 2:
		return []mirror.HostView{{Owner: 2, Host: "work-laptop", Sessions: []string{"lynx"}, Online: true, StatusReport: true,
			Windows: win("lynx", "cs-lynx-0001", mirror.StatusWaiting)}}
	}
	return nil
}

// cardsAgents is the agents table as each owner's principal reads it: owner
// 1's chief (40) and agent fix-login (41), and a TEAM's agent (77) a member
// sees since 0131; owner 2's agent (50) and chief (51).
type cardsAgents struct{ stubAgentsStore }

func (cardsAgents) rows(ctx context.Context) []agents.Agent {
	switch requestOwner(ctx) {
	case 1:
		return []agents.Agent{{ID: 40, Name: "chief", OwnerID: 1, Kind: agents.KindClaudeCode},
			{ID: 41, Name: "fix-login", OwnerID: 1, Kind: agents.KindClaudeCode},
			{ID: 77, Name: "team-bot", OwnerID: 900, Kind: agents.KindClaudeCode}}
	case 2:
		return []agents.Agent{{ID: 50, Name: "theirs", OwnerID: 2, Kind: agents.KindClaudeCode},
			{ID: 51, Name: "chief", OwnerID: 2, Kind: agents.KindClaudeCode}}
	}
	return nil
}
func (a cardsAgents) List(ctx context.Context) ([]agents.Agent, error) { return a.rows(ctx), nil }

func cardsServer(t *testing.T) *Server {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: cardsAgents{}, SessionLiveness: stubLiveness{},
		Mirror: cardsMirror{&fakeMirror{online: true}}})
	return s
}

func cardsAs(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

// chiefLog draws the chief's chat holding reply — owner 1's chief (agent 40),
// or owner 2's (51) under owner 2's principal — through withChatLogLive, the
// wiring every renderer of #chat-log goes through.
func chiefLog(t *testing.T, s *Server, ctx context.Context, reply string) string {
	t.Helper()
	chief := int64(40)
	if requestOwner(ctx) == 2 {
		chief = 51
	}
	v := ui.AgentDetailView{Name: "chief", Status: "running",
		Messages: []ui.ChatLine{{ID: 1, Role: "assistant", Content: reply}}}
	s.withChatLogLive(ctx, &v, agents.ChatSession{AgentID: chief})
	body, _, err := ui.ChatLogBody(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestASessionMarkerResolvesOnlyTheOwnersOwn: in the chief's chat, a marker
// naming one of the OWNER's sessions draws its card (title, computer, status,
// the link that opens it); one naming another owner's session, a team's
// session the owner can see, a team's agent, or a ghost draws the plain
// words "a session" — never a card, and nothing that session's index entry
// holds (its host, its tmux name, its id). The owner's agent is the control
// for the agent kind.
func TestASessionMarkerResolvesOnlyTheOwnersOwn(t *testing.T) {
	s := cardsServer(t)
	html := chiefLog(t, s, cardsAs(1),
		"Mine [[session:mirror:cs-heron-0001]]; agent [[session:agent:41]]; theirs [[session:mirror:cs-lynx-0001]]; "+
			"team [[session:mirror:cs-team-0001]]; team agent [[session:agent:77]]; their agent [[session:agent:50]]; "+
			"ghost [[session:mirror:no-such-0001]].")
	if n := strings.Count(html, `data-session-card="`); n != 2 {
		t.Fatalf("%d cards, want 2 (the owner's session and agent):\n%s", n, html)
	}
	for _, want := range []string{
		`data-session-card="busy"`, `>heron</span>`, `>studio-mac</span>`,
		`href="` + strings.ReplaceAll(ui.MirrorRef{Host: "studio-mac", Session: "heron", Window: "@1"}.ViewPath(), "&", "&amp;") + `"`,
		`href="/agents/fix-login"`,
		"theirs a session; team a session; team agent a session; their agent a session; ghost a session.",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the log lacks %s:\n%s", want, html)
		}
	}
	for _, leak := range []string{"work-laptop", "lynx", "team-box", "team-bot", "theirs<", "cs-lynx", "cs-team", "no-such", "[[session:"} {
		if strings.Contains(html, leak) {
			t.Errorf("the log draws %q:\n%s", leak, html)
		}
	}
}

// TestARawTmuxTargetUpgradesOnlyForTheOwnersOwn: an old reply's raw tmux
// target is the owner's session's card only when it is exactly one of the
// OWNER's; another owner's (or a near miss) is left as written.
func TestARawTmuxTargetUpgradesOnlyForTheOwnersOwn(t *testing.T) {
	s := cardsServer(t)
	for _, c := range []struct {
		name, src string
		cards     int
	}{
		{"own", "on `tmux heron:1 @ studio-mac`", 1},
		{"own, bare", "on tmux heron:1 @ studio-mac.", 1},
		{"own id", "on mirror:cs-heron-0001", 1},
		{"another owner's", "on `tmux lynx:1 @ work-laptop`", 0},
		{"a team's", "on tmux lynx:1 @ team-box", 0},
		{"near miss", "on `tmux heron:2 @ studio-mac`", 0},
	} {
		html := chiefLog(t, s, cardsAs(1), c.src)
		if n := strings.Count(html, `data-session-card="`); n != c.cards {
			t.Errorf("%s: %d cards, want %d:\n%s", c.name, n, c.cards, html)
		}
		// Left as written: its words are all still drawn. (A team window's
		// codename may still be a MENTION link — the mention rule's, which
		// links what the viewer can see — but never a card.)
		if c.cards == 0 {
			for _, w := range strings.Fields(strings.Trim(strings.TrimPrefix(c.src, "on "), "`")) {
				if !strings.Contains(html, w) {
					t.Errorf("%s: %q not drawn as written:\n%s", c.name, w, html)
				}
			}
		}
	}
	// The same text as owner 2: their own target is theirs.
	if html := chiefLog(t, s, cardsAs(2), "on `tmux lynx:1 @ work-laptop`"); strings.Count(html, `data-session-card="waiting"`) != 1 {
		t.Errorf("owner 2's own target is not their card:\n%s", html)
	}
}

// TestOnlyTheChiefsChatDrawsCards: a worker agent's chat (41, not the chief)
// draws no card for the same marker, and leaves its newest reply foldable.
func TestOnlyTheChiefsChatDrawsCards(t *testing.T) {
	s := cardsServer(t)
	v := ui.AgentDetailView{Name: "fix-login", Messages: []ui.ChatLine{{ID: 1, Role: "assistant", Content: "[[session:mirror:cs-heron-0001]]"}}}
	s.withChatLogLive(cardsAs(1), &v, agents.ChatSession{AgentID: 41})
	body, _, err := ui.ChatLogBody(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `data-session-card="`) || strings.Contains(string(body), "data-clamp-never") {
		t.Errorf("a worker's chat draws the chief's cards or unfolds its reply:\n%s", body)
	}
	if !strings.Contains(chiefLog(t, s, cardsAs(1), "x"), "data-clamp-never") {
		t.Error("the chief's newest reply is foldable")
	}
}
