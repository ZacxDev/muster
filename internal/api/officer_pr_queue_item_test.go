package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
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

// TestTheQueueDrainSkipsOnlyAFinishedItem: item 19 open, released back to
// open, claimed by ANOTHER instance or officer, waiting or escalated is not
// finished (its queued pull request still opens); done, or listed nowhere
// (cancelled), is finished with a reason; HQ unreadable, or a live list at its
// page bound that does not name it, is an error (the drain waits).
func TestTheQueueDrainSkipsOnlyAFinishedItem(t *testing.T) {
	it := func(id int, officer, holder string) officers.Item {
		return officers.Item{ID: officers.ItemID(strconv.Itoa(id)), OfficerID: officer, ClaimedBy: holder}
	}
	others := []officers.Item{it(7, "", ""), it(8, "", "")}
	ctx := context.Background()
	for name, by := range map[string]map[string][]officers.Item{
		"open (released)":                {officers.StatusOpen: append(others, it(19, "", ""))},
		"claimed by another instance":    {officers.StatusClaimed: {it(19, "engineering:example-web", "officer-engineering-example-web-2")}},
		"claimed under another officer":  {officers.StatusClaimed: {it(19, "engineering:other", "officer-engineering-other-1")}},
		"waiting":                        {officers.StatusWaiting: {it(19, "", "")}},
		"escalated":                      {officers.StatusEscalated: {it(19, "", "")}},
		"live, and done too (live wins)": {officers.StatusOpen: {it(19, "", "")}, officers.StatusDone: {it(19, "", "")}},
	} {
		finished, why, err := itemFinished(ctx, itemsBackend{by: by}, 19)
		if err != nil || finished {
			t.Errorf("%s: finished %v (%q), err %v; want not finished", name, finished, why, err)
		}
	}
	for name, c := range map[string]struct {
		by   map[string][]officers.Item
		want string
	}{
		"done":    {map[string][]officers.Item{officers.StatusOpen: others, officers.StatusDone: {it(19, "", "")}}, "item 19 is completed in HQ"},
		"nowhere": {map[string][]officers.Item{officers.StatusOpen: others, officers.StatusDone: {it(3, "", "")}}, "item 19 is no longer in HQ (cancelled or removed)"},
	} {
		finished, why, err := itemFinished(ctx, itemsBackend{by: c.by}, 19)
		if err != nil || !finished || !strings.Contains(why, c.want) {
			t.Errorf("%s: finished %v (%q), err %v; want finished saying %q", name, finished, why, err, c.want)
		}
	}
	if _, _, err := itemFinished(ctx, itemsBackend{err: errors.New("fake HQ: 503")}, 19); err == nil {
		t.Error("HQ unreadable: no error, want one (the drain waits)")
	}
	full := make([]officers.Item, 500)
	for i := range full {
		full[i] = it(1000+i, "", "")
	}
	if _, _, err := itemFinished(ctx, itemsBackend{by: map[string][]officers.Item{officers.StatusOpen: full}}, 19); err == nil {
		t.Error("a full open list without the item: no error, want one (not proven absent)")
	}
}
