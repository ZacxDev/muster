package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// payload.claims through officer_items, officer_claim and officer_brief:
// byte for byte, whatever its size (HQ bounds it; muster applies no cap).
// An INVARIANT GUARD: muster has always passed HQ's body through.

// claimsFixture is a claims string as HQ copies it: fenced blocks, a 🔴, CRLF
// line ends, and characters a JSON encoder escapes.
const claimsFixture = "```audit-claims\r\n🔴 premise: the limit is unbounded <at> deploy.yaml & \"quoted\"\r\n🟡 naming\r\n```\r\n```audit-round-0\r\nround 0 · requirements: 1\r\n```\r\n"

func (r *shepAPIRig) tool(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(args)
	return r.s.dispatchAgentTool(context.Background(), r.inst, name, string(b))
}

// claimsOf finds payload.claims (and claims_withheld_bytes) of item id in a
// tool answer: a list under items / open_items, or one item.
func payloadClaimsOf(t *testing.T, out, id string) (string, any) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	var find func(any) (string, any, bool)
	find = func(x any) (string, any, bool) {
		switch m := x.(type) {
		case map[string]any:
			if p, ok := m["payload"].(map[string]any); ok && (m["id"] == id || m["id"] == json.Number(id)) {
				c, _ := p["claims"].(string)
				return c, p["claims_withheld_bytes"], true
			}
			for _, e := range m {
				if c, w, ok := find(e); ok {
					return c, w, ok
				}
			}
		case []any:
			for _, e := range m {
				if c, w, ok := find(e); ok {
					return c, w, ok
				}
			}
		}
		return "", nil, false
	}
	c, w, ok := find(v)
	if !ok {
		t.Fatalf("item %s with a payload is not in %s", id, out)
	}
	return c, w
}

func TestPayloadClaimsReachTheInstanceByteForByte(t *testing.T) {
	r := newShepAPIRig(t)
	id := r.hq.AddItem(offShep, "audit.round", officers.ShepherdSubject(shepRepoAPI, 5))
	raw, _ := json.Marshal(map[string]any{"pr_loop": map[string]any{"repo": shepRepoAPI, "pr": 5}, "claims": claimsFixture})
	r.hq.SetPayload(id, string(raw))
	// officer_items: exactly HQ's bytes.
	out := r.tool(t, officers.ToolItems, map[string]any{})
	page, err := r.s.officerRT.hq.Backend(r.s.officerRT.reg.All()[1]).Items(context.Background(), officers.ItemsQuery{Status: officers.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	if out != string(page.Raw) {
		t.Fatalf("officer_items is not HQ's body byte for byte:\n%s\nvs\n%s", out, page.Raw)
	}
	if c, _ := payloadClaimsOf(t, out, id); c != claimsFixture {
		t.Fatalf("officer_items claims %q, want %q", c, claimsFixture)
	}
	// officer_brief and officer_claim: the claims string exactly.
	if c, _ := payloadClaimsOf(t, r.tool(t, officers.ToolBrief, map[string]any{}), id); c != claimsFixture {
		t.Fatalf("officer_brief claims %q", c)
	}
	if c, _ := payloadClaimsOf(t, r.tool(t, officers.ToolClaim, map[string]any{"item_id": id}), id); c != claimsFixture {
		t.Fatalf("officer_claim claims %q", c)
	}
}
