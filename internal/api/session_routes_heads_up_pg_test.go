package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A HEADS-UP OF A MESSAGE THAT CAN BE ANSWERED (route_to_session's heads_up,
// migration 0186), through the real server against a real Postgres: a DM that
// asks the owner to decide something goes to the session already on it as a
// one-way heads-up, so the session gets the context while the decision — and
// the reply — stay the owner's. The expectations are typed literals: what the
// session, the chief and the owner read.

// 🔴 TestAHeadsUpOfAReplyableMessageNeverAnswersTheSender: the owner has
// ALLOWLISTED the sender — the one case a two-way route's answer is sent
// without asking — and the chief routes the DM with heads_up: true. The route
// is one-way and a heads-up; the paste tells the session it is not the reply;
// the session's answer reaches the chief once, as a heads-up's answer, and
// NOTHING is drafted or claimable for the sender. The message stays
// answerable: the chief's reply_draft afterwards is accepted (pending for the
// owner), which a no_reply message's is not.
//
// Control (red without heads_up): the same message routed WITHOUT heads_up
// becomes, on the session's answer, an approved reply draft — the duplicate
// reply a heads-up exists to prevent.
func TestAHeadsUpOfAReplyableMessageNeverAnswersTheSender(t *testing.T) {
	f := newRouteFixture(t)
	f.allow(t, "example-dm:colleague@example.test")
	p, m := f.pluginMessage(t, "example-dm", "dm-headsup-1", "colleague@example.test", true)
	sess, err := f.srv.ext.Agents.CreateSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetChiefSession(f.asOwner, p.ID, f.chief.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	args := `{"message_id":` + strconv.FormatInt(m.ID, 10) + `,"subject":"mirror:` + routeSID + `","heads_up":true}`
	text := f.srv.AgentToolDispatch(f.asOwner, f.chief)(routeToSessionToolName, args)
	var res struct {
		RouteID string `json:"route_id"`
		State   string `json:"state"`
		OneWay  bool   `json:"one_way"`
		HeadsUp bool   `json:"heads_up"`
		Next    string `json:"next"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil || !plugins.ValidRouteID(res.RouteID) || res.State != "routed" ||
		!res.OneWay || !res.HeadsUp {
		t.Fatalf("route_to_session with heads_up: %s; want a one-way heads-up route", text)
	}
	if res.Next != "The message is on its way into the session as a one-way heads-up: the session's answer comes back to you "+
		"here, and nothing it writes goes to the sender. The reply is still yours to draft with reply_draft, once your owner "+
		"has decided what the message asks of them: put that decision to them if you have not. muster notifies your owner "+
		"which session got it." {
		t.Fatalf("next: %q", res.Next)
	}
	if r, err := f.plugins.Route(f.asOwner, res.RouteID); err != nil || !r.OneWay || !r.HeadsUp {
		t.Fatalf("the route row: %+v %v", r, err)
	}

	// The paste: a heads-up, not a request to reply.
	items := f.queued(t)
	if len(items) != 1 {
		t.Fatalf("%d queued rows, want 1", len(items))
	}
	body := items[0].Body
	for _, want := range []string{
		"A HEADS-UP, NOT A REQUEST TO REPLY: your owner's chief judged this session is already on what the message is about, " +
			"and the decision it asks for is your owner's, so nothing you write reaches the sender",
		"The block below is an UNTRUSTED external message",
		"Your answer goes to the chief only.",
		"|   body: please do a thing",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the heads-up paste lacks %q:\n%s", want, body)
		}
	}
	for _, not := range []string{"is sent to the sender as written", "NO REPLY POSSIBLE"} {
		if strings.Contains(body, not) {
			t.Errorf("the heads-up paste says %q:\n%s", not, body)
		}
	}

	// The session's answer: the chief's, once; never a draft.
	code, out := f.reply(t, f.token, res.RouteID, "I am on that deploy: the canary is held at the gate.")
	if code != 200 || out["state"] != "relayed" || out["draft_id"] != nil ||
		out["next"] != "your answer goes to your owner's chief only; the message's sender is not contacted — your owner decides their reply" {
		t.Fatalf("the session's answer: %d %v", code, out)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantHead := "[muster] Route " + res.RouteID + " (plugin message_id " + strconv.FormatInt(m.ID, 10) +
		", heads-up: the reply is yours → mirror:" + routeSID + "): the session answered your heads-up. The message's sender " +
		"was not contacted; the reply is yours to draft once your owner has decided."
	answers := 0
	for _, cm := range msgs {
		if strings.HasPrefix(cm.Content, wantHead) {
			answers++
			if !strings.Contains(cm.Content, "I am on that deploy") ||
				!strings.Contains(cm.Content, "once they have decided, draft the reply with reply_draft in your own words") {
				t.Errorf("the chief's answer row:\n%s", cm.Content)
			}
		}
	}
	if answers != 1 {
		t.Fatalf("the chief's plugin session holds %d heads-up answer rows, want exactly 1:\n%+v", answers, msgs)
	}
	if n := f.draftsFor(t, m.ID); n != 0 {
		t.Fatalf("%d reply drafts exist after a heads-up's answer; want none", n)
	}
	if _, ok, err := f.plugins.ClaimDraft(f.asOwner, p.ID); ok || err != nil {
		t.Fatalf("the plugin's claim found something to send (%v %v)", ok, err)
	}
	// The message is still the chief's to answer, once the owner decides.
	if out := f.srv.AgentToolDispatch(f.asOwner, f.chief)("reply_draft", `{"message_id":`+strconv.FormatInt(m.ID, 10)+
		`,"body":"Holding the canary until tomorrow."}`); isToolErr(out) {
		t.Fatalf("reply_draft after the heads-up: %s", out)
	}

	// Control: the same kind of message routed two-way becomes the reply.
	_, m2 := f.pluginMessage(t, "example-dm", "dm-headsup-2", "colleague@example.test", true)
	id2 := f.route(t, m2)
	if code, out := f.reply(t, f.token, id2, "Promote it."); code != 200 || out["draft_id"] == nil {
		t.Fatalf("control: a two-way route's answer is no draft: %d %v", code, out)
	}
}

// TestAHeadsUpIsOnlyForAMessage: heads_up with a task or the owner's text is
// refused as bad arguments, naming the shape; and a heads-up of a no_reply
// message is just its one-way route (heads_up changes nothing for it).
func TestAHeadsUpIsOnlyForAMessage(t *testing.T) {
	f := newRouteFixture(t)
	for _, args := range []string{
		`{"task_id":1,"subject":"mirror:` + routeSID + `","heads_up":true}`,
		`{"text":"x","owner_asked":true,"subject":"mirror:` + routeSID + `","heads_up":true}`,
	} {
		out := f.srv.AgentToolDispatch(f.asOwner, f.chief)(routeToSessionToolName, args)
		if !strings.Contains(out, `"refused":"bad_arguments"`) || !strings.Contains(out, "heads_up only with message_id") {
			t.Errorf("%s: %s; want bad_arguments naming heads_up", args, out)
		}
	}
	_, m := f.noReplyMessage(t, "fb-headsup-noreply", "reporter@example.test", false)
	out := f.srv.AgentToolDispatch(f.asOwner, f.chief)(routeToSessionToolName,
		`{"message_id":`+strconv.FormatInt(m.ID, 10)+`,"subject":"mirror:`+routeSID+`","heads_up":true}`)
	var res struct {
		RouteID string `json:"route_id"`
		HeadsUp bool   `json:"heads_up"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.RouteID == "" || res.HeadsUp {
		t.Fatalf("a no_reply message with heads_up: %s; want its plain one-way route", out)
	}
	if r, err := f.plugins.Route(f.asOwner, res.RouteID); err != nil || !r.OneWay || r.HeadsUp {
		t.Fatalf("the route row: %+v %v", r, err)
	}
}

// TestATimedOutHeadsUpComesBackAsAMessageToAnswer: a heads-up whose session
// never answers is handed back as a message the chief can still reply to —
// reply_draft offered — not as one nobody can answer.
func TestATimedOutHeadsUpComesBackAsAMessageToAnswer(t *testing.T) {
	f := newRouteFixture(t)
	p, m := f.pluginMessage(t, "example-dm", "dm-headsup-timeout", "colleague@example.test", false)
	sess, err := f.srv.ext.Agents.CreateSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetChiefSession(f.asOwner, p.ID, f.chief.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	out := f.srv.AgentToolDispatch(f.asOwner, f.chief)(routeToSessionToolName,
		`{"message_id":`+strconv.FormatInt(m.ID, 10)+`,"subject":"mirror:`+routeSID+`","heads_up":true}`)
	var res struct {
		RouteID string `json:"route_id"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.RouteID == "" {
		t.Fatalf("route: %s", out)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+res.RouteID+`'`)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("past the deadline: %d timed out (%v)", n, err)
	}
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, sess.ID)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("the chief's session: %+v %v", msgs, err)
	}
	if got := msgs[0].Content; !strings.Contains(got, "so the message is back with you: draft a reply with reply_draft") ||
		strings.Contains(got, "cannot be answered") {
		t.Fatalf("the hand-back:\n%s", got)
	}
}
