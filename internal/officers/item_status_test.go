package officers_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// pagedList is a backend's Items over n items per status, with each item's
// answer size: a page whose items' sizes sum past capBytes is refused with
// ErrHQAnswerTooLarge, as the HQ client refuses a body over its cap. The
// cursor is the next offset. calls records each request's (status, limit,
// cursor).
type pagedList struct {
	byStatus map[string][]officers.Item
	size     func(officers.Item) int
	capBytes int
	fail     error
	calls    []string
}

func (p *pagedList) items(_ context.Context, q officers.ItemsQuery) (officers.ItemsPage, error) {
	p.calls = append(p.calls, fmt.Sprintf("%s/%d/%s", q.Status, q.Limit, q.Cursor))
	if p.fail != nil {
		return officers.ItemsPage{}, p.fail
	}
	all := p.byStatus[q.Status]
	start, _ := strconv.Atoi(q.Cursor)
	end := start + q.Limit
	if end > len(all) {
		end = len(all)
	}
	page := all[start:end]
	total := 0
	for _, it := range page {
		total += p.size(it)
	}
	if total > p.capBytes {
		return officers.ItemsPage{}, fmt.Errorf("%w: GET items", officers.ErrHQAnswerTooLarge)
	}
	out := officers.ItemsPage{Items: page}
	if end < len(all) {
		out.NextCursor = strconv.Itoa(end)
	}
	return out, nil
}

func listOf(status string, n, from int) []officers.Item {
	out := make([]officers.Item, n)
	for i := range out {
		out[i] = officers.Item{ID: officers.ItemID(strconv.Itoa(from + i)), Status: status}
	}
	return out
}

// TestStatusesOfFollowsPagingToTheEnd: a wanted item on the LAST page of a
// list far longer than PageAll's five pages of 100 is found, by following
// each cursor; every page asks for StatusPageLimit. CONTROL: PageAll — the
// read the re-run used to make — does not reach it.
func TestStatusesOfFollowsPagingToTheEnd(t *testing.T) {
	const n = 1234 // past 5 pages of 100, and not a multiple of the page size
	last := officers.ItemID(strconv.Itoa(9000 + n - 1))
	p := &pagedList{byStatus: map[string][]officers.Item{officers.StatusEscalated: listOf(officers.StatusEscalated, n, 9000)},
		size: func(officers.Item) int { return 1 }, capBytes: 1 << 20}
	got, err := officers.StatusesOf(context.Background(), p.items, []string{officers.StatusEscalated}, map[officers.ItemID]bool{last: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Of[last] != officers.StatusEscalated {
		t.Fatalf("the last item of %d was not found: %v", n, got)
	}
	wantPages := (n + officers.StatusPageLimit - 1) / officers.StatusPageLimit
	if len(p.calls) != wantPages {
		t.Fatalf("%d requests, want %d pages of %d", len(p.calls), wantPages, officers.StatusPageLimit)
	}
	if p.calls[0] != officers.StatusEscalated+"/"+strconv.Itoa(officers.StatusPageLimit)+"/" {
		t.Fatalf("first request %q", p.calls[0])
	}
	all, err := officers.PageAll(context.Background(), p.items, officers.StatusEscalated)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range all {
		if it.ID == last {
			t.Fatal("control: PageAll reached the last item, so this list does not show the bound")
		}
	}
}

// TestStatusesOfHalvesAPageOverTheCap: items big enough that a page of
// StatusPageLimit is over the answer cap are read at half the size, down to
// one item a page; every wanted item is found. CONTROL: a 100-item page (the
// re-run's old read) is refused.
func TestStatusesOfHalvesAPageOverTheCap(t *testing.T) {
	const capBytes = 1000
	items := listOf(officers.StatusEscalated, 40, 500)
	size := func(it officers.Item) int {
		if it.ID == "530" {
			return 900 // one item nearly the cap alone: forces a page of 1
		}
		return 70 // 25 × 70 = 1750 > cap; 12 × 70 = 840 fits
	}
	p := &pagedList{byStatus: map[string][]officers.Item{officers.StatusEscalated: items}, size: size, capBytes: capBytes}
	want := map[officers.ItemID]bool{"501": true, "530": true, "539": true}
	got, err := officers.StatusesOf(context.Background(), p.items, []string{officers.StatusEscalated}, want)
	if err != nil {
		t.Fatal(err)
	}
	for id := range want {
		if got.Of[id] != officers.StatusEscalated {
			t.Fatalf("item %s not found over the cap: %v (requests %v)", id, got, p.calls)
		}
	}
	if _, err := p.items(context.Background(), officers.ItemsQuery{Status: officers.StatusEscalated, Limit: 100}); !errors.Is(err, officers.ErrHQAnswerTooLarge) {
		t.Fatalf("control: a 100-item page was not over the cap: %v", err)
	}
}

// TestStatusesOfStopsOnceEveryItemIsFound and reads the statuses in order.
func TestStatusesOfStopsOnceEveryItemIsFound(t *testing.T) {
	p := &pagedList{byStatus: map[string][]officers.Item{
		officers.StatusEscalated: listOf(officers.StatusEscalated, 60, 100),
		officers.StatusDone:      listOf(officers.StatusDone, 60, 200),
	}, size: func(officers.Item) int { return 1 }, capBytes: 1 << 20}
	got, err := officers.StatusesOf(context.Background(), p.items, []string{officers.StatusEscalated, officers.StatusDone},
		map[officers.ItemID]bool{"101": true, "201": true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Of["101"] != officers.StatusEscalated || got.Of["201"] != officers.StatusDone {
		t.Fatalf("statuses %v", got)
	}
	// escalated: three pages of 25 (60 items); done: the first page holds 201.
	if len(p.calls) != 4 {
		t.Fatalf("requests %v, want 3 escalated pages then 1 done page", p.calls)
	}
}

// TestStatusesOfReturnsAReadFailure: any other failure is returned, for the
// caller to retry — never an empty "nothing found".
func TestStatusesOfReturnsAReadFailure(t *testing.T) {
	boom := errors.New("HQ unavailable")
	p := &pagedList{fail: fmt.Errorf("%w: GET items", boom)}
	got, err := officers.StatusesOf(context.Background(), p.items, []string{officers.StatusEscalated}, map[officers.ItemID]bool{"1": true})
	if !errors.Is(err, boom) || got.Of != nil {
		t.Fatalf("got %v, %v; want the failure", got, err)
	}
	// One item alone over the cap is an error too.
	big := &pagedList{byStatus: map[string][]officers.Item{officers.StatusEscalated: listOf(officers.StatusEscalated, 3, 1)},
		size: func(officers.Item) int { return 10 }, capBytes: 5}
	if _, err := officers.StatusesOf(context.Background(), big.items, []string{officers.StatusEscalated}, map[officers.ItemID]bool{"2": true}); !errors.Is(err, officers.ErrHQAnswerTooLarge) {
		t.Fatalf("one item over the cap: %v, want ErrHQAnswerTooLarge", err)
	}
}

// TestStatusesOfReportsThePageBound: a list longer than MaxStatusPages pages
// stops at the bound, and says so (Bounded names the status) rather than
// reading as "not listed"; the next status is still read. CONTROL: a list of
// exactly MaxStatusPages full pages ends on its last page and is NOT bounded.
func TestStatusesOfReportsThePageBound(t *testing.T) {
	full := officers.MaxStatusPages * officers.StatusPageLimit
	beyond := officers.ItemID(strconv.Itoa(full + 7))
	p := &pagedList{byStatus: map[string][]officers.Item{officers.StatusDone: listOf(officers.StatusDone, full+10, 1)},
		size: func(officers.Item) int { return 1 }, capBytes: 1 << 20}
	got, err := officers.StatusesOf(context.Background(), p.items, []string{officers.StatusDone, officers.StatusWaiting}, map[officers.ItemID]bool{beyond: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Of[beyond]; ok || len(got.Bounded) != 1 || got.Bounded[0] != officers.StatusDone {
		t.Fatalf("got %+v; want %s absent and Bounded [done]", got, beyond)
	}
	if len(p.calls) != 400+1 || p.calls[400] != "waiting/25/" {
		t.Fatalf("%d requests (last %q); want 400 done pages then 1 waiting page", len(p.calls), p.calls[len(p.calls)-1])
	}

	exact := &pagedList{byStatus: map[string][]officers.Item{officers.StatusDone: listOf(officers.StatusDone, full, 1)},
		size: func(officers.Item) int { return 1 }, capBytes: 1 << 20}
	got, err = officers.StatusesOf(context.Background(), exact.items, []string{officers.StatusDone}, map[officers.ItemID]bool{"absent": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Bounded) != 0 || len(exact.calls) != 400 {
		t.Fatalf("control: a list of exactly %d items read %d pages, Bounded %v; want 400 pages, not bounded", full, len(exact.calls), got.Bounded)
	}
}

// TestStatusesOfGrowsThePageBackAfterAHalving: one item (id 10) fits a page
// only alone; the pages after it are asked at double the size each time,
// back up to StatusPageLimit and never past it, so the list's reach is not
// cut to one item a page for the rest of the status.
func TestStatusesOfGrowsThePageBackAfterAHalving(t *testing.T) {
	size := func(it officers.Item) int {
		if it.ID == "10" {
			return 995
		}
		return 10 // 25 × 10 fits the cap
	}
	p := &pagedList{byStatus: map[string][]officers.Item{officers.StatusEscalated: listOf(officers.StatusEscalated, 100, 0)}, size: size, capBytes: 1000}
	got, err := officers.StatusesOf(context.Background(), p.items, []string{officers.StatusEscalated}, map[officers.ItemID]bool{"99": true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Of["99"] != officers.StatusEscalated {
		t.Fatalf("item 99 not found: %+v", got)
	}
	want := []string{
		"escalated/25/", "escalated/12/", "escalated/6/", // 0..5
		"escalated/12/6", "escalated/6/6", "escalated/3/6", // 6..8
		"escalated/6/9", "escalated/3/9", "escalated/1/9", // 9
		"escalated/2/10", "escalated/1/10", // 10 alone
		"escalated/2/11", "escalated/4/13", "escalated/8/17", "escalated/16/25",
		"escalated/25/41", "escalated/25/66", "escalated/25/91",
	}
	if fmt.Sprint(p.calls) != fmt.Sprint(want) {
		t.Fatalf("requests\n %v\nwant\n %v", p.calls, want)
	}
}
