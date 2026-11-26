package agents

import (
	"context"
)

// RECENT MODELS: the OpenRouter models an agent's chat sessions were opened
// on, most recently used first — what the chief's OpenRouter model fields
// (New session, Continue) list before anything is typed (internal/api
// chiefRecentModels).
//
// 🔴 "USED" IS A SESSION'S LAST ACTIVITY, chat_sessions.updated_at, which only
// a stored message moves (AddChatMessage; archiving, Make primary, a read or a
// title do not). A model's place is its NEWEST session's, so a model picked
// long ago and talked to a minute ago is near the top, and a model with many
// sessions all gone quiet is not lifted by their number. Archived sessions
// count: an archived chat was still used on its model.

// RecentModelStore is the part of the session store the recent list reads.
// PGStore implements it; it is a separate interface so the many fakes of
// [Store] do not each grow a method, and a caller asserts it and lists
// nothing without it.
type RecentModelStore interface {
	// RecentOpenRouterModels is agentID's sessions' distinct OpenRouter models
	// (SessionOnOpenRouter), most recently used first, at most limit.
	RecentOpenRouterModels(ctx context.Context, agentID int64, limit int) ([]string, error)
}

// RecentOpenRouterModels implements RecentModelStore.
func (s *PGStore) RecentOpenRouterModels(ctx context.Context, agentID int64, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT model FROM chat_sessions
		WHERE agent_id = $1 AND model LIKE $2
		GROUP BY model
		ORDER BY max(updated_at) DESC, model
		LIMIT $3`, agentID, OpenRouterModelPrefix+"_%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		if SessionOnOpenRouter(m) {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}
