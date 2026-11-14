package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
)

// mentionIndex is what an assistant's reply can link to (ui/mentions.go), as
// ctx's principal sees it. It is LAZY (ui.MentionIndex): building it reads
// nothing, so a page, a turn or a socket whose replies name nothing costs
// nothing.
//
//   - Names, read once, on the first token that could be one: the agents
//     list (provisioned rows only — a host officer's instance is its mirrored
//     window) and the mirror hub's hosts, in memory. Each window is keyed as
//     the switcher keys it (ui mirrorCodeName) — and only the fields that key
//     and its href need are read, none of mirrorCards' per-viewer reads (host
//     ranks, pins, focus, the tmux preference).
//   - Tasks, asked about once per batch of primed ids (notes.LiveIDs: one
//     query, Get's visibility under ctx's principal).
//
// An agent its owner turned off is still linked: its page exists.
//
// No agent store, or a failed read (logged), gives the mirrors alone.
func (s *Server) mentionIndex(ctx context.Context) *ui.MentionIndex {
	names := func() ui.MentionNames {
		var n ui.MentionNames
		if s.ext.Agents != nil {
			list, err := s.ext.Agents.List(ctx)
			if err != nil {
				s.logger.Printf("mentions: list agents: %v", err)
			}
			for _, a := range agents.ProvisionedOnly(list) {
				n.Agents = append(n.Agents, a.Name)
			}
		}
		if s.ext.Mirror != nil {
			for _, h := range s.ext.Mirror.Hosts(ctx) {
				for _, e := range h.Entries() {
					n.Sessions = append(n.Sessions, ui.MirrorCardView{Host: e.Host, Session: e.Session, Window: e.Window,
						Title: mirrorTitle(e), Label: e.Label, Claude: e.Claude, AgentView: e.AgentView()})
				}
			}
		}
		return n
	}
	var tasks func([]int64) map[int64]bool
	if s.ext.Notes != nil {
		tasks = func(ids []int64) map[int64]bool {
			got, err := notes.LiveIDs(ctx, s.ext.Notes, ids)
			if err != nil {
				s.logger.Printf("mentions: task ids: %v", err)
			}
			return got
		}
	}
	return ui.NewMentionIndex(names, tasks)
}
