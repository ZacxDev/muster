package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE READ TOOLS' SHARED STEPS (officers/reads.go "THE PER-ITEM READ
// BUDGET"): the item a read is for, and charging it.
//
// officer_kube_read, officer_obs_raw and officer_obs_query (a judgement
// instance's) each resolve the item through heldItem and charge it through
// spendRead — the server's ONE officers.ReadBudget (officerRuntime.reads) —
// so the three tools spend one count per item, never one each.
// ---------------------------------------------------------------------------

// splitItemID removes item_id — the dispatcher's, never the backend's — from
// a tool's arguments object, returning the rest and the id it named.
func splitItemID(raw []byte) (rest []byte, item officers.ItemID, present bool, err error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &obj); err != nil || obj == nil {
		return nil, "", false, errors.New("arguments must be a JSON object")
	}
	v, ok := obj["item_id"]
	if !ok {
		return raw, "", false, nil
	}
	if err := json.Unmarshal(v, &item); err != nil {
		return nil, "", false, errors.New("item_id must be an item id")
	}
	delete(obj, "item_id")
	rest, _ = json.Marshal(obj)
	return rest, item, true, nil
}

// heldItem is the item instance holds a LIVE claim on in HQ: the one it
// names (which must be among them), or its only one. With required false, an
// instance holding none gets "" and no error (a read that is no item's).
func (s *Server) heldItem(ctx context.Context, c officers.Backend, instance string, named officers.ItemID, required bool) (officers.ItemID, error) {
	claimed, err := c.AllItems(ctx, officers.StatusClaimed)
	if err != nil {
		return "", err
	}
	now := s.officerRT.clock()
	var held []officers.ItemID
	for _, it := range claimed {
		if it.ClaimedBy == instance && it.LeaseLive(now) {
			held = append(held, it.ID)
		}
	}
	switch {
	case named != "":
		for _, id := range held {
			if id == named {
				return id, nil
			}
		}
		return "", fmt.Errorf("you hold no live claim on item %s: a read is counted against an item you hold", named)
	case len(held) == 1:
		return held[0], nil
	case len(held) == 0 && !required:
		return "", nil
	case len(held) == 0:
		return "", errors.New("you hold no claimed item: claim one first (officer_claim) — every read is counted against the item it is for")
	}
	return "", fmt.Errorf("you hold %d items: name the one this read is for with item_id", len(held))
}

// heldItemRefusal is tool's answer to a heldItem error: HQ's own refusal or
// outage as officerToolResult words it, else the reason.
func heldItemRefusal(tool string, err error) string {
	var hq *officers.HQError
	if errors.As(err, &hq) || errors.Is(err, officers.ErrHQUnavailable) {
		return officerToolResult(tool, nil, err)
	}
	return toolErr("%v", err)
}

// spendRead charges one read of item to officer o's budget: THE one call
// every counted read tool makes (officers.CountsRead).
func (s *Server) spendRead(o officers.Officer, item officers.ItemID) error {
	_, err := s.officerRT.reads.Spend(o.ID, item)
	return err
}

// settleRead is the second half of an HQ read tool's charge: spendRead
// RESERVED the unit before the call (so concurrent calls can never pass the
// cap), and settleRead gives it back when HQ's answer err does not count
// (officers.ReadCounted: a 429, a 5xx, a 403 not_investigating, or no
// answer at all). officer_obs_raw
// and a judgement instance's officer_obs_query call it after every HQ call
// they charged; officer_kube_read reads no HQ route and does not.
func (s *Server) settleRead(o officers.Officer, item officers.ItemID, err error) {
	if !officers.ReadCounted(err) {
		s.officerRT.reads.Refund(o.ID, item)
	}
}
