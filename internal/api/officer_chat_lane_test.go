package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// A CHAT LANE'S OWN TOOL CALLS (officer_chat.go, docs/officers.md "Chat
// sessions"). A lane's CLI calls muster's MCP server as its instance's agent
// — the same token as the primary — and says which session it is with
// agents.HeaderCCSession on every request: its ccd starts it with an MCP
// config of its own carrying that header (cmd/ccd lanes.go, "A CHAT LANE
// SAYS WHO IT IS"). These tests drive POST /mcp itself, with and without the
// header, so they pin the door a lane's call really takes.

// laneMCPCall is one tools/call on POST /mcp as agent a; session, when set, is
// the X-Muster-CC-Session the request carries. It answers the tool's text.
func laneMCPCall(t *testing.T, r *officerRig, a agents.Agent, session, tool, args string) string {
	t.Helper()
	return laneMCPCallWith(t, r, a, session, laneSecretOf(r, a, session), tool, args)
}

// laneMCPCallWith is laneMCPCall with secret as the call's
// X-Muster-CC-Lane-Secret ("" sends none).
func laneMCPCallWith(t *testing.T, r *officerRig, a agents.Agent, session, secret, tool, args string) string {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set(agents.HeaderCCSession, session)
	}
	if secret != "" {
		req.Header.Set(agents.HeaderCCLaneSecret, secret)
	}
	req = req.WithContext(context.WithValue(req.Context(), agentCtxKey{}, a))
	w := httptest.NewRecorder()
	r.s.handleMCP(w, req)
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Error != nil || len(resp.Result.Content) == 0 {
		t.Fatalf("POST /mcp %s: %d %s", tool, w.Code, w.Body.String())
	}
	return resp.Result.Content[0].Text
}

// laneRig is the kube rig's judgement instance with chat c-41's item held by
// its chat holder (as the chat runner holds it).
func laneRig(t *testing.T) (*officerRig, agents.Agent, string) {
	t.Helper()
	r := newKubeOfficerRig(t)
	judge := r.addNamed(offKJudge+"1", 2971)
	chat := r.hq.AddTieredItem(offK, officers.ItemKindChat, "chat:c-41", officers.TierJudgement)
	r.hq.SetPayload(chat, `{"chat_id":"c-41","seq":1}`)
	r.hq.SetClaim(chat, officers.ChatHolder(judge.Name), time.Now().Add(10*time.Minute))
	setLaneSecret(r, judge, officers.ChatSessionKey("c-41"), "fixture-lane-secret-c41-7d2e")
	return r, judge, chat
}

// rawCalls is the obs/raw bodies HQ received, as {item_id, instance}.
func rawCalls(r *officerRig) []string {
	var out []string
	for _, c := range r.hq.ObsRawCalls {
		out = append(out, fmt.Sprintf("%v/%v", c.Body["item_id"], c.Body["instance"]))
	}
	return out
}

// TestAChatLanesReadIsItsChatItemsWithThePrimaryIdle: the primary holds
// nothing; the lane's counted read (officer_obs_raw) is NOT refused for lack
// of a queue claim — it is HQ's call for the chat's item, named by its holder,
// and charged to that item's read budget.
func TestAChatLanesReadIsItsChatItemsWithThePrimaryIdle(t *testing.T) {
	r, judge, chat := laneRig(t)
	out := laneMCPCall(t, r, judge, officers.ChatSessionKey("c-41"), obsRawTool, rawArgs)
	if isToolErr(out) {
		t.Fatalf("a chat lane's read with the primary idle was refused: %s", out)
	}
	want := fmt.Sprintf("%s/%s", chat, officers.ChatHolder(judge.Name))
	if got := rawCalls(r); len(got) != 1 || got[0] != want {
		t.Fatalf("HQ's obs/raw calls %v; want one for the chat item as its holder (%s)", got, want)
	}
	if n := r.s.officerRT.reads.Spent(offK, officers.ItemID(chat)); n != 1 {
		t.Fatalf("the chat item's read budget spent %d, want 1", n)
	}
}

// TestAChatLanesReadIsNeverChargedToThePrimarysItem: the primary holds exactly
// one queue item (which is what a lane's read used to be charged to); the
// lane's read is the chat item's, and the queue item's budget is untouched.
// The control: the same call with no session header is the primary's, and is
// the queue item's.
func TestAChatLanesReadIsNeverChargedToThePrimarysItem(t *testing.T) {
	r, judge, chat := laneRig(t)
	queue := r.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	r.hq.SetClaim(queue, judge.Name, time.Now().Add(10*time.Minute))
	if out := laneMCPCall(t, r, judge, officers.ChatSessionKey("c-41"), obsRawTool, rawArgs); isToolErr(out) {
		t.Fatalf("the lane's read: %s", out)
	}
	if got, want := rawCalls(r), fmt.Sprintf("%s/%s", chat, officers.ChatHolder(judge.Name)); len(got) != 1 || got[0] != want {
		t.Fatalf("the lane's read reached HQ as %v; want %s (never the primary's queue item %s)", got, want, queue)
	}
	if n := r.s.officerRT.reads.Spent(offK, officers.ItemID(queue)); n != 0 {
		t.Fatalf("the primary's queue item was charged %d read(s) for the lane's call", n)
	}
	if out := laneMCPCall(t, r, judge, "", obsRawTool, rawArgs); isToolErr(out) {
		t.Fatalf("control: the primary's read: %s", out)
	}
	if got, want := rawCalls(r), fmt.Sprintf("%s/%s", queue, judge.Name); len(got) != 2 || got[1] != want {
		t.Fatalf("control: the primary's read reached HQ as %v; want %s", got, want)
	}
}

// TestAChatLaneCannotTakeQueueWork: the lane's officer_claim — claim-next and
// by id — is refused, and the queue item stays open and unheld. The control:
// the primary's claim of it succeeds.
func TestAChatLaneCannotTakeQueueWork(t *testing.T) {
	r, judge, _ := laneRig(t)
	r.hq.ClaimNext = true
	queue := r.hq.AddTieredItem(offK, "anomaly.alert", "node-a", officers.TierJudgement)
	for _, args := range []string{`{}`, `{"item_id":"` + queue + `"}`} {
		out := laneMCPCall(t, r, judge, officers.ChatSessionKey("c-41"), officers.ToolClaim, args)
		if !isToolErr(out) || !strings.Contains(out, "chat session") {
			t.Fatalf("a chat lane's officer_claim %s: %s; want a refusal naming the chat session", args, out)
		}
		if it := r.hq.Item(queue); it.Status != "open" || it.ClaimedBy != "" {
			t.Fatalf("after the lane's officer_claim %s the queue item is %s by %q; want open", args, it.Status, it.ClaimedBy)
		}
	}
	if out := laneMCPCall(t, r, judge, "", officers.ToolClaim, `{"item_id":"`+queue+`"}`); isToolErr(out) || r.hq.Item(queue).ClaimedBy != judge.Name {
		t.Fatalf("control: the primary's claim: %s (held by %q)", out, r.hq.Item(queue).ClaimedBy)
	}
}
