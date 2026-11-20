package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// itemsBackend answers Items from a status -> items map, paged as a backend
// pages: the request's limit (at most 100), the cursor the next offset, the
// list in the map's order (the built-in backend's is oldest first). AllItems
// is every backend's (officers.PageAll). err fails every read; failFrom > 0
// fails a page that starts at or past that offset. reads counts Items calls.
// Every other verb is the nil embedded Backend's (never called here).
type itemsBackend struct {
	officers.Backend
	by       map[string][]officers.Item
	err      error
	failFrom int
	reads    int
}

func (b *itemsBackend) Items(_ context.Context, q officers.ItemsQuery) (officers.ItemsPage, error) {
	b.reads++
	if b.err != nil {
		return officers.ItemsPage{}, b.err
	}
	all := b.by[q.Status]
	start, _ := strconv.Atoi(q.Cursor)
	if b.failFrom > 0 && start >= b.failFrom {
		return officers.ItemsPage{}, errors.New("fake HQ: 503 on page from " + strconv.Itoa(start))
	}
	limit := q.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	end := min(start+limit, len(all))
	page := officers.ItemsPage{Items: all[start:end]}
	if end < len(all) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

func (b *itemsBackend) AllItems(ctx context.Context, status string) ([]officers.Item, error) {
	return officers.PageAll(ctx, b.Items, status)
}

func qItem(id int, officer, holder string) officers.Item {
	return officers.Item{ID: officers.ItemID(strconv.Itoa(id)), OfficerID: officer, ClaimedBy: holder}
}

// qItems is n items numbered from 1000 up (never 19), oldest first.
func qItems(n int) []officers.Item {
	out := make([]officers.Item, n)
	for i := range out {
		out[i] = qItem(1000+i, "", "")
	}
	return out
}

const goneWhy = "item 19 is no longer in HQ (cancelled or removed), so its pull request is moot"

// TestTheQueueDrainSkipsOnlyAGoneItem: item 19 open, released back to open,
// claimed by ANOTHER instance or officer, waiting, escalated — or COMPLETED
// (listed done, which holds no_action too) — is not gone: its queued pull
// request still opens (the officer is told it may complete its item once its
// create is queued). Listed nowhere (cancelled or removed), every list read to
// its END, is gone, with a reason — whatever the lists' lengths: a list of
// exactly 500 (the old read's bound) or more no longer counts as "not known".
// HQ unreadable is an error (the drain waits).
func TestTheQueueDrainSkipsOnlyAGoneItem(t *testing.T) {
	others := []officers.Item{qItem(7, "", ""), qItem(8, "", "")}
	full := qItems(500)
	ctx := context.Background()
	for name, by := range map[string]map[string][]officers.Item{
		"open (released)":               {officers.StatusOpen: append(others, qItem(19, "", ""))},
		"claimed by another instance":   {officers.StatusClaimed: {qItem(19, "engineering:example-web", "officer-engineering-example-web-2")}},
		"claimed under another officer": {officers.StatusClaimed: {qItem(19, "engineering:other", "officer-engineering-other-1")}},
		"waiting":                       {officers.StatusWaiting: {qItem(19, "", "")}},
		"escalated":                     {officers.StatusEscalated: {qItem(19, "", "")}},
		// 🔴 completing the item after the 202 must not drop the row.
		"completed (done)":   {officers.StatusOpen: others, officers.StatusDone: {qItem(19, "", "")}},
		"live, and done too": {officers.StatusOpen: {qItem(19, "", "")}, officers.StatusDone: {qItem(19, "", "")}},
	} {
		gone, why, err := itemGone(ctx, &itemsBackend{by: by}, 19)
		if err != nil || gone {
			t.Errorf("%s: gone %v (%q), err %v; want not gone", name, gone, why, err)
		}
	}
	for name, by := range map[string]map[string][]officers.Item{
		"nowhere":                       {officers.StatusOpen: others, officers.StatusDone: {qItem(3, "", "")}},
		"nowhere, 499 done":             {officers.StatusOpen: others, officers.StatusDone: full[:499]},
		"nowhere, the open list is 500": {officers.StatusOpen: full},
		"nowhere, the done list is 500": {officers.StatusOpen: others, officers.StatusDone: full},
		"no list names any":             {},
	} {
		gone, why, err := itemGone(ctx, &itemsBackend{by: by}, 19)
		if err != nil || !gone || why != goneWhy {
			t.Errorf("%s: gone %v (%q), err %v; want gone saying %q", name, gone, why, err, goneWhy)
		}
	}
	if _, _, err := itemGone(ctx, &itemsBackend{err: errors.New("fake HQ: 503")}, 19); err == nil {
		t.Error("HQ unreadable: no error, want one (the drain waits)")
	}
}

// TestACancelledItemPastFiveHundredDoneIsGone: an officer with 1,234 done
// items (past the old read's 500-item bound) whose queued item 19 is listed
// nowhere — cancelled — is GONE (the drain drops the row and tells HQ): every
// list is paged to its end, done included. The old read stopped at 500 done
// and opened the create.
func TestACancelledItemPastFiveHundredDoneIsGone(t *testing.T) {
	b := &itemsBackend{by: map[string][]officers.Item{
		officers.StatusOpen: {qItem(7, "", "")},
		officers.StatusDone: qItems(1234),
	}}
	gone, why, err := itemGone(context.Background(), b, 19)
	if err != nil || !gone || why != goneWhy {
		t.Fatalf("gone %v (%q), err %v; want gone saying %q", gone, why, err, goneWhy)
	}
}

// TestTheNewestDoneItemPastTheFirstPagesIsNotGone: item 19 completed LAST,
// so it is the newest of 1,234 done items and on done's last page in a
// backend that lists oldest first: it is found (not gone, the create opens),
// and the read stops there — done's pages are read, the item found on the
// last one. The same item listed newest first is found on the first page.
// Reads: one page each of open, claimed, waiting and escalated, then done's
// pages of 25 — all 50 oldest first, one newest first.
func TestTheNewestDoneItemPastTheFirstPagesIsNotGone(t *testing.T) {
	oldestFirst := append(qItems(1233), qItem(19, "", ""))
	newestFirst := append([]officers.Item{qItem(19, "", "")}, qItems(1233)...)
	for name, c := range map[string]struct {
		done  []officers.Item
		reads int
	}{"oldest first": {oldestFirst, 54}, "newest first": {newestFirst, 5}} {
		b := &itemsBackend{by: map[string][]officers.Item{officers.StatusOpen: {qItem(7, "", "")}, officers.StatusDone: c.done}}
		gone, why, err := itemGone(context.Background(), b, 19)
		if err != nil || gone {
			t.Errorf("%s: gone %v (%q), err %v; want not gone (the create opens)", name, gone, why, err)
		}
		if b.reads != c.reads {
			t.Errorf("%s: %d pages read, want %d", name, b.reads, c.reads)
		}
	}
}

// TestAReadErrorOnALaterPageWaits: 1,234 done items, item 19 nowhere, and
// the done page from offset 600 fails: not known, so an error (the drain
// waits; never a drop on a list it could not read whole). The old read never
// reached offset 600 and opened the create.
func TestAReadErrorOnALaterPageWaits(t *testing.T) {
	b := &itemsBackend{failFrom: 600, by: map[string][]officers.Item{
		officers.StatusOpen: {qItem(7, "", "")},
		officers.StatusDone: qItems(1234),
	}}
	gone, why, err := itemGone(context.Background(), b, 19)
	if err == nil || gone || !strings.Contains(err.Error(), "fake HQ: 503 on page from 600") {
		t.Fatalf("gone %v (%q), err %v; want an error naming the failed page (the drain waits)", gone, why, err)
	}
}

// TestAListPastThePageBoundWaits: a done list still with pages after
// officers.MaxStatusPages, item 19 named nowhere read: not known, so an
// error that says the bound was reached (the drain waits; never opened,
// never dropped).
func TestAListPastThePageBoundWaits(t *testing.T) {
	b := &itemsBackend{by: map[string][]officers.Item{
		officers.StatusDone: qItems(officers.MaxStatusPages*officers.StatusPageLimit + 7),
	}}
	gone, why, err := itemGone(context.Background(), b, 19)
	want := "item 19 is in none of HQ's lists, but the done list(s) still had pages after muster's bound of " +
		strconv.Itoa(officers.MaxStatusPages) + " pages, so whether it is gone is not known: the queued pull request waits"
	if err == nil || gone || err.Error() != want {
		t.Fatalf("gone %v (%q), err %v; want the error %q", gone, why, err, want)
	}
}
