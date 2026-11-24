package ui

import (
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// WHO TYPED A LINE OF A TEAM'S SHARED CHAT (team sharing slice 4.4: several
// members type into a team agent's one conversation). The api decides WHETHER a
// line carries a name (ChatLine.Typer: set only on a person-typed user line of a
// team's chat, internal/api chatTypers) and WHICH name; this file only draws
// it, the same way on the stored bubble and on the chat socket's echo.

// TyperNameMax is the most runes a typer's label draws; a longer name ends in
// "…". A display name has no length limit of its own, and the label sits on top
// of a bubble that is at most 85% of the log's width.
const TyperNameMax = 40

// typerName is name as its label draws it: whitespace runs (newlines, tabs)
// folded to one space, capped at TyperNameMax runes. "" stays "".
func typerName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if r := []rune(name); len(r) > TyperNameMax {
		name = strings.TrimSpace(string(r[:TyperNameMax-1])) + "…"
	}
	return name
}

// typerLabel is the small name line at the top of a team chat's typed bubble;
// nil for "" (every personal chat, and every line nobody typed). The name is
// TEXT (gomponents escapes it): a display name is the member's own words.
func typerLabel(name string) g.Node {
	name = typerName(name)
	if name == "" {
		return nil
	}
	return Div(g.Attr("data-typed-by", ""), Class("mb-1 text-xs font-medium text-fg2"), g.Text(name))
}

// TyperLabelHTML is typerLabel as HTML, "" for none: what the chat socket puts
// before a sent message's echo (internal/api wsSentEcho), so the echo draws the
// label the stored bubble will.
func TyperLabelHTML(name string) string {
	n := typerLabel(name)
	if n == nil {
		return ""
	}
	var b strings.Builder
	_ = n.Render(&b)
	return b.String()
}
