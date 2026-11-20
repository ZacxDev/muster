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
// any queue an officer holds (MaxStatusPages × StatusPageLimit items at the
// full page size), so in practice the list is read to its end.
const MaxStatusPages = 400

// StatusesOf is how each of want stands in the officer's backend: for every
// status in statuses, in order, it pages the items list (items, a backend's
// Items) to its END — following each page's cursor, not stopping at a page
// count the way PageAll does for the wake loop — and records the status of
// each wanted item it sees. It stops as soon as every wanted item is found.
//
//   - A page over the answer cap (ErrHQAnswerTooLarge) is asked again, from
//     the same cursor, at half the page size, down to one item; the smaller
//     size is kept for the rest of that status. One item alone over the cap
//     is an error naming it as such.
//   - Any other read failure is returned (nil map): the caller retries later.
//   - An item listed under no status read is absent from the answer.
func StatusesOf(ctx context.Context, items func(context.Context, ItemsQuery) (ItemsPage, error), statuses []string, want map[ItemID]bool) (map[ItemID]string, error) {
	out := map[ItemID]string{}
	if len(want) == 0 {
		return out, nil
	}
	for _, st := range statuses {
		q := ItemsQuery{Status: st, Limit: StatusPageLimit}
		for pages := 0; pages < MaxStatusPages; {
			page, err := items(ctx, q)
			if errors.Is(err, ErrHQAnswerTooLarge) && q.Limit > 1 {
				q.Limit /= 2
				continue
			}
			if err != nil {
				if errors.Is(err, ErrHQAnswerTooLarge) {
					return nil, fmt.Errorf("one %s item alone is over the answer cap (page size 1): %w", st, err)
				}
				return nil, err
			}
			pages++
			for _, it := range page.Items {
				if want[it.ID] {
					out[it.ID] = it.Status
				}
			}
			if len(out) == len(want) {
				return out, nil
			}
			if page.NextCursor == "" || len(page.Items) == 0 {
				break
			}
			q.Cursor = page.NextCursor
		}
	}
	return out, nil
}
