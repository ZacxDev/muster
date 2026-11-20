package api

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// itemsBackend answers AllItems from a status -> items map (or fails); every
// other verb is the nil embedded Backend's (never called here).
type itemsBackend struct {
	officers.Backend
	by  map[string][]officers.Item
	err error
}

func (b itemsBackend) AllItems(_ context.Context, status string) ([]officers.Item, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.by[status], nil
}

// TestTheQueueDrainSkipsOnlyAGoneItem: item 19 open, released back to open,
// claimed by ANOTHER instance or officer, waiting, escalated — or COMPLETED
// (listed done, which holds no_action too) — is not gone: its queued pull
// request still opens (the officer is told it may complete its item once its
// create is queued). Listed nowhere (cancelled or removed) is gone, with a
// reason. A list at its page bound that does not name it is not proof it is
// gone (the create opens rather than block the queue); HQ unreadable is an
// error (the drain waits).
func TestTheQueueDrainSkipsOnlyAGoneItem(t *testing.T) {
	it := func(id int, officer, holder string) officers.Item {
		return officers.Item{ID: officers.ItemID(strconv.Itoa(id)), OfficerID: officer, ClaimedBy: holder}
	}
	others := []officers.Item{it(7, "", ""), it(8, "", "")}
	full := make([]officers.Item, 500)
	for i := range full {
		full[i] = it(1000+i, "", "")
	}
	ctx := context.Background()
	for name, by := range map[string]map[string][]officers.Item{
		"open (released)":               {officers.StatusOpen: append(others, it(19, "", ""))},
		"claimed by another instance":   {officers.StatusClaimed: {it(19, "engineering:example-web", "officer-engineering-example-web-2")}},
		"claimed under another officer": {officers.StatusClaimed: {it(19, "engineering:other", "officer-engineering-other-1")}},
		"waiting":                       {officers.StatusWaiting: {it(19, "", "")}},
		"escalated":                     {officers.StatusEscalated: {it(19, "", "")}},
		// 🔴 THE DEFECT: completing the item after the 202 dropped the row.
		"completed (done)":               {officers.StatusOpen: others, officers.StatusDone: {it(19, "", "")}},
		"live, and done too":             {officers.StatusOpen: {it(19, "", "")}, officers.StatusDone: {it(19, "", "")}},
		"nowhere, the open list is full": {officers.StatusOpen: full},
		"nowhere, the done list is full": {officers.StatusOpen: others, officers.StatusDone: full},
	} {
		gone, why, err := itemGone(ctx, itemsBackend{by: by}, 19)
		if err != nil || gone {
			t.Errorf("%s: gone %v (%q), err %v; want not gone", name, gone, why, err)
		}
	}
	for name, by := range map[string]map[string][]officers.Item{
		"nowhere":           {officers.StatusOpen: others, officers.StatusDone: {it(3, "", "")}},
		"nowhere, 499 done": {officers.StatusOpen: others, officers.StatusDone: full[:499]},
		"no list names any": {},
	} {
		gone, why, err := itemGone(ctx, itemsBackend{by: by}, 19)
		if want := "item 19 is no longer in HQ (cancelled or removed), so its pull request is moot"; err != nil || !gone || why != want {
			t.Errorf("%s: gone %v (%q), err %v; want gone saying %q", name, gone, why, err, want)
		}
	}
	if _, _, err := itemGone(ctx, itemsBackend{err: errors.New("fake HQ: 503")}, 19); err == nil {
		t.Error("HQ unreadable: no error, want one (the drain waits)")
	}
}
