package notes

import "context"

// LiveIDs reports which of ids name a live (not dismissed) task that ctx's
// principal can read: exactly the tasks Get would return. It is ONE query,
// for a caller that only needs to know which ids exist (a chat reply's #N
// mentions, internal/ui/mentions.go), rather than one Get — with its
// attachments, comments and sessions — per id.
//
// The read runs on the pool with ctx, as Get's does, so row-level security
// scopes it to the same principal: another owner's task is not found.
func (s *PGStore) LiveIDs(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM notes WHERE id = ANY($1) AND `+liveOnly, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// liveIDser is a store that answers LiveIDs in one read.
type liveIDser interface {
	LiveIDs(ctx context.Context, ids []int64) (map[int64]bool, error)
}

// LiveIDs is store's LiveIDs: one read when the store (or the store a
// WithStatusLogging decorator wraps) has it, else one Get per id — the same
// answer, the slow way, for a store with no batched read (a test fake).
func LiveIDs(ctx context.Context, store Store, ids []int64) (map[int64]bool, error) {
	if d, ok := store.(*statusLoggingStore); ok {
		store = d.Store
	}
	if l, ok := store.(liveIDser); ok {
		return l.LiveIDs(ctx, ids)
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if _, err := store.Get(ctx, id); err == nil {
			out[id] = true
		}
	}
	return out, nil
}
