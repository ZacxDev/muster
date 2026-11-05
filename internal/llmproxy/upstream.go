package llmproxy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// foldSystemMessages rewrites a request's `messages` so that it carries no
// `role: "system"` entry, and reports how many it folded (0: raw is returned
// unchanged, byte for byte).
//
// 🔴 THE PROVIDER REFUSES A SYSTEM MESSAGE INSIDE `messages`. Claude Code sends
// mid-conversation system messages (an entry with role "system" in
// `messages`; on a session's first turn it holds the environment block), and
// the provider's Anthropic-compatible endpoint answers every such request for
// the pinned non-Anthropic models with HTTP 400 "Mid-conversation reasoning
// effort (configuration_update) is not supported on <model>". That was almost
// every routine-tier turn.
//
// Each system entry's content blocks join the adjacent user turn (a string is
// made a text block; cache_control and every other block field are kept), so
// the content stays where it was in the conversation and the cached prefix
// stays stable. Where no user turn is adjacent (between two assistant turns)
// the entry becomes a user turn of its own. Two user turns in a row are not an
// option: the provider refuses those too. In a turn that something was folded
// into, tool_result blocks are moved first, as the Messages API requires of a
// turn answering a tool_use. Only the turns a system entry touched are
// re-encoded; every other entry is passed through as the client wrote it.
//
// A body whose `messages` is not an array of objects, or whose system entry's
// content is neither a string nor an array, is returned unchanged: the
// provider's own error then says what is wrong, and upstreamError records it.
func foldSystemMessages(raw json.RawMessage) (json.RawMessage, int) {
	if !bytes.Contains(raw, []byte(`"system"`)) {
		return raw, 0
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return raw, 0
	}
	turns := make([]turn, 0, len(msgs))
	n := 0
	for _, m := range msgs {
		var head struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &head); err != nil {
			return raw, 0
		}
		t := turn{raw: m, role: head.Role}
		if head.Role == "system" {
			blocks, ok := contentBlocks(head.Content)
			if !ok {
				return raw, 0
			}
			t = turn{role: "user", blocks: blocks, touch: true}
			n++
		}
		turns = append(turns, t)
	}
	if n == 0 {
		return raw, 0
	}
	// Coalesce each folded entry with the user turns next to it.
	out := make([]turn, 0, len(turns))
	for _, t := range turns {
		if len(out) > 0 {
			prev := &out[len(out)-1]
			if prev.role == "user" && t.role == "user" && (prev.touch || t.touch) {
				pb, ok1 := turnBlocks(*prev)
				tb, ok2 := turnBlocks(t)
				if !ok1 || !ok2 {
					return raw, 0
				}
				*prev = turn{role: "user", blocks: append(pb, tb...), touch: true}
				continue
			}
		}
		out = append(out, t)
	}
	enc := make([]json.RawMessage, 0, len(out))
	for _, t := range out {
		if !t.touch {
			enc = append(enc, t.raw)
			continue
		}
		b, err := json.Marshal(struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		}{"user", toolResultsFirst(t.blocks)})
		if err != nil {
			return raw, 0
		}
		enc = append(enc, b)
	}
	b, err := json.Marshal(enc)
	if err != nil {
		return raw, 0
	}
	return b, n
}

// turn is one entry of `messages` while foldSystemMessages works on it.
type turn struct {
	raw    json.RawMessage // the client's own bytes, when not touched
	role   string
	blocks []json.RawMessage // set when touched
	touch  bool              // folded into, so re-encoded
}

// turnBlocks is a turn's content as blocks.
func turnBlocks(t turn) ([]json.RawMessage, bool) {
	if t.touch {
		return t.blocks, true
	}
	var head struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(t.raw, &head); err != nil {
		return nil, false
	}
	return contentBlocks(head.Content)
}

// contentBlocks is a message's content as an array of blocks: a string is one
// text block.
func contentBlocks(c json.RawMessage) ([]json.RawMessage, bool) {
	var s string
	if err := json.Unmarshal(c, &s); err == nil {
		b, err := json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"text", s})
		return []json.RawMessage{b}, err == nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(c, &blocks); err != nil || blocks == nil {
		return nil, false
	}
	return blocks, true
}

// toolResultsFirst moves tool_result blocks to the front, keeping the order
// within each group.
func toolResultsFirst(blocks []json.RawMessage) []json.RawMessage {
	var results, rest []json.RawMessage
	for _, b := range blocks {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &head) == nil && head.Type == "tool_result" {
			results = append(results, b)
		} else {
			rest = append(rest, b)
		}
	}
	return append(results, rest...)
}

// maxUpstreamErrorBytes bounds Record.UpstreamError (before the ellipsis).
const maxUpstreamErrorBytes = 300

// keyShaped matches what looks like a credential: an `sk-…` key of any
// provider, and a bearer token.
var (
	keyShaped    = regexp.MustCompile(`sk-[A-Za-z0-9][A-Za-z0-9_-]{15,}`)
	authSchemeToken = regexp.MustCompile(`(?i)\b(bearer)\s+[^\s"',;]+`)
)

// upstreamError is a non-2xx provider body as one line for the audit record
// and the log: "<type>: <message>" from an Anthropic- or OpenAI-shaped error
// object, else the body's own text; whitespace collapsed; the officer's key
// and anything key-shaped redacted; then truncated to maxUpstreamErrorBytes
// (on a rune boundary). Redaction comes first, so a truncation can never leave
// part of a key behind.
func upstreamError(body []byte, key string) string {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	s := ""
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		s = e.Error.Message
		typ := e.Error.Type
		if typ == "" {
			if c, ok := e.Error.Code.(string); ok {
				typ = c
			}
		}
		if typ != "" {
			s = typ + ": " + s
		}
	} else {
		s = string(body)
	}
	s = strings.Join(strings.Fields(s), " ")
	if key != "" {
		s = strings.ReplaceAll(s, key, "[redacted]")
	}
	s = keyShaped.ReplaceAllString(s, "[redacted]")
	s = authSchemeToken.ReplaceAllString(s, "${1} [redacted]")
	if len(s) > maxUpstreamErrorBytes {
		cut := maxUpstreamErrorBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}
