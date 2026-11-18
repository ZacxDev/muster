package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ChatLaneSecret implements chatLaneSecrets for the fake store: the secret
// setLaneSecret recorded for (agent, session key), "" for none.
func (s *officerAgents) ChatLaneSecret(_ context.Context, agentID int64, sessionKey string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.laneSecrets[strconv.FormatInt(agentID, 10)+" "+sessionKey], nil
}

func setLaneSecret(r *officerRig, a agents.Agent, key, secret string) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.store.laneSecrets == nil {
		r.store.laneSecrets = map[string]string{}
	}
	r.store.laneSecrets[strconv.FormatInt(a.ID, 10)+" "+key] = secret
}

// laneSecretOf is the secret recorded for (a, key), "" for none.
func laneSecretOf(r *officerRig, a agents.Agent, key string) string {
	if key == "" {
		return ""
	}
	s, _ := r.store.ChatLaneSecret(context.Background(), a.ID, key)
	return s
}

// TestAForgedChatSessionHeaderIsRefused (Y4a): a call naming a chat session
// is the lane's only with that lane's secret. Without it, or with another,
// the call is refused outright — never acted on as the lane, and never as
// the primary either (a forged identity is not the primary's word).
func TestAForgedChatSessionHeaderIsRefused(t *testing.T) {
	r, judge, chat := laneRig(t)
	key := officers.ChatSessionKey("c-41")
	for _, secret := range []string{"", "not-the-lane-secret"} {
		out := laneMCPCallWith(t, r, judge, key, secret, obsRawTool, rawArgs)
		if !isToolErr(out) || !strings.Contains(out, "lane secret") {
			t.Fatalf("a call naming %s with secret %q: %s; want a refusal naming the lane secret", key, secret, out)
		}
	}
	if n := len(r.hq.ObsRawCalls); n != 0 {
		t.Fatalf("a forged lane call reached HQ (%d obs/raw calls)", n)
	}
	// Another chat's key with this lane's secret is forged too.
	if out := laneMCPCallWith(t, r, judge, officers.ChatSessionKey("c-99"), laneSecretOf(r, judge, key), obsRawTool, rawArgs); !isToolErr(out) {
		t.Fatalf("another chat's key with this lane's secret: %s; want refused", out)
	}
	// The control: the lane's own secret is its call.
	if out := laneMCPCall(t, r, judge, key, obsRawTool, rawArgs); isToolErr(out) {
		t.Fatalf("the lane's own call: %s", out)
	}
	if got := rawCalls(r); len(got) != 1 || !strings.HasPrefix(got[0], chat+"/") {
		t.Fatalf("HQ's obs/raw calls %v; want one, for the chat item", got)
	}
}

// TestAChatLaneCannotShip (Y1b): a chat session's calls take no ship action
// that is bound to an item or writes to GitHub — merge, tag, mark ready,
// shepherd, public branch — and nothing is merged. Reads stay its own.
func TestAChatLaneCannotShip(t *testing.T) {
	r := newShipRig(t)
	ctx := withChatLaneCall(context.Background(), officers.ChatSessionKey("c-41"))
	for _, tool := range []string{officers.ToolMergePR, officers.ToolTagRelease, officers.ToolMarkReady, officers.ToolShepherdReady,
		officers.ToolShepherdMergeAsk, officers.ToolPublicBranch} {
		out, _ := r.s.dispatchOfficerTool(ctx, r.inst, tool, `{"pr":1,"repo":"example-org/example-web","item_id":"7"}`)
		if !isToolErr(out) || !strings.Contains(out, "chat session") {
			t.Errorf("a chat lane's %s: %s; want a refusal naming the chat session", tool, out)
		}
	}
	if n := len(r.gh.mergedPRs()); n != 0 {
		t.Fatalf("a chat lane merged %d pull request(s)", n)
	}
}

// TestAChatLanesConnectorBlockIsItsChatsNotThePrimarys (Y1a): the block
// record — what an approved grant re-runs — names the item the REFUSED
// session holds.
//
//   - a chat lane's refusal (its own call) records its chat item, never the
//     primary's queue item;
//   - a refusal on a path that carries no session (the connector proxy) while
//     a chat claim is live is ambiguous, and records nothing;
//   - the lane's refusal never debounces the primary's: once the chat claim
//     has ended, the primary's refusal on the same grant records its item.
func TestAChatLanesConnectorBlockIsItsChatsNotThePrimarys(t *testing.T) {
	r := newRerunRig(t)
	queue := r.held("example-org/example-infra#3615")
	chat := r.hq.AddItem(rrOff, officers.ItemKindChat, "chat:c-41")
	r.hq.SetPayload(chat, `{"chat_id":"c-41","seq":1}`)
	r.hq.SetClaim(chat, officers.ChatHolder(r.judge.Name), r.clock().Add(10*time.Minute))
	recorded := func(grant int64) map[officers.ItemID]bool {
		t.Helper()
		bs, err := r.s.officerRT.connectorBlocks().BlocksForGrant(context.Background(), rrOff, grant, time.Time{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[officers.ItemID]bool{}
		for _, b := range bs {
			out[b.Item] = true
		}
		return out
	}
	// The proxy path: no session, a chat claim live — ambiguous.
	r.refuse(officers.GitHubConnector)
	if got := recorded(rrGitHub); len(got) != 0 {
		t.Fatalf("an unattributed refusal while a chat claim is live recorded %v; want nothing (ambiguous)", got)
	}
	// The lane's own refusal: its chat item only.
	r.s.noteConnectorBlocked(withChatLaneCall(context.Background(), officers.ChatSessionKey("c-41")), r.judge, rrDriver, rrDriverC)
	r.s.WaitForConnectorBlocks()
	if got := recorded(rrDriver); len(got) != 1 || !got[officers.ItemID(chat)] {
		t.Fatalf("the lane's refusal recorded %v; want exactly its chat item %s (never queue item %s)", got, chat, queue)
	}
	// The chat claim ends; the primary is refused on the same grant inside
	// the debounce window: its item is recorded.
	r.hq.SetClaim(chat, officers.ChatHolder(r.judge.Name), r.clock().Add(-time.Minute))
	r.s.noteConnectorBlocked(context.Background(), r.judge, rrDriver, rrDriverC)
	r.s.WaitForConnectorBlocks()
	if got := recorded(rrDriver); !got[officers.ItemID(queue)] {
		t.Fatalf("the primary's refusal after the lane's recorded %v; want its queue item %s too (a lane's refusal must not debounce it)", got, queue)
	}
}
