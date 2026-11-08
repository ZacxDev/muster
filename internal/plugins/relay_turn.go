package plugins

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/fence"
)

// RelayTurn is what a STORED chief turn says about the plugin message it
// relayed, read back from muster's own words in it (ParseRelayTurn).
type RelayTurn struct {
	// Plugin is the plugin's label as the preface spells it (sanitised, cut).
	Plugin string
	// MessageID is the plugin message's id; TaskID the task of a task event,
	// 0 for a message.
	MessageID, TaskID int64
	// Subject is the message's subject — a task event's title — as the fenced
	// block carries it (sanitised), "" when the block names none.
	Subject string
}

// relayPreface matches the START of the preface a relayed turn begins with: a
// message's (preface) or a task event's (taskEventPreface). Anchored at the
// first byte.
var relayPreface = regexp.MustCompile(`^Plugin (.+?): (?:message_id (\d+)|task event for task_id (\d+) \(message_id (\d+)\)), delivered to you once\. `)

// ParseRelayTurn reads a stored chief turn written by RenderMessage (a plugin
// message, or a task event) back into its plugin, ids and subject; ok false
// for any other text.
//
// 🔴 THE CALLER MUST ALREADY KNOW THE ROW IS NOT A PERSON'S. chat_messages has
// no column linking a row to the plugin message it relayed — Deliverer stores
// the rendered turn as a plain `user` row — so the only STRUCTURAL fact about
// such a row is the negative one: typed_by is 0 (agents.ChatMessage.TypedBy:
// nobody typed it into muster's chat). This function then names WHICH muster
// relay it is from muster's own preface, which a person typing can spell too:
// a typed row is never passed here (internal/api chatLinesOf), so an owner who
// pastes a relay's text keeps their bubble.
//
// ⚠ THE PREFACE IS MUSTER'S TEXT, NOT THE SENDER'S. It is the turn's first line,
// outside the fence, and every outside field in it (the label) is sanitised, so
// a sender cannot make a row START this way; what the sender writes is inside
// the fence, where only the first block's subject (or title) line is read — a
// line that begins with the fence's ContentPrefix and muster's own field name,
// with a sanitised value that cannot carry a newline. Bound to the renderer by
// TestARelayTurnReadsBackWhatRenderMessageWrote.
func ParseRelayTurn(content string) (RelayTurn, bool) {
	m := relayPreface.FindStringSubmatch(content)
	if m == nil {
		return RelayTurn{}, false
	}
	r := RelayTurn{Plugin: m[1]}
	field := "  subject: "
	if m[2] != "" {
		r.MessageID, _ = strconv.ParseInt(m[2], 10, 64)
	} else {
		r.TaskID, _ = strconv.ParseInt(m[3], 10, 64)
		r.MessageID, _ = strconv.ParseInt(m[4], 10, 64)
		field = "  title: "
	}
	// The first fenced block only: a hint or author block follows it under its
	// own nonce, and its lines are not this message's.
	in := false
	for _, line := range strings.Split(content, "\n") {
		switch {
		case !in:
			in = strings.HasPrefix(line, "<<<")
		case strings.HasPrefix(line, "<<<end "):
			return r, true
		case strings.HasPrefix(line, fence.ContentPrefix+field):
			r.Subject = strings.TrimSpace(strings.TrimPrefix(line, fence.ContentPrefix+field))
			return r, true
		}
	}
	return r, true
}
