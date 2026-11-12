package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE EPISODE PUSH AND ITS ROW NAME THE WINDOW BY ITS LABEL, RESOLVED WHEN
// SENT OR DRAWN (session_push.go episodeWho), end to end on a real Postgres.
// The fixture's window is labelled "Gold:1" by its host. Only names that exist
// at the base too, so each is shown red there.

// renumber moves the fixture's window to index i, its host's label with it.
func (f *stateFixture) renumber(i int, label string) {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.windows[0].Index = i
	f.mirror.windows[0].Label = label
}

// TestAHeldStopRenumberedBeforeItsPushNamesWhereItIsNow (regression): a stop
// the chief judges is held while it decides; the window is renumbered (tmux
// renumber-windows) before the push goes. The push names the window where it
// is when SENT — its new index and its host's label for it — never the index
// stored when the stop opened, nor the raw tmux session alone.
func TestAHeldStopRenumberedBeforeItsPushNamesWhereItIsNow(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("pushed while the chief decides: %v", got)
	}
	if e := f.oneEpisode(t); e.Index != 1 || e.TmuxSession != "main" {
		t.Fatalf("the episode stored %s:%d", e.TmuxSession, e.Index)
	}
	f.renumber(5, "Gold:5")
	f.advance(episodeChiefWait + time.Minute)
	f.status(mirror.StatusIdle, 30*time.Minute)
	f.statePass(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
	if p := f.payloads()[0]; p.Title != "Gold:5 · tmux main:5 @ ws is stalled" {
		t.Fatalf("title %q", p.Title)
	}
}

// TestAStalledRowNamesItsWindowByItsLabelNow (regression): the Asks tab's
// "Stalled / waiting" row names the window as its push does — its label, then
// its tmux target and host — resolved when drawn, so a window renumbered since
// the stop opened shows where it is now.
func TestAStalledRowNamesItsWindowByItsLabelNow(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	f.oneEpisode(t)
	page := f.page(t, "/attention")
	if !strings.Contains(page, `data-attention-episode-where>Gold:1 · tmux main:1 @ ws</span>`) {
		t.Fatalf("the row does not name the window by its label:\n%s", episodeRows(page))
	}
	f.renumber(6, "Gold:6")
	page = f.page(t, "/attention")
	if !strings.Contains(page, `data-attention-episode-where>Gold:6 · tmux main:6 @ ws</span>`) {
		t.Fatalf("the row does not name where the window is now:\n%s", episodeRows(page))
	}
	// Its host drops; the hub still lists the window (the grace period): the
	// row says so, as a notice does (entryWhere).
	f.mirror.mu.Lock()
	f.mirror.online = false
	f.mirror.mu.Unlock()
	page = f.page(t, "/attention")
	if !strings.Contains(page, `data-attention-episode-where>Gold:6 · tmux main:6 @ ws (host offline)</span>`) {
		t.Fatalf("the row does not mark its offline host:\n%s", episodeRows(page))
	}
}

// episodeRows is page's "Stalled / waiting" section, for a failure message.
func episodeRows(page string) string {
	i := strings.Index(page, `data-testid="attention-episodes"`)
	if i < 0 {
		return "(no Stalled / waiting section)"
	}
	return page[i:min(len(page), i+1500)]
}

// TestAStalledRowWhoseWindowMovedOnKeepsItsCodename (regression): the window
// that held the stop's conversation now runs another one (a /clear gave it a
// new Claude session id), so its key no longer resolves; the row still names
// the stored place by its host's codename ("Gold:1"), never the bare target.
func TestAStalledRowWhoseWindowMovedOnKeepsItsCodename(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	f.oneEpisode(t)
	f.mirror.mu.Lock()
	f.mirror.windows[0].ClaudeSession = "5d4c3b2a-1908-4f7e-8d6c-5b4a39281706"
	f.mirror.mu.Unlock()
	page := f.page(t, "/attention")
	if !strings.Contains(page, `data-attention-episode-where>Gold:1 · tmux main:1 @ ws</span>`) {
		t.Fatalf("the row lost its codename on the fallback:\n%s", episodeRows(page))
	}
}
