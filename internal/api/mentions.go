package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/ui"
)

// mentionIndex is what an assistant's reply can link to (ui/mentions.go), as
// ctx's principal sees it: the agents and mirrored sessions the session
// switcher lists — the same agents list (provisioned only, an off chief left
// out) and the same mirror hub entries agentsListEntries builds its entries
// from — and the tasks the task store answers for, asked lazily per id the
// reply names (remembered and capped by the index).
//
// 🔴 BUILT ONCE PER RESPONSE: one agents read and one walk of the in-memory
// hub, never per message. It skips what agentsListEntries adds for the
// sidebar (activity order, pins, focus titles, tags) — the index needs names
// and addresses only.
//
// No agent store gives an index of the mirrors alone; a failed agents read
// (logged) does the same rather than failing the page.
func (s *Server) mentionIndex(ctx context.Context) *ui.MentionIndex {
	var names []string
	if s.ext.Agents != nil {
		list, err := s.ext.Agents.List(ctx)
		if err != nil {
			s.logger.Printf("mentions: list agents: %v", err)
		}
		for _, a := range s.withoutOffChief(ctx, agents.ProvisionedOnly(list)) {
			names = append(names, a.Name)
		}
	}
	var task func(int64) bool
	if s.ext.Notes != nil {
		task = func(id int64) bool {
			_, err := s.ext.Notes.Get(ctx, id)
			return err == nil
		}
	}
	return ui.NewMentionIndex(names, s.mirrorCards(ctx, pins.Set{}, nil), task)
}

// hasAssistantText reports whether lines hold an assistant reply to link
// mentions in: a conversation without one needs no index (and no reads).
func hasAssistantText(lines []ui.ChatLine) bool {
	for _, l := range lines {
		if l.Role == "assistant" && l.Kind != "tool_call" && l.Kind != "tool_result" && l.Content != "" {
			return true
		}
	}
	return false
}
