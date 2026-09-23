package ui

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/notes"
)

// Shared helpers for this package's tests.
//
// 🔴 THEY LIVE IN THEIR OWN FILE, WHICH IS A DELIBERATE CHANGE FROM UPSTREAM.
// There these three were defined inside whichever large test file happened to
// need them first, so a test file that used `renderString` silently depended on
// a11y_test.go being present in the package. That is invisible while the whole
// suite moves together and immediately fatal when part of it is carved out —
// which is exactly what happened here: the first compile of the moved tests
// failed on `undefined: renderString`, naming a symbol whose owning file was
// never part of the moving set and had no reason to be.
//
// Keeping them here means "which file owns the helper" is answerable by looking
// at the name, and a future carve takes one obvious file with it.

// renderString renders a node to its HTML/JS source text.
func renderString(t *testing.T, n g.Node) string {
	t.Helper()
	var b bytes.Buffer
	if err := n.Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// renderNode is renderString under the name some tests call it by.
func renderNode(t *testing.T, n g.Node) string {
	t.Helper()
	return renderString(t, n)
}

// documentSource renders a document and strips the COMMENTS out of every inline
// <script> body, leaving the markup and the executable JavaScript intact.
//
// 🔴 IT EXISTS BECAUSE TWO GUARDS WRITTEN IN THIS COMMIT FAILED AGAINST THEIR
// OWN PROSE, WHICH IS THE SAME DEFECT IN THE OPPOSITE DIRECTION. Every document
// this package renders embeds appScript, whose comments legitimately quote the
// markup and the attribute names they are about — `<h1 id="page-heading">` in
// the heading comment, `data-action-url` in the comment recording what was
// deleted. A guard that counts `<h1` or greps for an attribute over the raw
// render therefore reads PROSE as if it were markup: the one-h1 sweep counted
// two on every document, and the router-surface sweep reported a leak whose only
// occurrence was the sentence saying it had been removed.
//
// The failure direction matters. Here it was LOUD — the guards went red and the
// prose was the cause. The dangerous direction is the mirror image and is what
// upstream measured: a comment that MENTIONS the thing keeps a
// `strings.Contains` guard GREEN after the code it was about has been deleted.
// Stripping comments is what makes these assertions about behaviour.
//
// ⚠ IT STRIPS COMMENTS, NOT SCRIPTS. Deleting whole <script> bodies would be
// simpler and would also have made both guards pass — and it would have made
// TestTheShellCarriesNoPermissionRouterSurface structurally unable to see a
// revived listener, which is the most likely way that surface comes back.
func documentSource(t *testing.T, n g.Node) string {
	t.Helper()
	raw := renderString(t, n)

	var out strings.Builder
	rest := raw
	for {
		open := strings.Index(rest, "<script")
		if open < 0 {
			out.WriteString(rest)
			break
		}
		gt := strings.Index(rest[open:], ">")
		close := strings.Index(rest[open:], "</script>")
		if gt < 0 || close < 0 || close < gt {
			// Not a well-formed inline script (or an external one with no body);
			// copy it through untouched rather than guessing.
			out.WriteString(rest[:open+len("<script")])
			rest = rest[open+len("<script"):]
			continue
		}
		bodyStart := open + gt + 1
		bodyEnd := open + close
		out.WriteString(rest[:bodyStart])
		stripped, err := stripJSComments(rest[bodyStart:bodyEnd])
		if err != nil {
			t.Fatalf("documentSource: stripJSComments desynced on an inline script: %v", err)
		}
		out.WriteString(stripped)
		rest = rest[bodyEnd:]
	}
	return out.String()
}

// jsSource renders a <script> node and returns its JavaScript BODY, without the
// surrounding tags. The tags matter: `</script>` contains a `/` that the
// regex/division heuristic in stripJSComments reads as a regex opener (it
// follows a `<`, an operator), which then runs to EOF and reports a desync.
func jsSource(t *testing.T, n g.Node) string {
	t.Helper()
	raw := renderString(t, n)
	if !strings.HasPrefix(raw, "<script") {
		t.Fatalf("jsSource: node does not render a <script> element: %.40q", raw)
	}
	i := strings.Index(raw, ">")
	j := strings.LastIndex(raw, "</script>")
	if i < 0 || j < i {
		t.Fatalf("jsSource: cannot find the script body in %.40q", raw)
	}
	return raw[i+1 : j]
}

// stripJSComments removes // line comments and /* */ block comments from a
// JavaScript source, leaving string, template and REGEX literals intact.
//
// It exists because every assertion below about what the SPA script DOES is
// otherwise text-walkable by the prose ABOVE the code: appScript's doc comment
// for the HEADINGS map literally contains `<h1 id="page-heading">`, so
// `strings.Contains(js, "page-heading")` stayed green after the two lines that
// actually rewrite the heading were deleted. Asserting on the comment-stripped
// source is what makes those guards about behaviour.
//
// Regex literals are NOT optional to handle, contrary to what an earlier
// revision of this comment claimed. appScript contains several, one of which is
// `aaProject.replace(/"/g, '\\"')` — the `"` inside that regex flipped the old
// scanner into a double-quoted string, desynced quote parity for the rest of
// the file, and left a real comment line un-stripped (MEASURED: one survivor,
// "// Telemetry: the human's permission decision ..."). The failure direction
// is silently PERMISSIVE — surviving comments make comment-stripped guards
// text-walkable again — i.e. exactly the defect class these guards exist to
// remove.
//
// Regex-vs-division is decided by the standard previous-significant-token
// heuristic (regexCanStartAfter), not by a real JS parser, so it is not
// provably correct for arbitrary JS. A desync is therefore made LOUD rather
// than silent: an unterminated string/regex/block comment, or a regex literal
// that reaches a newline, returns an error.
// TestStripJSCommentsLeavesNoCommentsInAppScript is the end-to-end check that
// it actually parses THIS script.
func stripJSComments(src string) (string, error) {
	const (
		code = iota
		lineComment
		blockComment
		inSingle
		inDouble
		inTick
		inRegex
		inRegexClass
	)
	var out strings.Builder
	m := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch m {
		case code:
			if c == '/' && i+1 < len(src) && src[i+1] == '/' {
				m, i = lineComment, i+1
				continue
			}
			if c == '/' && i+1 < len(src) && src[i+1] == '*' {
				m, i = blockComment, i+1
				continue
			}
			switch c {
			case '\'':
				m = inSingle
			case '"':
				m = inDouble
			case '`':
				m = inTick
			case '/':
				if regexCanStartAfter(out.String()) {
					m = inRegex
				}
			}
			out.WriteByte(c)
		case lineComment:
			if c == '\n' {
				m = code
				out.WriteByte(c)
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				m, i = code, i+1
			}
		case inRegex, inRegexClass:
			out.WriteByte(c)
			if c == '\n' {
				// A regex literal cannot span a line, so the heuristic mis-read
				// a division as a regex opener. Fail loudly rather than silently
				// swallowing the rest of the file.
				return "", fmt.Errorf("stripJSComments: unterminated regex literal at byte %d — the regex/division heuristic mis-fired", i)
			}
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
				continue
			}
			switch {
			case m == inRegex && c == '[':
				m = inRegexClass
			case m == inRegexClass && c == ']':
				m = inRegex
			case m == inRegex && c == '/':
				m = code
			}
		default: // inside a string / template literal
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
				continue
			}
			if (m == inSingle && c == '\'') || (m == inDouble && c == '"') || (m == inTick && c == '`') {
				m = code
			}
		}
	}
	switch m {
	case code, lineComment:
		return out.String(), nil
	default:
		return "", fmt.Errorf("stripJSComments: source ended inside a string/regex/block comment (scanner state %d) — quote parity desynced", m)
	}
}

// jsRegexKeywords are the JS keywords after which a `/` opens a regex literal
// rather than being a division operator — the only identifier-shaped tokens for
// which that is true.
var jsRegexKeywords = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true,
	"new": true, "delete": true, "void": true, "throw": true, "case": true,
	"do": true, "else": true, "yield": true, "await": true,
}

var jsIdentTail = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]*$`)

// regexCanStartAfter reports whether a `/` following the already-emitted code
// starts a regex literal. Standard heuristic: a regex cannot follow a VALUE, so
// it cannot follow an identifier (unless that identifier is a keyword), a
// number, a closing bracket, or a string literal.
func regexCanStartAfter(prev string) bool {
	prev = strings.TrimRight(prev, " \t\r\n")
	if prev == "" {
		return true
	}
	last := prev[len(prev)-1]
	switch {
	case last == ')' || last == ']' || last == '\'' || last == '"' || last == '`':
		return false
	case last >= '0' && last <= '9':
		return false
	case jsIdentTail.MatchString(prev):
		return jsRegexKeywords[jsIdentTail.FindString(prev)]
	}
	return true
}

// mustStripJSComments strips comments and fails the test on a scanner desync.
func mustStripJSComments(t *testing.T, src string) string {
	t.Helper()
	out, err := stripJSComments(src)
	if err != nil {
		t.Fatalf("stripJSComments: %v", err)
	}
	return out
}

// TestStripJSCommentsIsAWorkingInstrument is the positive/negative control for
// the helper every script assertion below depends on. Without it a bug that
// made stripJSComments return its input unchanged would silently restore the
// text-walkability those guards exist to remove.
func TestStripJSCommentsIsAWorkingInstrument(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"line comment removed", "a; // page-heading\nb;", "a; \nb;"},
		{"block comment removed", "a; /* page-heading */ b;", "a;  b;"},
		{"multiline block removed", "a;\n/*\n * page-heading\n */\nb;", "a;\n\nb;"},
		{"code kept", "heading.textContent = HEADINGS[tab];", "heading.textContent = HEADINGS[tab];"},
		{"string literal kept verbatim", `var u = "http://x/y"; // gone`, `var u = "http://x/y"; `},
		{"slash inside single quotes kept", `var s = '// not a comment';`, `var s = '// not a comment';`},
		{"template literal kept", "var t = `a /* b */ c`;", "var t = `a /* b */ c`;"},
		{"escaped quote does not end the string", `var s = 'it\'s // fine'; // gone`, `var s = 'it\'s // fine'; `},
		// Regex literals. The first is the shape MEASURED in appScript that broke
		// the previous scanner: the `"` inside the regex flipped it into a
		// double-quoted string and desynced everything after it.
		{"regex containing a double quote does not open a string",
			`var e = p.replace(/"/g, '\\"'); // gone`, `var e = p.replace(/"/g, '\\"'); `},
		{"regex containing a // sequence is not a comment",
			`var re = /a\/\/b/; // gone`, `var re = /a\/\/b/; `},
		{"character class containing a slash", `var re = /[/]x/; // gone`, `var re = /[/]x/; `},
		{"plain regex kept", `var re = /\d+/g; // gone`, `var re = /\d+/g; `},
		{"slash after an identifier is division, not a regex",
			"var q = total/count; // gone\nvar z = 1;", "var q = total/count; \nvar z = 1;"},
		{"slash after a closing paren is division", "var q = (a+b)/2; // gone\nx;", "var q = (a+b)/2; \nx;"},
		{"regex after the return keyword", `return /x/.test(s); // gone`, `return /x/.test(s); `},
	}
	for _, tc := range cases {
		got, err := stripJSComments(tc.in)
		if err != nil {
			t.Errorf("%s: stripJSComments(%q) errored: %v", tc.name, tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: stripJSComments(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	// NEGATIVE control: the scanner must be able to go red at all.
	for _, bad := range []string{
		`var s = "unterminated`,
		"var q = a +/ 2;\nnext;", // heuristic mis-fire → regex hits a newline
	} {
		if _, err := stripJSComments(bad); err == nil {
			t.Errorf("stripJSComments accepted %q — it cannot report a desync", bad)
		}
	}
}

// renderDetailCard renders the DETAIL shape of the same card — the one the
// /tasks/{id} page is built from. Comments, attachments, the session thread and
// the add-comment form live ONLY there now, so any test about them must use
// this; renderCard (the board shape) deliberately renders none of it.
func renderDetailCard(w io.Writer, n notes.Note) error {
	return RenderNoteCard(w, TaskCardView{Note: n, Detail: true})
}

// renderCard renders a task card with NO linked agent (the common case these
// card tests exercise). Tests that need the dispatched-card state call
// RenderNoteCard directly with a TaskCardView carrying an AgentBrief.
func renderCard(w io.Writer, n notes.Note) error { return RenderNoteCard(w, TaskCardView{Note: n}) }
