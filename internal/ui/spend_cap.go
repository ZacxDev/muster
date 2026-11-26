package ui

import (
	"encoding/json"
	"fmt"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE BUDGET CARD (internal/api chief_session_cap.go): an OpenRouter session
// whose budget is spent is answered with this card, stored as an assistant
// part of kind ChatKindSpendCap — what was spent, the cap, when it resets, and
// Continue in a new session onto a Claude model.

// ChatKindSpendCap is the stored part's kind.
const ChatKindSpendCap = "spend_cap"

// spendCapAttr marks the card.
const spendCapAttr = "data-spend-cap"

// SpendCapCard is the card's content, stored as JSON.
type SpendCapCard struct {
	DaySpentUSD     float64   `json:"day_spent_usd"`
	DayReservedUSD  float64   `json:"day_reserved_usd"`
	DailyCapUSD     float64   `json:"daily_cap_usd"`
	SessionSpentUSD float64   `json:"session_spent_usd"`
	SessionCapUSD   float64   `json:"session_cap_usd"`
	ResetsAt        time.Time `json:"resets_at"`
	// Refusal is the door's own sentence (which cap, and why).
	Refusal string `json:"refusal"`
}

// spendCapNow is the clock the card's "in 3h 12m" reads (tests set it).
var spendCapNow = time.Now

// usd is a dollar amount as "$12.34".
func usd(v float64) string { return fmt.Sprintf("$%.2f", v) }

// spendCapCard draws the card for a stored part's content; b is the chat's
// bar (nil off a chief chat: then no Continue).
func spendCapCard(content string, b *ChiefChatBar, claudeModels []string) g.Node {
	var c SpendCapCard
	if json.Unmarshal([]byte(content), &c) != nil {
		return Div(g.Attr(spendCapAttr, ""), Class("rounded-xl bg-s1 p-3 text-sm text-fg ring-1 ring-inset ring-line"),
			g.Text("This session's OpenRouter budget is spent; nothing was sent."))
	}
	lines := []g.Node{
		P(Class("text-sm font-semibold text-fg"), g.Text("OpenRouter budget reached — nothing was sent")),
	}
	day := "Today (UTC) your chief sessions on OpenRouter have spent " + usd(c.DaySpentUSD) + " of the " + usd(c.DailyCapUSD) + " daily cap"
	if c.DayReservedUSD > 0.005 {
		day += ", with " + usd(c.DayReservedUSD) + " held by requests in flight"
	}
	lines = append(lines, P(g.Attr("data-spend-cap-day", ""), Class("text-xs text-fg2"), g.Text(day+".")))
	if c.SessionCapUSD > 0 {
		lines = append(lines, P(Class("text-xs text-fg2"),
			g.Text("This session has spent "+usd(c.SessionSpentUSD)+" of its "+usd(c.SessionCapUSD)+" cap (it does not reset).")))
	}
	if !c.ResetsAt.IsZero() {
		left := c.ResetsAt.Sub(spendCapNow()).Round(time.Minute)
		when := "The daily cap resets at 00:00 UTC (" + c.ResetsAt.UTC().Format("Mon 2 Jan") + ")"
		if left > 0 {
			when += fmt.Sprintf(", in %dh %02dm", int(left.Hours()), int(left.Minutes())%60)
		}
		lines = append(lines, P(g.Attr("data-spend-cap-reset", ""), Class("text-xs text-fg2"), g.Text(when+".")))
	}
	if b != nil && len(claudeModels) > 0 {
		if kind, _ := chiefChatKind(*b); kind == ChiefChatKindSide {
			lines = append(lines, chiefContinueGroup(b,
				[]SessionRuntimeView{{Runtime: "claude-code", Label: "Claude Code"}}, claudeModels, ChatSurfaceChiefPanel))
		}
	}
	return Div(
		g.Attr(spendCapAttr, ""),
		g.Attr("role", "status"),
		Class("flex flex-col gap-1.5 rounded-xl bg-s1 p-3 ring-1 ring-inset ring-line"),
		g.Group(lines),
	)
}
