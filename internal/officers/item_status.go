package officers

import (
	"context"
	"errors"
	"fmt"
)

// StatusPageLimit is the page size StatusesOf asks for. An item's payload
// is HQ's, of any size: a page of 100 escalated items measured over muster's
// 1 MiB answer cap (ErrHQAnswerTooLarge), so a grant's re-run read none of
// them. A smaller page fits typical items, and a page still over the cap is
// asked again at half the size (down to one item).
const StatusPageLimit = 25

// MaxStatusPages bounds the pages StatusesOf reads per status: well past
// any queue an officer holds, so in practice the list is read to its end. A
// read that stops at it is reported (StatusRead.Bounded), never taken for
// the end of the list.
const MaxStatusPages = 400

// StatusRead is StatusesOf's answer.
type StatusRead struct {
	// Of is the status of each wanted item seen. An item absent from it was
	// listed under no status read — unless Bounded is non-empty.
	Of map[ItemID]string
	// Bounded names each status whose read stopped at MaxStatusPages with
	// pages still left: an item absent from Of may be beyond the bound, so
	// its absence does not say HQ lacks it.
	Bounded []string
}

// StatusesOf is how each of want stands in the officer's backend: for every
// status in statuses, in order, it pages the items list (items, a backend's
// Items) to its END — following each page's cursor, not stopping at a page
// count the way PageAll does for the wake loop — and records the status of
// each wanted item it sees. It stops as soon as every wanted item is found,
// so a caller lists its short statuses first and a long one (done) last.
//
//   - A page over the answer cap (ErrHQAnswerTooLarge) is asked again, from
//     the same cursor, at half the page size, down to one item; once a
//     smaller page is read, the next page is asked at double its size, back
//     up to StatusPageLimit. One item alone over the cap is an error naming
//     it as such.
//   - A status with pages still left after MaxStatusPages is named in
//     Bounded, and the next status is read.
//   - Any other read failure is returned (nil Of): the caller retries later.
func StatusesOf(ctx context.Context, items func(context.Context, ItemsQuery) (ItemsPage, error), statuses []string, want map[ItemID]bool) (StatusRead, error) {
	out := StatusRead{Of: map[ItemID]string{}}
	if len(want) == 0 {
		return out, nil
	}
	for _, st := range statuses {
		q := ItemsQuery{Status: st, Limit: StatusPageLimit}
		for pages := 0; ; {
			if pages == MaxStatusPages {
				// Reached only after a page that named a next cursor.
				out.Bounded = append(out.Bounded, st)
				break
			}
			page, err := items(ctx, q)
			if errors.Is(err, ErrHQAnswerTooLarge) && q.Limit > 1 {
				q.Limit /= 2
				continue
			}
			if err != nil {
				if errors.Is(err, ErrHQAnswerTooLarge) {
					return StatusRead{}, fmt.Errorf("one %s item alone is over the answer cap (page size 1): %w", st, err)
				}
				return StatusRead{}, err
			}
			pages++
			for _, it := range page.Items {
				if want[it.ID] {
					out.Of[it.ID] = it.Status
				}
			}
			if len(out.Of) == len(want) {
				return out, nil
			}
			if page.NextCursor == "" || len(page.Items) == 0 {
				break
			}
			q.Cursor = page.NextCursor
			q.Limit = min(2*q.Limit, StatusPageLimit)
		}
	}
	return out, nil
}
