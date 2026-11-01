package plugins

import (
	"strings"
	"testing"
	"time"
)

// TestASessionHintAndAnOnlineAuthorCarryTheirWhere: the chief's hint line and
// an online author's window line carry the window's where (mirror.Where's
// text, passed in), so an ask the chief raises from them can name it.
func TestASessionHintAndAnOnlineAuthorCarryTheirWhere(t *testing.T) {
	got := renderHints(Hints{Window: 6 * time.Hour, Sessions: []SessionHint{{ID: "mirror:0b2c3d4e-a", Name: "ops", Host: "box-1",
		Where: "mango:1 · tmux scratch3:5 @ box-1"}}}, hintNonce, true)
	if !strings.Contains(got, "\n| mirror:0b2c3d4e-a — ops @ box-1 — where: mango:1 · tmux scratch3:5 @ box-1\n") {
		t.Fatalf("hint block lacks the where line:\n%s", got)
	}
	a := &AuthorHint{ID: "mirror:7d1e5b2a-0000-4000-8000-0000000b4a5e", Host: "box-1", Kind: "git-push",
		Repo: "acme-labs/widget-api", HeadRef: "feat/gizmo-route", LastSeen: authorSeen, Online: true, Where: "tmux build:7 @ box-1"}
	if got := renderAuthor(a, hintNonce); !strings.Contains(got, "|   window: online, where: tmux build:7 @ box-1\n") {
		t.Fatalf("author block lacks the where:\n%s", got)
	}
}
