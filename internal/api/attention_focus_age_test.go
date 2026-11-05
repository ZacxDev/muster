package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
)

// TestTheAsksListShowsAFocusTitlesAge: an Asks list row's focus title (set by
// withFocus from the session's stored focus) says how old it is — " · 2d"
// past a day — and one older than four days is marked stale and drawn muted.
// The three sessions' focuses are 2, 5 and 0 days old, distinct from each
// other and from both thresholds. CONTROL: the fresh one carries no age and no
// stale mark, and its title's own element still holds exactly its title.
func TestTheAsksListShowsAFocusTitlesAge(t *testing.T) {
	now := time.Now()
	sids := map[string]struct {
		title string
		age   time.Duration
	}{
		"0a1b2c3d-0000-4000-8000-00000000aaaa": {"Weighing the plover samples", 50 * time.Hour},
		"0a1b2c3d-0000-4000-8000-00000000bbbb": {"Charting the heron migration", 5*24*time.Hour + 3*time.Hour},
		"0a1b2c3d-0000-4000-8000-00000000cccc": {"Counting the tern nests", 2 * time.Hour},
	}
	rows := map[focus.Subject]focus.Focus{}
	var items []attentionItem
	for sid, f := range sids {
		sub := focus.MirrorSubject(sid)
		rows[sub] = focus.Focus{Subject: sub, Owner: 1, Title: f.title, Source: focus.SourceSummarizer, UpdatedAt: now.Add(-f.age)}
		items = append(items, attentionItem{Key: "k-" + sid[len(sid)-4:], Kind: attentionKindSession, Text: "waiting", Since: now.Add(-time.Minute), claudeSession: sid})
	}
	// Two chief NOTICES: each carries its session's focus title as the
	// decision snapshotted it. One snapshot is still the stored title (5 days
	// old: it gets that age, stale); the other's session has a newer title
	// since (no age: the stored time is the newer title's).
	same, moved := focus.MirrorSubject("0a1b2c3d-0000-4000-8000-00000000dddd"), focus.MirrorSubject("0a1b2c3d-0000-4000-8000-00000000eeee")
	rows[same] = focus.Focus{Subject: same, Owner: 1, Title: "Banding the osprey chicks", Source: focus.SourceSummarizer, UpdatedAt: now.Add(-5*24*time.Hour - 7*time.Hour)}
	// The newer title is itself 3 days old, so an age copied from it would show.
	rows[moved] = focus.Focus{Subject: moved, Owner: 1, Title: "A newer title", Source: focus.SourceSummarizer, UpdatedAt: now.Add(-3*24*time.Hour - 5*time.Hour)}
	items = append(items,
		attentionItem{Key: "k-dddd", Kind: attentionKindNotice, Text: "nudge", Since: now.Add(-time.Minute), Focus: "Banding the osprey chicks", subject: same},
		attentionItem{Key: "k-eeee", Kind: attentionKindNotice, Text: "nudge", Since: now.Add(-time.Minute), Focus: "Ringing the gull colony", subject: moved})
	s := &Server{}
	s.ext.Focus = &memFocus{rows: rows}
	s.withFocus(context.Background(), items)
	page := attentionItemsHTML(items, now)
	row := func(key string) string {
		i := strings.Index(page, `id="`+key+`"`)
		if i < 0 {
			t.Fatalf("no row %s in:\n%s", key, page)
		}
		j := strings.Index(page[i:], "</li>")
		return page[i : i+j]
	}
	plover, heron, tern := row("k-aaaa"), row("k-bbbb"), row("k-cccc")
	osprey, gull := row("k-dddd"), row("k-eeee")
	for _, c := range []struct{ name, row, want string }{
		{"two days", plover, `<span data-attention-focus>Weighing the plover samples</span><span class="font-normal" data-focus-age title="focus title set 2d ago" aria-label="focus title set 2d ago"> · 2d</span>`},
		{"five days", heron, `<span data-attention-focus>Charting the heron migration</span><span class="font-normal" data-focus-age title="focus title set 5d ago (stale)" aria-label="focus title set 5d ago (stale)"> · 5d</span>`},
		{"five days, stale", heron, `text-fg2 italic opacity-60" data-focus-stale>`},
		{"fresh", tern, `<span data-attention-focus>Counting the tern nests</span></span>`},
		{"a notice, five days", osprey, `<span data-attention-focus>Banding the osprey chicks</span><span class="font-normal" data-focus-age`},
		{"a notice, stale", osprey, `text-fg2 italic opacity-60" data-focus-stale>`},
		{"a notice whose session moved on", gull, `<span data-attention-focus>Ringing the gull colony</span></span>`},
	} {
		if !strings.Contains(c.row, c.want) {
			t.Errorf("%s: the row lacks\n%s\nin:\n%s", c.name, c.want, c.row)
		}
	}
	if strings.Contains(plover, "data-focus-stale") || strings.Contains(plover, "opacity-60") {
		t.Errorf("a two-day-old focus is marked stale:\n%s", plover)
	}
	if strings.Contains(tern, "data-focus-age") || strings.Contains(tern, "data-focus-stale") {
		t.Errorf("a two-hour-old focus carries an age:\n%s", tern)
	}
}

// TestANoticeOnlyListGetsItsFocusAge: a list of chief notices alone — no
// mirrored session row to make the focus index be read — still gives each its
// focus title's age. Red at 80a44d4a.
func TestANoticeOnlyListGetsItsFocusAge(t *testing.T) {
	now := time.Now()
	sub := focus.MirrorSubject("0a1b2c3d-0000-4000-8000-00000000ffff")
	s := &Server{}
	s.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{
		sub: {Subject: sub, Owner: 1, Title: "Banding the osprey chicks", Source: focus.SourceSummarizer, UpdatedAt: now.Add(-50 * time.Hour)},
	}}
	items := []attentionItem{{Key: "k-ffff", Kind: attentionKindNotice, Text: "nudge", Since: now.Add(-time.Minute), Focus: "Banding the osprey chicks", subject: sub}}
	s.withFocus(context.Background(), items)
	if page := attentionItemsHTML(items, now); !strings.Contains(page, `aria-label="focus title set 2d ago"> · 2d</span>`) {
		t.Errorf("a notice-only list drew no focus age:\n%s", page)
	}
}
