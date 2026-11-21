package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// officer_complete's `ask` ({kind, ref}: the single thing an escalation asks a
// person to do) reaches HQ's complete AS SENT. HQ's complete accepts the key;
// before this, muster's tool schema did not offer it (and closes its
// properties), and its dispatch had no field for it, so an officer could not
// send one and HQ never saw one.
// ---------------------------------------------------------------------------

// askArg is the ask the forwarding test sends. Its ref is a string HQ would
// take; muster checks neither field.
const askArg = `{"kind":"connector_grant","ref":"35"}`

// TestOfficerCompleteOffersAskWithKindAndRef: every HQ officer pool's
// officer_complete schema has an `ask` object property whose properties are
// exactly kind and ref, both strings, and kind is NOT an enum — HQ owns the
// vocabulary, so a kind HQ adds must need no muster change.
func TestOfficerCompleteOffersAskWithKindAndRef(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	for _, a := range []agents.Agent{
		r.addNamed(offKJudge+"1", 2301),
		r.addNamed(officers.InstanceName(offA, 1), 2302),
		r.addNamed(offKRout+"1", 2303),
	} {
		raw, ok := completePropsOf(t, r.s.MCPToolDefsFor(ctx, a))["ask"]
		if !ok {
			t.Errorf("%s: officer_complete offers no ask", a.Name)
			continue
		}
		var ask struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &ask); err != nil {
			t.Fatalf("%s: ask %s: %v", a.Name, raw, err)
		}
		if ask.Type != "object" {
			t.Errorf("%s: ask is type %q, want object", a.Name, ask.Type)
		}
		if len(ask.Properties) != 2 {
			t.Errorf("%s: ask has %d properties %s, want exactly kind and ref", a.Name, len(ask.Properties), raw)
		}
		for _, k := range []string{"kind", "ref"} {
			var p map[string]any
			if err := json.Unmarshal(ask.Properties[k], &p); err != nil || p["type"] != "string" {
				t.Errorf("%s: ask.%s is %s, want a string property", a.Name, k, ask.Properties[k])
			}
			if _, enum := p["enum"]; enum {
				t.Errorf("%s: ask.%s is an enum (%s): HQ owns the kinds", a.Name, k, ask.Properties[k])
			}
		}
	}
	// A builtin officer is not offered it: its backend has no asks.
	b := completePropsOf(t, []agents.ToolDef{completeToolDef(officers.Officer{ID: "marketing:example", Backend: officers.BackendBuiltin})})
	if _, has := b["ask"]; has {
		t.Errorf("a builtin officer's officer_complete offers ask")
	}
}

// TestOfficerCompleteForwardsAskToHQ: an escalation carrying
// ask {kind: connector_grant, ref: "35"} reaches HQ's complete body with
// exactly that object, byte for byte; the same complete without ask sends no
// ask key at all (an HQ that predates a field refuses it, even as null).
func TestOfficerCompleteForwardsAskToHQ(t *testing.T) {
	r := newKubeOfficerRig(t)
	ctx := context.Background()
	judge := r.addNamed(offKJudge+"1", 2311)

	item := r.hq.AddTieredItem(offK, "followup.check", "ask-sent", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	args := `{"item_id":"` + item + `","outcome":"escalated","summary":"a connector grant is waiting","ask":` + askArg + `}`
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, args); isToolErr(out) {
		t.Fatalf("a complete with ask: %s", out)
	}
	body := rawCompleteBody(t, r)
	got, sent := body["ask"]
	if !sent {
		t.Fatalf("ask not forwarded: HQ's complete body has no ask key (%s)", completeRequests(r.hq)[0].Raw)
	}
	if string(got) != askArg {
		t.Errorf("HQ received ask %s, want %s", got, askArg)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil || !reflect.DeepEqual(decoded, map[string]any{"kind": "connector_grant", "ref": "35"}) {
		t.Errorf("HQ received ask %s (%v), want exactly {kind: connector_grant, ref: \"35\"}", got, err)
	}
	if it := r.hq.Item(item); it.Status != "escalated" {
		t.Errorf("the item is %s, want escalated", it.Status)
	}

	item = r.hq.AddTieredItem(offK, "followup.check", "ask-absent", officers.TierJudgement)
	r.hq.SetClaim(item, judge.Name, time.Now().Add(10*time.Minute))
	r.hq.ResetRequests()
	args = `{"item_id":"` + item + `","outcome":"escalated","summary":"a connector grant is waiting"}`
	if out := r.s.dispatchAgentTool(ctx, judge, completeTool, args); isToolErr(out) {
		t.Fatalf("a complete without ask: %s", out)
	}
	body = rawCompleteBody(t, r)
	if v, sent := body["ask"]; sent {
		t.Errorf("HQ received ask %s on a complete that sent none", v)
	}
	if _, ok := body["summary"]; !ok {
		t.Fatalf("the body read is wrong: no summary in %v", body)
	}
}
