package cctranscript

import (
	"regexp"
	"strings"
)

// THE CLI'S PASTE WRAPPER, READ BACK. A paste the Claude Code TUI collapses
// into a "[Pasted text]" placeholder is recorded — in the transcript and in the
// UserPromptSubmit hook's prompt alike — wrapped in its own tags, and any
// `<pasted_content` the prompt itself holds is escaped. These are the one
// reading of both: cmd/ccd matches a turn it pasted with them, and the
// mirrored view (internal/plugins RoutedPromptText) reads a routed paste back
// with them. Moved here from cmd/ccd/server.go unchanged.

// escapedPastedContentTag finds a `<` followed by a backslash, any run of
// characters that are neither ASCII name characters nor angle brackets
// (whitespace included), and the tag name in any ASCII case.
var escapedPastedContentTag = regexp.MustCompile(`(?i)<\\([^A-Za-z0-9_<>-]*pasted_content)`)

// UnescapePastedContentTag removes the backslash the CLI puts after a `<` that
// could be read as its paste-wrapper tag.
//
// The CLI's rule, read from the pinned CLI's bundle (2.1.296) and measured on its
// real TUI: in ANY submitted prompt, wrapped or not, a `<` that is not already
// followed by a backslash, then any run of characters that are not name
// characters or angle brackets (so `/`, spaces, punctuation), then
// `pasted_content` in any case, then a non-name character or the end, has `\`
// inserted after the `<`. Measured: `<pasted_content id="abcd">` →
// `<\pasted_content id="abcd">`, `</pasted_content>` → `<\/pasted_content>`,
// `<PASTED_CONTENT>` → `<\PASTED_CONTENT>`, `< pasted_content` →
// `<\ pasted_content`, `<#/pasted_content` → `<\#/pasted_content`; a
// `<\pasted_content` already in the prompt is left as it is (2.1.296 in the
// image, `tengu_virtual_pancake` on, 2026-10-11; not measured with it off).
//
// ccd cannot tell an inserted backslash from one the prompt already had, so it
// does not try: the backslash is removed on both sides of the comparison, and a
// real one cancels out.
//
// ⚠ ACCEPTED LIMITS. (1) Two prompts that differ ONLY in such a backslash have
// the same key, so an operator submitting `<\pasted_content` while ccd waits on
// the same text with `<pasted_content` would bind to ccd's turn. (2) The CLI also
// escapes look-alike spellings — a fullwidth `＜`, which it REPLACES with `<\`
// (measured: `＜pasted_content>` came back `<\pasted_content>`, a not_submitted),
// look-alike letters, other underscore-like characters. Those are not undone, so
// ccd's OWN prompt holding one is a not_submitted. The other direction is a wrong
// match ccd cannot prevent: a kickoff holding `<pasted_content>` and an operator
// typing the same text with `＜` or `‹` are made byte-identical by the CLI (both
// reported as `<\pasted_content>`), so the operator's submit binds to ccd's turn.
// (3) The CLI requires the name to END there (`<pasted_contents` is not escaped);
// this does not check that. It only widens (1) to longer names.
func UnescapePastedContentTag(key string) string {
	return escapedPastedContentTag.ReplaceAllString(key, "<$1")
}

func isTagNameByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-'
}

// UnwrapPastedContent returns the body of a prompt that is, whitespace around it
// aside, exactly ONE block of the CLI's paste wrapper:
//
//	\n\n<pasted_content id="1c0d">\n…body…\n</pasted_content id="1c0d">\n
//
// The CLOSING tag repeats the opening tag's id. ok is false for anything else —
// text before or after the block, differing ids, an empty id, a plain
// `</pasted_content>` close (the pinned CLI never writes one), or a body holding
// another unescaped tag (two blocks) — so a prompt that merely CONTAINS the paste
// is still not ccd's turn. The body is returned as reported, tag escaping and all
// (cmd/ccd turnKey removes that).
//
// What makes this safe is the anchoring and the id repeated in both tags, not the
// id's spelling: the pinned CLI writes four lowercase hex digits, and any
// non-empty run of ASCII letters, digits, `_` and `-` is accepted so that a CLI
// that lengthens its id does not bring the false not_submitted back.
//
// Measured on the pinned CLI (2.1.296, the real TUI in the image, pasted by ccd,
// hook payloads captured with CCD_HOOK_LOG, 2026-10-11): with the CLI's
// `tengu_virtual_pancake` feature flag on, a paste the TUI collapses into a
// "[Pasted text]" placeholder is reported — in the hook's `prompt` and in the
// transcript alike — in that wrapper: 960, 3690, 10990 and 12000 characters and a
// single 900-character line were wrapped; 19 characters on one line and on two
// lines were not. With the flag off (a CLI with no cached flags) the same seven
// came back verbatim. The flag is served to the CLI per account, so which one a
// pod gets is not ccd's to know: both are accepted. A paste's trailing newline is
// not doubled inside the block (promptKey ignores it).
func UnwrapPastedContent(reported string) (body string, ok bool) {
	const open, tagEnd, closeStart = `<pasted_content id="`, "\">\n", "\n</pasted_content id=\""
	s := strings.TrimSpace(reported)
	rest, found := strings.CutPrefix(s, open)
	if !found {
		return "", false
	}
	n := 0
	for n < len(rest) && isTagNameByte(rest[n]) {
		n++
	}
	if n == 0 {
		return "", false
	}
	id := rest[:n]
	if rest, found = strings.CutPrefix(rest[n:], tagEnd); !found {
		return "", false
	}
	if body, found = strings.CutSuffix(rest, closeStart+id+`">`); !found {
		return "", false
	}
	if strings.Contains(body, "<pasted_content") || strings.Contains(body, "</pasted_content") {
		return "", false
	}
	return body, true
}
