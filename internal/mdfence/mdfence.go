// Package mdfence lists the fenced code blocks of a Markdown message, in
// order: what the chief's session_context names a message's blocks by
// (docs/design/chief-in-agent-view.md, 4.1), so a kickoff written as one block
// (the handoff skill's) can be named {ref, block: k} instead of being copied.
//
// It follows CommonMark's fenced code blocks where it matters for naming:
//
//   - an opening fence is three or more backticks or tildes, indented at most
//     three spaces, followed by an optional info string whose first word is the
//     block's language (a backtick fence's info string has no backtick);
//   - the block ends at a line of the SAME character, at least as long, with
//     nothing else on it but spaces (indented at most three spaces);
//   - a block never closed runs to the end of the message.
//
// Everything else of Markdown — lists, quotes, indented code — is not parsed:
// a fence inside a list item indented four or more spaces is not a block here.
package mdfence

import "strings"

// Block is one fenced code block: its language (the info string's first
// word, "" for none), its body without the fence lines, and where it starts
// (the opening fence's line, from 0).
type Block struct {
	Lang string
	Body string
	Line int
}

// Blocks is text's fenced code blocks, in order.
func Blocks(text string) []Block {
	lines := strings.Split(text, "\n")
	var out []Block
	for i := 0; i < len(lines); i++ {
		ch, n, info, ok := opening(lines[i])
		if !ok {
			continue
		}
		b := Block{Lang: firstWord(info), Line: i}
		var body []string
		j := i + 1
		for ; j < len(lines); j++ {
			if closing(lines[j], ch, n) {
				break
			}
			body = append(body, strings.TrimSuffix(lines[j], "\r"))
		}
		b.Body = strings.Join(body, "\n")
		out = append(out, b)
		i = j // past the closing fence (or the end)
	}
	return out
}

// indent is how many leading spaces s has, and s without them.
func indent(s string) (int, string) {
	t := strings.TrimLeft(s, " ")
	return len(s) - len(t), t
}

// opening reports whether line opens a fence: its character, its length, and
// its info string.
func opening(line string) (byte, int, string, bool) {
	line = strings.TrimSuffix(line, "\r")
	sp, t := indent(line)
	if sp > 3 || len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return 0, 0, "", false
	}
	ch := t[0]
	n := 0
	for n < len(t) && t[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, "", false
	}
	info := strings.TrimSpace(t[n:])
	if ch == '`' && strings.Contains(info, "`") {
		return 0, 0, "", false
	}
	return ch, n, info, true
}

// closing reports whether line closes a fence of ch at least n long.
func closing(line string, ch byte, n int) bool {
	line = strings.TrimSuffix(line, "\r")
	sp, t := indent(line)
	if sp > 3 {
		return false
	}
	t = strings.TrimRight(t, " ")
	if len(t) < n {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] != ch {
			return false
		}
	}
	return true
}

// firstWord is s's first space-separated word.
func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}
