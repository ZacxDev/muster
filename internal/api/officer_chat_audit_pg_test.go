package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE #864 AUDIT'S THREE DEFECTS, each pinned through the real server and
// stores (outboundWorld) and named only by what origin's branch already had
// before the fix — the gateway's captured turn, POST /mcp, the routes and the
// rows — so each test is shown red on the code before it, not merely absent.

// chatTurnOf sends msg on the officer page of officer (owner query q) as user
// 1 and waits for the turn: the turn the gateway took.
func (w *outboundWorld) chatTurnOf(gw *laneChat, officer, q, msg string) laneTurn {
	w.t.Helper()
	before := len(gw.sent())
	rec := w.post("/officers/"+officer+"/chat"+q, url.Values{"message": {msg}})
	if rec.Code != http.StatusSeeOther {
		w.t.Fatalf("send to %s%s: %d %.300s", officer, q, rec.Code, rec.Body.String())
	}
	w.srv.WaitForOfficerChats()
	sent := gw.sent()
	if len(sent) != before+1 {
		w.t.Fatalf("send to %s%s: %d turns went out, want 1 (page: %.600s)", officer, q, len(sent)-before, chatSection(w.officerPageOf(officer, q)))
	}
	return sent[len(sent)-1]
}

// officerPageOf is officer's page (owner query q) as user 1 reads it.
func (w *outboundWorld) officerPageOf(officer, q string) string {
	w.t.Helper()
	rec := w.do("GET", "/officers/"+officer+q, "", func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieFor(w.srv))
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
	})
	return rec.Body.String()
}

// mcpAs is one tools/call on POST /mcp as instance a, carrying session and
// secret as a chat lane's CLI sends them: the tool's text.
func (w *outboundWorld) mcpAs(a agents.Agent, session, secret, tool, args string) string {
	w.t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(agents.HeaderCCSession, session)
	req.Header.Set(agents.HeaderCCLaneSecret, secret)
	// As requireConnectorAgent admits the instance's own token: the agent,
	// acting as its owner through the agent door.
	ctx := context.WithValue(req.Context(), agentCtxKey{}, a)
	if p, ok := w.srv.agentPrincipal(ctx, a, authz.ViaAgent); ok {
		ctx = authz.WithPrincipal(ctx, p)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	w.srv.handleMCP(rec, req)
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Result.Content) == 0 {
		w.t.Fatalf("POST /mcp %s: %d %s", tool, rec.Code, rec.Body.String())
	}
	return resp.Result.Content[0].Text
}

// TestAReplayedLaneSecretOnlyProposes: the lane secret of user 1's chat is
// written into the instance pod, where every process can read it. Replayed
// from a call that is NOT the chat (the primary's own MCP connection, naming
// the chat's session and secret), officer_refusal_drop and
// officer_refusal_refile change NOTHING: the refusal still waits on its
// owner, undecided. CONTROL: the same refusal is decided when user 1 taps the
// card the replay drew — so the refusal was decidable, and the call reached
// the tool.
func TestAReplayedLaneSecretOnlyProposes(t *testing.T) {
	gw := &laneChat{reply: func() (string, error) { return "ok", nil }}
	w := newOutboundWorldWith(t, true, outboundFixture{gateway: gw})
	wipeCards(t)
	t.Cleanup(func() { wipeCards(t) })
	turn := w.chatTurnOf(gw, builtinOfficerID, "", "hello")
	_, refusal := w.refusedDraft("A refused post.", "the window is closed")
	item := strconv.FormatInt(refusal, 10)
	for _, c := range []struct{ tool, args string }{
		{officers.ToolRefusalDrop, `{"item_id":` + item + `}`},
		{officers.ToolRefusalRefile, `{"item_id":` + item + `,"when":"1h"}`},
	} {
		out := w.mcpAs(w.agent, turn.sessionKey, turn.laneSecret, c.tool, c.args)
		if it := w.itemFull(refusal); it.payload["owner_decision"] != nil || it.status != officers.StatusOpen || it.origin == officers.OriginOwner {
			t.Fatalf("a replayed %s decided the refusal: %+v (tool said %s)", c.tool, it, out)
		}
	}
	// CONTROL: the person's own click on the drawn drop card.
	var dropCard cardRow
	for _, c := range w.cards() {
		if c.kind == "drop" && c.decision == "" {
			dropCard = c
		}
	}
	if dropCard.id == 0 {
		t.Fatalf("the replay drew no drop card: %+v", w.cards())
	}
	if rec := w.cardPost(dropCard.id, "approve", url.Values{"card_sha256": {dropCard.sha}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: the person's tap: %d %.300s", rec.Code, rec.Body.String())
	}
	if it := w.itemFull(refusal); it.payload["owner_decision"] != "drop" {
		t.Fatalf("control: after the person's tap: %+v", it)
	}
}

// TestAChatRefileStaysUnderTheBudget: a re-file asked for in the chat and
// confirmed by the person comes back as the OFFICER's work — never with the
// owner's origin, which the budget hold lets through — and is re-filed
// (owner_decision refile).
func TestAChatRefileStaysUnderTheBudget(t *testing.T) {
	gw := &laneChat{reply: func() (string, error) { return "ok", nil }}
	w := newOutboundWorldWith(t, true, outboundFixture{gateway: gw})
	wipeCards(t)
	t.Cleanup(func() { wipeCards(t) })
	turn := w.chatTurnOf(gw, builtinOfficerID, "", "please re-file the refused one")
	_, refusal := w.refusedDraft("A refused post.", "the window is closed")
	if before := w.itemFull(refusal); before.origin == officers.OriginOwner {
		t.Fatalf("fixture: the refusal already has the owner's origin")
	}
	w.mcpAs(w.agent, turn.sessionKey, turn.laneSecret, officers.ToolRefusalRefile, `{"item_id":`+strconv.FormatInt(refusal, 10)+`,"when":"1h"}`)
	for _, c := range w.cards() {
		if c.kind == "refile" && c.decision == "" {
			if rec := w.cardPost(c.id, "approve", url.Values{"card_sha256": {c.sha}}); rec.Code != http.StatusSeeOther {
				t.Fatalf("the person's tap: %d %.300s", rec.Code, rec.Body.String())
			}
		}
	}
	it := w.itemFull(refusal)
	if it.payload["owner_decision"] != "refile" {
		t.Fatalf("the chat's re-file was not made: %+v", it)
	}
	if it.origin == officers.OriginOwner {
		t.Fatalf("the chat's re-file carries the owner's origin, which gets past the budget hold: %+v", it)
	}
}

// upProv is a provisioner whose instances named in up are running.
type upProv struct {
	stubProvisioner
	mu sync.Mutex
	up map[string]bool
}

func (p *upProv) set(names ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.up = map[string]bool{}
	for _, n := range names {
		p.up[n] = true
	}
}

func (p *upProv) Instances(context.Context) ([]provision.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []provision.Instance
	for n := range p.up {
		out = append(out, provision.Instance{Ref: provision.Ref{Name: n}, Phase: provision.PhaseRunning, Ready: true})
	}
	return out, nil
}

// chatFailed reports the chat section's failure line, "" for none.
func chatFailed(section string) string {
	i := strings.Index(section, "data-chat-failed>")
	if i < 0 {
		return ""
	}
	rest := section[i+len("data-chat-failed>"):]
	if j := strings.Index(rest, "<"); j >= 0 {
		rest = rest[:j]
	}
	return html.UnescapeString(rest)
}

// TestTwoOfficersOfOneOwnerEachChat: user 1 chats with two officers of their
// own; each turn goes out on that officer's own instance, in a session of
// its own, and neither page says it failed.
func TestTwoOfficersOfOneOwnerEachChat(t *testing.T) {
	const second = "marketing:second"
	gw := &laneChat{reply: func() (string, error) { return "ok", nil }}
	w := newOutboundWorldWith(t, true, outboundFixture{gateway: gw, moreOfficers: func(*testing.T, *connFixture) string {
		return `,{"id":"` + second + `","backend":"builtin"}`
	}})
	t.Cleanup(func() {
		_, _ = w.f.pool.Exec(dbtest0(w), `DELETE FROM agents WHERE name LIKE 'officer-marketing-second%'`)
	})
	a := w.chatTurnOf(gw, builtinOfficerID, "", "first officer")
	b := w.chatTurnOf(gw, second, "", "second officer")
	if a.sessionKey == b.sessionKey || !strings.Contains(b.agent, "second") {
		t.Fatalf("the two chats: %+v and %+v; want two sessions, the second on the second officer's instance", a, b)
	}
	if f := chatFailed(chatSection(w.officerPageOf(second, ""))); f != "" {
		t.Fatalf("the second officer's chat failed: %q", f)
	}
}

// TestAChatMovesToARunningInstance: a chat whose instance (1) is no longer
// running goes to the instance that is (2), and answers there.
func TestAChatMovesToARunningInstance(t *testing.T) {
	prov := &upProv{}
	gw := &laneChat{reply: func() (string, error) { return "ok", nil }}
	w := newOutboundWorldWith(t, true, outboundFixture{officerExtra: `,"max_instances":2`, provisioner: prov, gateway: gw})
	first := w.chatTurnOf(gw, builtinOfficerID, "", "on instance one")
	o, err := w.srv.officerRT.reg.Get(builtinOfficerID)
	if err != nil {
		t.Fatal(err)
	}
	if first.agent != o.InstanceName(1) {
		t.Fatalf("the first turn went to %s, want %s", first.agent, o.InstanceName(1))
	}
	startPoolInstance(t, w.f, w.srv, builtinOfficerID, "", 2, "fixture-chat-instance-two")
	// Instance 1 stopped (its row says so, and the provisioner has no pod
	// for it); instance 2 running.
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `UPDATE agents SET status = 'stopped' WHERE name = $1`, o.InstanceName(1)); err != nil {
		t.Fatal(err)
	}
	prov.set(o.InstanceName(2))
	next := w.chatTurnOf(gw, builtinOfficerID, "", "instance one is gone")
	if next.agent != o.InstanceName(2) {
		t.Fatalf("with only instance 2 running the turn went to %s", next.agent)
	}
	if f := chatFailed(chatSection(w.officerPage())); f != "" {
		t.Fatalf("the moved chat failed: %q", f)
	}
}

// TestAPersonalAndATeamOfficerEachChat: user 1 chats with their own officer
// and with an officer of a team they manage; both answer. (MUSTER_OFFICERS
// refuses one id declared twice, whatever the owners.)
func TestAPersonalAndATeamOfficerEachChat(t *testing.T) {
	const teamOfficer = "marketing:team"
	var team int64
	gw := &laneChat{reply: func() (string, error) { return "ok", nil }}
	w := newOutboundWorldWith(t, true, outboundFixture{gateway: gw, moreOfficers: func(t *testing.T, f *connFixture) string {
		team = newTeam(t, f.pool, 1)
		return `,{"id":"` + teamOfficer + `","backend":"builtin","owner":` + strconv.FormatInt(team, 10) + `}`
	}})
	q := "?owner=" + strconv.FormatInt(team, 10)
	mine := w.chatTurnOf(gw, builtinOfficerID, "", "my own officer")
	theirs := w.chatTurnOf(gw, teamOfficer, q, "the team's officer")
	if mine.sessionKey == theirs.sessionKey || mine.agent == theirs.agent {
		t.Fatalf("the personal and the team chat: %+v and %+v; want two sessions on two instances", mine, theirs)
	}
	if f := chatFailed(chatSection(w.officerPageOf(teamOfficer, q))); f != "" {
		t.Fatalf("the team officer's chat failed: %q", f)
	}
}

// TestABatchCardRejectNeedsARecentSignIn: rejecting a batch card rejects every
// entry, so a stale sign-in is refused (403), as the Asks page's batch
// decision is, and nothing is decided. CONTROL: a fresh sign-in rejects it.
func TestABatchCardRejectNeedsARecentSignIn(t *testing.T) {
	w := newOutboundWorld(t, true)
	wipeCards(t)
	t.Cleanup(func() { wipeCards(t) })
	ids, _ := w.followsFor("lu")
	batch := plugins.BatchID(1, builtinOfficerID, w.draft(ids[0]).CreatedAt.UTC().Format("2006-01-02"))
	card := w.cardFor(`{"batch_id":"` + batch + `"}`)
	sha := w.cards()[0].sha
	path := tChatPath + "/cards/" + strconv.FormatInt(card, 10) + "/reject"
	rec := w.do("POST", path, url.Values{"card_sha256": {sha}}.Encode(), func(r *http.Request) {
		w.operatorForm(r)
		r.Header.Set("Cookie", staleCookieFor(w.srv))
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a stale batch reject: %d; want 403", rec.Code)
	}
	if got := w.draft(ids[0]).Status; got != plugins.DraftPending {
		t.Fatalf("a stale batch reject decided the entry: %s", got)
	}
	if rec := w.post(path, url.Values{"card_sha256": {sha}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("control: a fresh batch reject: %d %.300s", rec.Code, rec.Body.String())
	}
	if got := w.draft(ids[0]).Status; got != plugins.DraftRejected {
		t.Fatalf("control: after a fresh batch reject: %s", got)
	}
}

// dbtest0 is a system context for cleanup.
func dbtest0(w *outboundWorld) context.Context { return authz.System(context.Background()) }
