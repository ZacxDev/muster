package agents

import "context"

// THE TURN'S OWN PROMPT ROW, handed from the door that stored it to the turn's
// context hook: the kickoff deliverer stores a first turn's user row BEFORE it
// sends the turn (internal/agentkickoff), and puts the row's id here, so the
// hook it calls (internal/api OfficerTurnContext) can record the running
// turn's input for the turn's whole run — what an officer's tool call reads
// as the moment it was made in (migration 0185, "Show session").

type turnInputRowKey struct{}

// WithTurnInputRow is ctx for a turn whose prompt is chat_messages row id.
func WithTurnInputRow(ctx context.Context, id int64) context.Context {
	if id <= 0 {
		return ctx
	}
	return context.WithValue(ctx, turnInputRowKey{}, id)
}

// TurnInputRow is ctx's turn's prompt row, 0 for none.
func TurnInputRow(ctx context.Context) int64 {
	id, _ := ctx.Value(turnInputRowKey{}).(int64)
	return id
}
