package ui

import (
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/notes"
)

// taskClaimChip is the board/detail chip of a CLAIMED task (notes.Claims —
// the one predicate the task triage and the chief's card also read):
// "⚑ worked by <claimants>". data-task-claimed carries the claim sources, so a
// probe asserts the state, not the words. Nothing for an unclaimed task.
func taskClaimChip(n notes.Note) g.Node {
	claims := notes.Claims(n)
	if len(claims) == 0 {
		return g.Text("")
	}
	names := make([]string, 0, len(claims))
	sources := make([]string, 0, len(claims))
	for _, c := range claims {
		names = append(names, c.Who)
		if len(sources) == 0 || sources[len(sources)-1] != c.Source {
			sources = append(sources, c.Source)
		}
	}
	who := strings.Join(names, ", ")
	return Span(
		g.Attr("data-task-claimed", strings.Join(sources, " ")),
		g.Attr("title", "Claimed: "+who+" is working this task"),
		Class("max-w-[12rem] truncate shrink-0 rounded-md bg-s2 px-1.5 py-0.5 text-xs text-accent"),
		g.Text("⚑ worked by "+who),
	)
}
