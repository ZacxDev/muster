package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheChiefPagesStripIsTheSwitcherOrder: the chief page's strip of chips
// (ui chiefChatStrip) places the chats in agents.SwitcherOrder — the primary
// first, then the side chats by last activity, newest first, then the archived
// ones, newest first — as the server reads them (chiefChatViews), the seam
// between the store's order and the renderer.
//
// The fixture's activity times are pairwise distinct and NOT in id order (the
// oldest side chat is the newest row, an archived chat was active more recently
// than one side chat), so an order by id, by creation or by activity across the
// groups each draws a different strip. The chat the page is on (side chat B) is
// the one chip marked current, and the panel's body draws the same order.
func TestTheChiefPagesStripIsTheSwitcherOrder(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	sys := authz.System(context.Background())
	primary, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	mk := func() agents.ChatSession {
		s, err := f.srv.ext.Agents.CreateSession(ctx, f.chief.ID, f.chief.Name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b, c, oldArch, newArch := mk(), mk(), mk(), mk(), mk()
	cs, _ := f.srv.chiefChatStore()
	for _, s := range []agents.ChatSession{oldArch, newArch} {
		if _, err := cs.SetArchived(ctx, f.chief.ID, s.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	at := map[int64]time.Duration{
		primary.ID: 0, // the oldest of all: the primary is first whatever its age
		a.ID:       50 * time.Minute,
		b.ID:       10 * time.Minute,
		c.ID:       30 * time.Minute, // created last of the three, between a and b
		oldArch.ID: 5 * time.Minute,
		newArch.ID: 40 * time.Minute, // newer than side chat b
	}
	for id, d := range at {
		if _, err := f.pool.Exec(sys, `UPDATE chat_sessions SET updated_at = $2 WHERE id = $1`, id, base.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	want := []int64{primary.ID, a.ID, c.ID, b.ID, newArch.ID, oldArch.ID}

	chips := func(html string) []int64 {
		var out []int64
		for _, m := range regexp.MustCompile(`<li data-chief-chip="(\d+)"`).FindAllStringSubmatch(html, -1) {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			out = append(out, id)
		}
		return out
	}
	same := func(got []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	page := f.call(t, http.MethodGet, ui.AgentSessionHref(f.chief.Name, b.ID), nil, f.operator())
	if page.Code != http.StatusOK {
		t.Fatalf("GET the chief page: %d", page.Code)
	}
	html := page.Body.String()
	if got := chips(html); !same(got) {
		t.Errorf("the page's strip is %v, want %v (primary, side by activity, archived by activity)", got, want)
	}
	if n := strings.Count(html, `aria-current="true" class=`); n != 1 ||
		!regexp.MustCompile(`data-chief-chat-row="`+strconv.FormatInt(b.ID, 10)+`" title="[^"]*" aria-current="true"`).MatchString(html) {
		t.Errorf("the page's current chip is not chat %d alone (%d current)", b.ID, n)
	}
	if !strings.Contains(html, `>Archived (2)<`) {
		t.Error("the page's strip does not say Archived (2)")
	}

	panel := f.call(t, http.MethodGet, "/ui/chief/panel?session="+strconv.FormatInt(b.ID, 10), nil, f.operator())
	if panel.Code != http.StatusOK {
		t.Fatalf("GET the chief panel: %d", panel.Code)
	}
	if got := chips(panel.Body.String()); !same(got) {
		t.Errorf("the panel's strip is %v, want %v", got, want)
	}
}
