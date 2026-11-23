package api

import (
	"context"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// WHO TYPED EACH LINE OF A TEAM'S SHARED CHAT (team sharing slice 4.4). Several
// members type into a team agent's one conversation, so each typed user line
// is drawn with its typer's name (ui.ChatLine.Typer), read from the row's
// existing typed_by (migration 0081): nothing new is collected. A personal chat
// draws no names at all — its lines are all its owner's.

// chatTyperYou is the label of the viewer's OWN lines in a team chat.
const chatTyperYou = "You"

// teamChat reports whether a chat owned by owner is a TEAM's shared room for
// the request: the viewer is a member of the team that owns it (the browser
// session's team scope, which is the only door that sees a team agent). A
// personal agent's owner is a person, never a team the viewer is in.
func teamChat(ctx context.Context, owner int64) bool {
	p, ok := authz.From(ctx)
	if !ok || owner <= 0 {
		return false
	}
	_, in := p.TeamScope().RoleIn(owner)
	return in
}

// chatTypers sets Typer on every typed user line of lines when the chat (owned
// by owner) is a team's shared room, and on none otherwise. The viewer's own
// lines say "You"; another member's say their display name, or
// chatTyperFallback when there is none or the account cannot be read. One read
// per distinct typer.
func (s *Server) chatTypers(ctx context.Context, owner int64, lines []ui.ChatLine) {
	if !teamChat(ctx, owner) {
		return
	}
	me := int64(0)
	if p, ok := authz.From(ctx); ok {
		me = p.UserID
	}
	names := map[int64]string{}
	for i := range lines {
		l := &lines[i]
		if l.Role != "user" || l.TypedBy <= 0 {
			continue
		}
		if l.TypedBy == me {
			l.Typer = chatTyperYou
			continue
		}
		name, ok := names[l.TypedBy]
		if !ok {
			name = s.chatTyperName(ctx, l.TypedBy)
			names[l.TypedBy] = name
		}
		l.Typer = name
	}
}

// chatTyperName is the name another member's line is labelled with: the
// account's display name, else chatTyperFallback. 🔴 NEVER the email: every
// member of the team reads the label, viewers included.
func (s *Server) chatTyperName(ctx context.Context, uid int64) string {
	if pn, ok := s.ext.Users.(personNamer); ok {
		if u, err := pn.ByID(ctx, uid); err == nil {
			if name := strings.Join(strings.Fields(u.DisplayName), " "); name != "" {
				return name
			}
		}
	}
	return chatTyperFallback(uid)
}

// chatTyperFallback is the label of a typer with no display name, or whose
// account cannot be read (an id with no person row): "user #<id>", as the
// team page's own fallback (personLabel) names a person — distinct per typer,
// so two unnamed members are still told apart.
func chatTyperFallback(uid int64) string {
	return "user #" + strconv.FormatInt(uid, 10)
}
