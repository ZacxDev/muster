package ui

import (
	"strings"
	"unicode"
)

// THE CHIEF'S REPLIES, AT A GLANCE (operator decisions, #871).

// newestReply is the index in msgs of the newest reply the log draws as an
// agent's bubble — an assistant text row that is not a "no reply" sentinel —
// or -1 for none. The chief's is drawn whole (AgentDetailView.UnfoldNewest).
func newestReply(msgs []ChatLine) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Kind == "tool_call" || m.Kind == "tool_result" || m.Relay != nil {
			continue
		}
		if m.Role == "assistant" && !isSentinelReply(m.Content) {
			return i
		}
	}
	return -1
}

// NO DUPLICATE RELAY. A session's answer to a routed request is drawn as its
// "From <session>" card (routed_card.go), and the chief is told not to repeat
// it (internal/api routeAnswerPreface). The backstop, for a model that does
// anyway: a chief reply drawn after the card that repeats an answer — the
// whole reply, or a run of it, equal word for word to the answer once
// whitespace, quote marks and markdown quoting are set aside — has that run
// collapsed to relayCollapsed. What the reply adds (a next step) is kept.
//
// 🔴 ONLY THE DRAWING CHANGES: the stored reply, and what the chief is handed,
// are untouched.

// relayCollapsed replaces a repeated answer.
const relayCollapsed = "*See the answer above.*"

// relayMinWords is how long an answer must be for a reply that merely
// CONTAINS it to be collapsed; a reply that is the answer and nothing else is
// collapsed whatever its length. A two-word answer ("Done, merged") is too
// likely to recur in a reply's own words.
const relayMinWords = 3

// ⚠ ITS SLICES GO THROUGH span (mentions.go): Tailwind scans this package's
// Go, and a slice bounded by names reads to it as an arbitrary property.

// relayWord is one word of a text: its comparable form and its byte span.
type relayWord struct {
	w          string
	start, end int
}

// relayTrim is what a word's comparison ignores at its ends: quote marks,
// markdown's emphasis, code and blockquote marks.
const relayTrim = "\"'“”‘’*_`>"

// relayWords is s's words, each trimmed of relayTrim; a word that is nothing
// else (a lone ">") is dropped.
func relayWords(s string) []relayWord {
	var out []relayWord
	i := 0
	for i < len(s) {
		for i < len(s) && relaySpace(s[i]) {
			i++
		}
		j := i
		for j < len(s) && !relaySpace(s[j]) {
			j++
		}
		if j > i {
			if w := strings.Trim(span(s, i, j), relayTrim); w != "" {
				out = append(out, relayWord{w: w, start: i, end: j})
			}
		}
		i = j
	}
	return out
}

func relaySpace(b byte) bool { return b < 0x80 && unicode.IsSpace(rune(b)) }

// dedupRelay is reply with every run that repeats one of answers collapsed
// (see above); reply unchanged when none does.
func dedupRelay(reply string, answers []string) string {
	for _, a := range answers {
		reply = dedupOne(reply, a)
	}
	return reply
}

func dedupOne(reply, answer string) string {
	aw, rw := relayWords(answer), relayWords(reply)
	n := len(aw)
	if n == 0 || len(rw) < n {
		return reply
	}
	if n < relayMinWords && len(rw) != n {
		return reply
	}
	for i := 0; i+n <= len(rw); i++ {
		match := true
		for k := 0; k < n; k++ {
			if rw[i+k].w != aw[k].w {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		start, end := rw[i].start, rw[i+n-1].end
		// The whole of the lines it sits on, when nothing else is on them but
		// quoting: a quoted answer leaves no stray ">" behind.
		ls := strings.LastIndexByte(span(reply, 0, start), '\n') + 1
		if strings.Trim(span(reply, ls, start), " \t>") == "" {
			start = ls
		}
		le := strings.IndexByte(span(reply, end, len(reply)), '\n')
		if le < 0 {
			le = len(reply)
		} else {
			le += end
		}
		if strings.Trim(span(reply, end, le), " \t\"'“”’") == "" {
			end = le
		}
		return span(reply, 0, start) + relayCollapsed + span(reply, end, len(reply))
	}
	return reply
}
