package api

import (
	"context"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/itemorigin"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/ui"
)

// "SHOW SESSION" ON AN OFFICER'S NEEDS-YOU ITEMS (migration 0184,
// officer_origin.go), through the real server, stores and dispatcher: the
// instance's own tool calls file a draft, ask its owner, and escalate; each
// row records the instance, its session and the moment FROM THE BOUND CALLER;
// the officer page draws "Show session" to exactly that; the agent page opens
// it at the moment; an older item opens the session as a whole; another
// owner is never linked or let in; a caller-supplied session is ignored.

// originWorld is an outbound world whose instance has a session with two
// messages (a turn's prompt and reply): the moment its calls record is the
// reply.
type originWorld struct {
	*outboundWorld
	session        int64
	prompt, moment int64
}

func newOriginWorld(t *testing.T) originWorld {
	t.Helper()
	w := newOutboundWorld(t, true)
	ag := w.srv.ext.Agents
	sess, err := ag.LatestOrCreateSession(w.ctx, w.agent.ID, w.agent.Name)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ag.AddChatMessage(w.ctx, agents.ChatMessage{AgentID: w.agent.ID, SessionID: sess.ID, Role: "user", Content: "New work for the officer."})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ag.AddChatMessage(w.ctx, agents.ChatMessage{AgentID: w.agent.ID, SessionID: sess.ID, Role: "assistant", Content: "Read the queue; one reply needs the owner."})
	if err != nil {
		t.Fatal(err)
	}
	return originWorld{outboundWorld: w, session: sess.ID, prompt: p.ID, moment: m.ID}
}

// want is the origin the world's instance's calls must record.
func (w originWorld) want() itemorigin.Origin {
	return itemorigin.Origin{AgentID: w.agent.ID, SessionID: w.session, MessageID: w.moment}
}

// askOrigin is ask id's recorded origin (zero for none).
func (w originWorld) askOrigin(id int64) itemorigin.Origin {
	w.t.Helper()
	m, err := w.store.AskOrigins(authz.System(context.Background()), 1, []int64{id})
	if err != nil {
		w.t.Fatal(err)
	}
	return m[id]
}

func sameOrigin(got, want itemorigin.Origin) bool {
	return got.AgentID == want.AgentID && got.SessionID == want.SessionID && got.MessageID == want.MessageID && !got.At.IsZero()
}

// ownerAsks is user 1's open asks raised as the officer.
func (w originWorld) ownerAsks() []attention.Ask {
	w.t.Helper()
	list, err := w.srv.ext.Attention.Open(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession}))
	if err != nil {
		w.t.Fatal(err)
	}
	return raisedByOfficer(list, 1, builtinOfficerID)
}

// TestAnOfficersAskDraftRefusalAndEscalationRecordWhereTheyWereMade (PR B
// (2)): a draft (officer_post_draft), an ask (officer_ask_person) and an
// escalation (officer_complete "escalated", raised by the sweep) made by the
// instance's own calls each record the instance, its session and the moment
// — the session's newest message at the call — and a time; the plugin's
// refusal of the draft reaches its draft's. A row no officer call made (the
// control: an ask made outside the dispatcher) records none.
func TestAnOfficersAskDraftRefusalAndEscalationRecordWhereTheyWereMade(t *testing.T) {
	w := newOriginWorld(t)
	draft, _ := w.refusedDraft("A post the plugin refuses.", "cadence: not today")
	d, err := w.ps.Draft(w.ctx, draft)
	if err != nil || !sameOrigin(d.Origin, w.want()) {
		t.Fatalf("the draft's origin %+v (%v); want %+v", d.Origin, err, w.want())
	}

	plan := w.claimedPlan("origin:ask", `{}`)
	out := w.call(officers.ToolAskPerson, `{"item_id":"`+strconv.FormatInt(plan, 10)+`","to":"owner","question":"Which repository first?","options":["web","api"]}`)
	if isToolErr(out) {
		t.Fatalf("ask: %s", out)
	}
	asks := w.ownerAsks()
	if len(asks) != 1 || !sameOrigin(w.askOrigin(asks[0].ID), w.want()) {
		t.Fatalf("the ask's origin: %d asks, %+v; want one ask from %+v", len(asks), w.askOrigin(asks[0].ID), w.want())
	}

	esc := w.claimedPlan("origin:escalate", `{}`)
	if out := w.call(officers.ToolComplete, `{"item_id":"`+strconv.FormatInt(esc, 10)+`","outcome":"escalated","summary":"This needs the owner."}`); isToolErr(out) {
		t.Fatalf("escalate: %s", out)
	}
	w.srv.SweepOfficerAttention(context.Background())
	var notice *attention.Ask
	for _, a := range w.ownerAsks() {
		if strings.Contains(a.Question, "This needs the owner.") {
			a := a
			notice = &a
		}
	}
	if notice == nil || !sameOrigin(w.askOrigin(notice.ID), w.want()) {
		t.Fatalf("the escalation's ask: %+v; want it from %+v", notice, w.want())
	}

	// CONTROL: an ask made outside an officer's call records no origin.
	plain, err := w.srv.ext.Attention.Create(w.srv.attentionPrincipal(context.Background(), 1), attention.New{
		Asker: attention.Asker{System: true, Label: officerAskLabel(builtinOfficerID)}, Question: "made by muster itself", Timeout: 3600e9})
	if err != nil || w.askOrigin(plain.ID) != (itemorigin.Origin{}) {
		t.Fatalf("control: an ask outside a call has origin %+v (%v)", w.askOrigin(plain.ID), err)
	}
}

// showSessionHrefs is every "Show session" link in page, with its kind.
func showSessionHrefs(page string) map[string]string {
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`<a href="([^"]*)" class="[^"]*" data-show-session="(moment|whole)">Show session</a>`).FindAllStringSubmatch(page, -1) {
		out[html.UnescapeString(m[1])] = m[2]
	}
	return out
}

// TestShowSessionOpensTheSessionAtTheMoment (PR B (3) and (4)): the officer
// page's Needs-you draws "Show session" on the draft, the ask and the
// refusal, each linking /agents/<instance>?session=<s>&at=<moment>; the agent
// page opened there draws its moment marker right after the moment's message
// and before the next. An item with no origin (made before 0184) links the
// instance's session as a whole: no at=, no marker.
func TestShowSessionOpensTheSessionAtTheMoment(t *testing.T) {
	w := newOriginWorld(t)
	w.refusedDraft("A refused post.", "cadence: not today")
	pending, _ := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"x","target":{"kind":"post"},"body":"A post to approve."}`)
	if pending == 0 {
		t.Fatal("no pending draft")
	}
	if rec := w.check(w.tok, pending, true, ""); rec.Code != http.StatusOK {
		t.Fatalf("pass the plugin's check: %d", rec.Code)
	}
	plan := w.claimedPlan("origin:show", `{}`)
	if out := w.call(officers.ToolAskPerson, `{"item_id":"`+strconv.FormatInt(plan, 10)+`","to":"owner","question":"Ship it?","options":["yes","no"]}`); isToolErr(out) {
		t.Fatalf("ask: %s", out)
	}
	moment := ui.AgentMomentHref(w.agent.Name, w.session, w.moment)
	if moment != "/agents/"+w.agent.Name+"?session="+strconv.FormatInt(w.session, 10)+"&at="+strconv.FormatInt(w.moment, 10)+"#session-moment" {
		t.Fatalf("the moment's href is %q", moment)
	}
	sec := needsYou(w.officerPage())
	links := showSessionHrefs(sec)
	if links[moment] != "moment" {
		t.Fatalf("Needs you has no Show session to the moment; it has %v\n%.3000s", links, sec)
	}
	for _, what := range []string{`data-draft-id="` + strconv.FormatInt(pending, 10) + `"`, `data-refusal=`, `data-attention-text`} {
		i := strings.Index(sec, what)
		if i < 0 {
			t.Fatalf("Needs you lacks %s", what)
		}
		card := sec[i:]
		if j := strings.Index(card, "</li>"); j > 0 && !strings.HasPrefix(what, "data-refusal") {
			card = card[:j]
		} else if j := strings.Index(card, "</article>"); j > 0 {
			card = card[:j]
		}
		if !strings.Contains(card, `href="`+html.EscapeString(moment)+`"`) {
			t.Errorf("the card at %s has no Show session to the moment:\n%s", what, card)
		}
	}

	// The agent page at the moment: the marker after the moment's message.
	page := w.do("GET", strings.TrimSuffix(moment, "#session-moment"), "", func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieFor(w.srv))
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
	})
	if page.Code != http.StatusOK {
		t.Fatalf("the agent page at the moment: %d", page.Code)
	}
	body := page.Body.String()
	marker := strings.Index(body, `data-session-moment="`+strconv.FormatInt(w.moment, 10)+`"`)
	reply := strings.Index(body, `data-msg-id="`+strconv.FormatInt(w.moment, 10)+`"`)
	if marker < 0 || reply < 0 || marker < reply || !strings.Contains(body, `id="session-moment"`) {
		t.Fatalf("the agent page's moment: marker at %d, the moment's message at %d (want the marker after it)", marker, reply)
	}

	// An older item: its origin cleared, as every row before 0184.
	w.draftSQL(pending, `origin_agent_id = NULL, origin_session_id = NULL, origin_message_id = NULL, origin_at = NULL`)
	whole := ui.AgentSessionHref(w.agent.Name, w.session)
	sec = needsYou(w.officerPage())
	i := strings.Index(sec, `data-draft-id="`+strconv.FormatInt(pending, 10)+`"`)
	card := sec[i:]
	card = card[:strings.Index(card, "</li>")]
	if !strings.Contains(card, `href="`+html.EscapeString(whole)+`" class="`+ui.OfficerShowSessionClass+`" data-show-session="whole"`) || strings.Contains(card, "at=") {
		t.Fatalf("an older draft's Show session is not its instance's session as a whole (%s):\n%s", whole, card)
	}
	wholePage := w.do("GET", whole, "", func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieFor(w.srv))
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
	}).Body.String()
	if strings.Contains(wholePage, "data-session-moment") {
		t.Fatal("the session as a whole draws a moment marker")
	}
}

// TestShowSessionNeverOpensAnotherOwnersSession (PR B (5)): the link is drawn
// only to the item owner's officer's own instance — a draft whose origin is
// the owner's OTHER (non-officer) agent draws none — and another owner who
// follows the moment's link gets the page a missing agent gets (404); the
// database refuses an origin naming another owner's agent outright.
func TestShowSessionNeverOpensAnotherOwnersSession(t *testing.T) {
	w := newOriginWorld(t)
	id, _ := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"x","target":{"kind":"post"},"body":"Mine."}`)
	if rec := w.check(w.tok, id, true, ""); rec.Code != http.StatusOK {
		t.Fatalf("check: %d", rec.Code)
	}
	moment := ui.AgentMomentHref(w.agent.Name, w.session, w.moment)
	if showSessionHrefs(needsYou(w.officerPage()))[moment] != "moment" {
		t.Fatal("control: the owner's own draft has no Show session")
	}
	// Another owner follows the link: the page a missing agent gets.
	w.srv.sessionUserOverride = func(r *http.Request) int64 {
		if v, err := strconv.ParseInt(r.Header.Get(pageUserHeader), 10, 64); err == nil && v > 0 {
			return v
		}
		return authz.OperatorUserID
	}
	asOther := w.do("GET", strings.TrimSuffix(moment, "#session-moment"), "", func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieFor(w.srv))
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set(pageUserHeader, strconv.FormatInt(w.other, 10))
	})
	if asOther.Code != http.StatusNotFound || strings.Contains(asOther.Body.String(), "Read the queue") {
		t.Fatalf("another owner opening the moment: %d", asOther.Code)
	}
	// The owner's own NON-officer agent as the origin: no link is drawn.
	var plain int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'origin-plain-agent', 'ns-origin-plain') RETURNING id`).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, plain)
	})
	w.draftSQL(id, `origin_agent_id = `+strconv.FormatInt(plain, 10)+`, origin_session_id = NULL, origin_message_id = NULL`)
	if links := showSessionHrefs(needsYou(w.officerPage())); len(links) != 0 {
		t.Fatalf("a draft whose origin is not one of the officer's instances links %v", links)
	}
	// Another owner's agent as the origin: refused by the database.
	var theirs int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `INSERT INTO agents (owner_id, name, namespace) VALUES ($1, 'origin-their-agent', 'ns-origin-their') RETURNING id`, w.other).Scan(&theirs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, theirs)
	})
	if _, err := w.f.pool.Exec(authz.System(w.ctx), `UPDATE plugin_reply_drafts SET origin_agent_id = $1 WHERE id = $2`, theirs, id); err == nil {
		t.Fatal("the database took another owner's agent as a draft's origin")
	}
}

// TestACallerSuppliedSessionIsIgnored (PR B (6)): an instance that names an
// origin in its arguments — another agent, another session, a message — gets
// the same origin as without them: its own (the bound caller's), and no row
// records what it named.
func TestACallerSuppliedSessionIsIgnored(t *testing.T) {
	w := newOriginWorld(t)
	var foreign int64
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'origin-forged', 'ns-origin-forged') RETURNING id`).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, foreign)
	})
	forged := `"origin_agent_id":` + strconv.FormatInt(foreign, 10) + `,"origin_session_id":1,"origin_message_id":1,"session":"1","session_id":1,"at":"1"`

	// Both tools TAKE a call with unknown keys (their arguments are decoded
	// leniently), so the forged values reach the handler: the case this pins.
	id, out := w.postDraft(`{"plugin":"` + outboundPluginName + `","venue":"x","target":{"kind":"post"},"body":"Forged.",` + forged + `}`)
	if id == 0 {
		t.Fatalf("post_draft refused the forged keys (%s): the guard is not exercised", out)
	}
	d, err := w.ps.Draft(w.ctx, id)
	if err != nil || !sameOrigin(d.Origin, w.want()) {
		t.Fatalf("a draft filed with a forged origin records %+v (%v); want its caller's %+v", d.Origin, err, w.want())
	}
	plan := w.claimedPlan("origin:forged", `{}`)
	if out := w.call(officers.ToolAskPerson, `{"item_id":"`+strconv.FormatInt(plan, 10)+`","to":"owner","question":"Forged?",`+forged+`}`); isToolErr(out) {
		t.Fatalf("ask_person refused the forged keys (%s): the guard is not exercised", out)
	}
	if asks := w.ownerAsks(); len(asks) != 1 || !sameOrigin(w.askOrigin(asks[0].ID), w.want()) {
		t.Fatalf("an ask with a forged origin records %+v; want its caller's %+v", asks, w.want())
	}
	// Nothing anywhere names the forged agent.
	var n int
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT (SELECT count(*) FROM attention_asks WHERE origin_agent_id = $1) + (SELECT count(*) FROM plugin_reply_drafts WHERE origin_agent_id = $1)`, foreign).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows name the forged agent (%v)", n, err)
	}
}
